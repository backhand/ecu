"""FastAPI tool server for an ECU computer-use instance.

This wraps the *existing* tool implementations shipped in the anthropic
computer-use-demo image (``computer_use_demo.tools``) and exposes them over
HTTP. We deliberately do NOT reimplement the input/action logic: every
GUI action is forwarded to the demo's ``ComputerTool`` (xdotool/scrot under
the hood). Only ``/exec`` and the ``/screenshot`` framing add local logic.

Component 1 of ECU. The screenshot diff protocol (``nochange``/``diff``) is
Component 6: ``/screenshot`` keeps the last frame (encoded bytes + decoded
pixels) and a monotonic ``frame_token``, and against the caller's ``since``
token it returns ``nochange`` (pixel-identical), ``diff`` (only changed tile
regions), or ``full`` (first frame / forced / since mismatch / diff-bigger-
than-full). The client composites diff regions onto its cached base to
reconstruct the full frame; the model never sees a diff (see skill/ecu_client.py).

Wire efficiency (protocol #1+#2): the captured frame is downscaled to the
caller's ``max_width`` and lossy-encoded (JPEG/WebP, PNG escape hatch) *at the
source*, so the wire carries ~20-60 KB instead of a ~1 MB full-res PNG. The
diff base the server holds is the *downscaled* frame, so region coordinates are
already in the shown space the client composites in. The response reports the
real captured size as ``native_width``/``native_height`` alongside the shown
``width``/``height`` so the client can translate clicks back to native pixels.

Security note: this binds to 0.0.0.0 *inside the container* on purpose. The
"localhost-only" property is enforced by how the instance publishes the port
(``docker -p 127.0.0.1:PORT`` + firewall, Component 4), not here.
"""

from __future__ import annotations

import asyncio
import base64
import io
import os
import shlex
import subprocess
import threading
from typing import Literal

import numpy as np
from fastapi import FastAPI
from fastapi.responses import JSONResponse
from PIL import Image
from pydantic import BaseModel, Field

from .watch import register_watch_routes

# Import the *latest* concrete ComputerTool directly from the module. Note the
# demo's tools/__init__.py only re-exports the 20241022/20250124 classes, but
# the 20251124 class (the newest, adds scroll/zoom) lives in tools.computer and
# is wired into tools.groups. We pull it straight from the module so we always
# drive the most capable executor present in the image.
from computer_use_demo.tools.computer import (  # type: ignore[import-not-found]
    ComputerTool20251124 as ComputerTool,
)

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

# Display geometry. The demo's ComputerTool reads WIDTH/HEIGHT/DISPLAY_NUM from
# the environment itself; we read them here only to report dimensions and to
# build the display-readiness probe. Default 1280x800 per the ECU brief (the
# base image ships 1024x768; whatever is set in the env wins).
WIDTH = int(os.getenv("WIDTH") or 1280)
HEIGHT = int(os.getenv("HEIGHT") or 800)
DISPLAY_NUM = os.getenv("DISPLAY_NUM", "1")
DISPLAY = f":{DISPLAY_NUM}"

# noVNC lives on this port in the base image (novnc_startup.sh --listen 6080,
# serving /opt/noVNC, so vnc.html is the client page, plus a /websockify
# WebSocket bridging to the VNC server on :5900). The tool server reverse-proxies
# it under /watch so it is reachable through the single tunnel target (:8000) --
# see watch.py and register_watch_routes below (Component 9).
NOVNC_PORT = int(os.getenv("ECU_NOVNC_PORT") or 6080)

# Diff protocol (Component 6). The dirty-check is a coarse grid: we compare the
# current frame against the base in TILE_SIZE x TILE_SIZE blocks (cheap, no
# per-pixel bbox work), then merge horizontally-adjacent changed tiles in each
# tile-row into rectangles. 64px keeps the tile count low and the merged
# rectangles tight without dragging large unchanged areas into a crop.
TILE_SIZE = int(os.getenv("ECU_DIFF_TILE") or 64)

# Full-fallback when the change is too big to be worth diffing. Two triggers:
#  * byte rule: the diff region PNGs together weigh >= the full-frame PNG (the
#    brief's primary rule -- never ship a diff bigger than a full frame);
#  * coverage rule: the changed tiles cover >= FULL_COVERAGE of the frame. With
#    a horizontal row-merge a whole-screen change reconstructs the same pixels
#    as ~one full frame at near-identical size, so the byte rule sits right on
#    the boundary; the coverage rule makes the brief's stated "a whole-screen
#    change / page transition falls back to full" deterministic. Either trips it.
FULL_COVERAGE = float(os.getenv("ECU_DIFF_FULL_COVERAGE") or 0.9)

# Wire-encoding defaults (protocol #1+#2). The full frame and every diff region
# are downscaled to ``max_width`` and lossy-encoded before they hit the wire.
#  * DEFAULT_MAX_WIDTH = WIDTH means "no downscale by default" (a no-op): callers
#    opt into a smaller frame by passing a smaller max_width.
#  * jpeg is the universal default; webp compresses ~25-35% better where the
#    decoder supports it; png is a crisp-text escape hatch (lossless, larger).
#  * quality ~75 is a good legibility/size tradeoff for UI screenshots.
DEFAULT_MAX_WIDTH = int(os.getenv("ECU_SHOT_MAX_WIDTH") or WIDTH)
DEFAULT_FORMAT = (os.getenv("ECU_SHOT_FORMAT") or "jpeg").lower()
DEFAULT_QUALITY = int(os.getenv("ECU_SHOT_QUALITY") or 75)

# Map a request ``format`` to (Pillow format, response content tag). PNG is
# lossless and ignores ``quality``; jpeg/webp are lossy.
_FORMATS: dict[str, str] = {"jpeg": "JPEG", "webp": "WEBP", "png": "PNG"}

app = FastAPI(title="ECU tool server", version="1")


# ---------------------------------------------------------------------------
# Tool instance + frame state
# ---------------------------------------------------------------------------

# Instantiate the computer tool once. ComputerTool.__init__ asserts WIDTH and
# HEIGHT are set in the environment, so the entrypoint must export them.
#
# We disable the demo's coordinate/image scaling so the contract is 1:1:
# client coordinates map directly to screen pixels and the returned PNG is at
# full WIDTH x HEIGHT. (With scaling enabled, non-standard resolutions get
# silently rescaled to XGA/WXGA/FWXGA and coordinates remapped, which would
# make the reported width/height disagree with the pixels.)
_computer = ComputerTool()
_computer._scaling_enabled = False

# --- input must never block on a capture (protocol #4) ---------------------
#
# Two demo behaviours made a single /click take ~18 s on this headless desktop —
# input that was, in effect, blocking on a capture on every action. Measured in
# the container: xdotool click alone is ~0.15 s, but a /click was ~18 s.
#
#   (1) Post-action capture. Every demo action ends in
#       ``ComputerTool.shell(cmd)`` with the DEFAULT ``take_screenshot=True``,
#       which after the xdotool command does ``await asyncio.sleep(2 s)`` and a
#       FULL ``screenshot()`` the tool server then throws away (we capture frames
#       only on the explicit /screenshot path). ~2 s + a grab per action.
#
#   (2) ``mousemove --sync``. The demo builds click/move as
#       ``xdotool mousemove --sync X Y ...``. ``--sync`` blocks until the X
#       server acks the pointer reaching the target; under this Xvfb + mutter it
#       hangs ~16 s PER action (the dominant cost — measured: with ``--sync``
#       ~16.5 s, without it ~0.15 s, identical otherwise).
#
# Both are baked into the demo's ``__call__``/``shell``, so we drive xdotool
# OURSELVES for the input endpoints: build the same commands but WITHOUT
# ``--sync`` and WITHOUT a post-action capture, and run them through the demo's
# async ``shell`` (``asyncio.create_subprocess_shell`` -> non-blocking). Input
# handlers then cost only the xdotool subprocess (~tens of ms) and hold no lock
# the capture path needs, so they stay sub-100 ms even during a screenshot storm.
#
# The /screenshot path is unchanged: it captures via ``screenshot()`` directly
# (``_capture_png`` -> ``action="screenshot"``), which we keep going through the
# demo tool. We also force ``shell`` to never auto-capture and zero the settle
# delay as belt-and-braces for any demo path that still routes through ``shell``
# (e.g. ``type``'s trailing screenshot is suppressed; the chunked type shells
# already passed ``take_screenshot=False``).
_computer._screenshot_delay = 0

_demo_shell = _computer.shell


async def _shell_no_capture(command: str, take_screenshot: bool = True):
    # Force take_screenshot=False regardless of the caller: no action needs the
    # demo's post-command sleep+screenshot, and the tool server captures frames
    # only on the explicit /screenshot path.
    return await _demo_shell(command, take_screenshot=False)


_computer.shell = _shell_no_capture  # type: ignore[method-assign]

# xdotool prefix (DISPLAY set), mirrored from the demo tool so our hand-built
# input commands target the same X display.
_XDOTOOL = f"DISPLAY={DISPLAY} xdotool"


async def _xdotool(args: str) -> None:
    """Run an ``xdotool <args>`` command on the desktop, fast and capture-free.

    Goes through the demo's async ``shell`` (now forced to never auto-capture),
    which runs the subprocess via ``asyncio.create_subprocess_shell`` so the
    event loop keeps serving other requests (input + screenshots interleave).
    Deliberately omits ``mousemove --sync`` — the sync ack hangs ~16 s here and
    is unnecessary: a screenshot taken afterwards already reflects the move. We
    surface only hard failures; xdotool's stderr on a successful move is benign.
    """
    result = await _demo_shell(f"{_XDOTOOL} {args}", take_screenshot=False)
    # xdotool is silent on success; any stderr means a real failure (bad key
    # name, display down, ...). Surface it as a tool error (same strictness the
    # demo applied to imageless action results).
    err = (getattr(result, "error", None) or "").strip()
    if err:
        raise ToolActionError(err)


# xdotool button numbers for the pointer buttons we expose.
_XBUTTON = {"left": 1, "middle": 2, "right": 3}
# xdotool wheel-button numbers for scrolling (up/down/left/right).
_XSCROLL = {"up": 4, "down": 5, "left": 6, "right": 7}


# --- per-action executors (shared by the individual endpoints AND /actions) --
#
# The actual input/exec logic lives in these small helpers so the single-action
# endpoints (/click, /move, ...) and the batch /actions endpoint drive the EXACT
# same code path: same hand-built, capture-free, no-`--sync` xdotool commands
# through `_xdotool`, and the same `sh -c` exec runner. There is no second
# implementation of an action to drift — /actions is a thin loop over these.


async def _do_click(x: int, y: int, button: str = "left") -> None:
    """Move-then-click at (x, y) in one capture-free xdotool call (no --sync)."""
    await _xdotool(f"mousemove {x} {y} click {_XBUTTON[button]}")


async def _do_move(x: int, y: int) -> None:
    """Move the pointer to (x, y), capture-free, no --sync."""
    await _xdotool(f"mousemove {x} {y}")


async def _do_type(text: str) -> None:
    """Type ``text`` at the focus with the demo's cadence (--delay 12)."""
    await _xdotool(f"type --delay 12 -- {shlex.quote(text)}")


async def _do_key(keys: str) -> None:
    """Press an xdotool key/chord, e.g. ``Return`` / ``ctrl+l`` / ``alt+F4``."""
    await _xdotool(f"key -- {keys}")


async def _do_scroll(x: int, y: int, dx: int = 0, dy: int = 0) -> None:
    """Scroll at (x, y): one wheel-button burst per nonzero axis.

    Builds the SAME single mousemove + ``click --repeat N <wheel>`` bursts the
    ``/scroll`` handler builds (dy>0 down / dy<0 up, dx>0 right / dx<0 left),
    both axes in ONE xdotool call. A zero-zero scroll is a no-op (no xdotool
    call at all) that still counts as a successful action.
    """
    parts = [f"mousemove {x} {y}"]
    if dy:
        parts.append(
            f"click --repeat {abs(dy)} "
            f"{_XSCROLL['down'] if dy > 0 else _XSCROLL['up']}"
        )
    if dx:
        parts.append(
            f"click --repeat {abs(dx)} "
            f"{_XSCROLL['right'] if dx > 0 else _XSCROLL['left']}"
        )
    if len(parts) == 1:
        return  # no-op scroll (dx==dy==0): nothing to do
    await _xdotool(" ".join(parts))


def _exec_run(command: str, timeout: float | None) -> dict:
    """Run ``sh -c <command>`` (blocking) and return {stdout, stderr, code}.

    The single source of truth for /exec semantics: a one-shot ``sh -c`` (no
    persistent shell), with a timeout (default handled by the caller / model)
    that maps a TimeoutExpired to ``code == 124`` and an appended note on
    stderr. Both the /exec endpoint and the batch ``exec`` action call this via
    ``asyncio.to_thread`` so the blocking subprocess never stalls the loop.
    """
    try:
        proc = subprocess.run(
            ["sh", "-c", command],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            timeout=timeout,
        )
        return {
            "stdout": proc.stdout.decode(errors="replace"),
            "stderr": proc.stderr.decode(errors="replace"),
            "code": proc.returncode,
        }
    except subprocess.TimeoutExpired as exc:
        out = exc.stdout.decode(errors="replace") if exc.stdout else ""
        err = exc.stderr.decode(errors="replace") if exc.stderr else ""
        return {
            "stdout": out,
            "stderr": err + f"\ncommand timed out after {timeout}s",
            "code": 124,
        }


class _FrameState:
    """The instance's single base frame: encoded bytes, decoded pixels, token.

    Single-tenant per instance, so this is the one frame the brief refers to
    ("per session = the one frame"). The base now holds the *downscaled* frame:
    ``pixels`` (an HxWx3 uint8 numpy array in the shown space) is what the diff
    tiling runs against, and ``base_width`` records the shown width so a caller
    asking for a *different* ``max_width`` invalidates the base (different base
    size -> we must return a full frame). The diff protocol advances ``token``
    only when the base actually moves (full/diff), never on ``nochange``. All
    access is lock-guarded.
    """

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._token = 0
        self._image: bytes | None = None
        self._pixels: np.ndarray | None = None
        self._base_width: int = 0

    def snapshot(self) -> tuple[int, bytes | None, np.ndarray | None, int]:
        """Atomically read the current base (token, image, pixels, base_width)."""
        with self._lock:
            return self._token, self._image, self._pixels, self._base_width

    def set(self, image: bytes, pixels: np.ndarray) -> int:
        """Replace the base with a new (downscaled) frame, advance the token."""
        with self._lock:
            self._token += 1
            self._image = image
            self._pixels = pixels
            self._base_width = int(pixels.shape[1])
            return self._token

    @property
    def last_image(self) -> bytes | None:
        with self._lock:
            return self._image

    @property
    def token(self) -> int:
        with self._lock:
            return self._token


_frames = _FrameState()


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


class ToolActionError(Exception):
    """Raised when an input action or capture reports an error."""


async def _capture_png() -> bytes:
    """Take a screenshot via the demo tool and return raw PNG bytes.

    The demo's ``ComputerTool.screenshot()`` spawns the capture (gnome-screenshot
    / scrot) as an *async* subprocess (``tools/run.py`` uses
    ``asyncio.create_subprocess_shell`` + ``await communicate()``), so the actual
    grab already yields the event loop. The one synchronous slice it still runs
    on the loop is reading the ~1 MB PNG back off disk (``path.read_bytes()``).
    That is cheap for one capture but multiplies under concurrent screenshot
    load; the SCREENSHOT-ONLY capture gate (see ``_CaptureGate``) bounds it by
    collapsing concurrent screenshot requests onto a single in-flight grab.
    Input handlers never go through here, so they never pay for capture.
    """
    result = await _computer(action="screenshot")
    b64 = getattr(result, "base64_image", None)
    if not b64:
        raise ToolActionError(getattr(result, "error", None) or "screenshot failed")
    return base64.b64decode(b64)


class _CaptureGate:
    """Single-flight gate for SCREENSHOT capture only (protocol #4).

    The single-tenant desktop has exactly one framebuffer, so launching N
    simultaneous ``scrot`` processes for N concurrent ``/screenshot`` requests is
    pure waste — and, worse, each capture's synchronous tail (reading the PNG off
    disk) runs on the event loop, so N of them pile up and can starve the loop
    that input handlers also need. This was the live failure: a ``/click`` queued
    behind a burst of in-flight screenshots and timed out at 60 s.

    The gate coalesces concurrent capture requests onto a single in-flight grab:
    the first caller launches the capture; everyone who asks while it is running
    awaits that SAME capture and gets its bytes (the freshly captured frame).
    Once it resolves the gate is clear and the next request starts a new grab —
    so a screenshot is never served stale beyond one coalesced batch.

    Crucially this lock lives ONLY on the ``/screenshot`` path. Input handlers
    (``/click`` etc.) acquire NOTHING here, so they are never blocked by a
    capture: input stays sub-100 ms even under a screenshot storm.
    """

    def __init__(self) -> None:
        self._lock = asyncio.Lock()
        self._inflight: "asyncio.Future[bytes] | None" = None

    async def capture(self) -> bytes:
        # Fast path: a capture is already running — await its result instead of
        # launching a second scrot. We snapshot the future under the lock, then
        # await it OUTSIDE the lock so a waiter never holds the lock (which would
        # serialize the next batch behind this one).
        async with self._lock:
            inflight = self._inflight
            if inflight is None:
                inflight = asyncio.ensure_future(_capture_png())
                self._inflight = inflight
                leader = True
            else:
                leader = False
        try:
            return await inflight
        finally:
            if leader:
                # Clear the slot so the next request triggers a fresh capture.
                async with self._lock:
                    if self._inflight is inflight:
                        self._inflight = None


_capture_gate = _CaptureGate()


# --- diff protocol helpers (Component 6) -----------------------------------


def _decode_rgb(png: bytes) -> np.ndarray:
    """Decode an encoded image (any Pillow format) to an HxWx3 uint8 RGB array.

    RGB (not RGBA) so the per-tile comparison and the region crops match what
    the client reconstructs with (``Image.open(...).convert("RGB")``).
    """
    im = Image.open(io.BytesIO(png)).convert("RGB")
    return np.asarray(im, dtype=np.uint8)


def _normalize_encoding(
    fmt: str | None, quality: int | None, max_width: int | None
) -> tuple[str, int, int]:
    """Clamp/resolve the request's (format, quality, max_width) to safe values.

    Unknown/absent format -> the server default; quality clamped to 1..100;
    max_width to at least 1 (a missing max_width means "native", i.e. WIDTH).
    """
    pil_fmt = _FORMATS.get((fmt or DEFAULT_FORMAT).lower(), _FORMATS[DEFAULT_FORMAT])
    q = DEFAULT_QUALITY if quality is None else int(quality)
    q = max(1, min(100, q))
    mw = DEFAULT_MAX_WIDTH if max_width is None else int(max_width)
    mw = max(1, mw)
    return pil_fmt, q, mw


def _encode(im: Image.Image, pil_fmt: str, quality: int) -> bytes:
    """Encode a PIL RGB image in ``pil_fmt`` (PNG lossless; jpeg/webp lossy)."""
    buf = io.BytesIO()
    if pil_fmt == "PNG":
        im.save(buf, format="PNG")
    else:
        # optimize=False keeps JPEG encoding cheap; quality drives size/fidelity.
        im.save(buf, format=pil_fmt, quality=quality)
    return buf.getvalue()


def _encode_pixels(pixels: np.ndarray, pil_fmt: str, quality: int) -> bytes:
    """Encode a full HxWx3 RGB pixel array in the chosen wire format."""
    return _encode(Image.fromarray(pixels, mode="RGB"), pil_fmt, quality)


def _encode_crop(
    pixels: np.ndarray, x: int, y: int, w: int, h: int, pil_fmt: str, quality: int
) -> bytes:
    """Crop (x,y,w,h) out of an RGB pixel array and encode it in ``pil_fmt``."""
    crop = pixels[y : y + h, x : x + w]
    return _encode(Image.fromarray(crop, mode="RGB"), pil_fmt, quality)


def _downscale_to_width(pixels: np.ndarray, max_width: int) -> np.ndarray:
    """Downscale an RGB pixel array so its width is ``max_width`` (LANCZOS).

    A no-op when the frame is already <= max_width (never upscales). Height is
    scaled proportionally and clamped to >= 1. Returns a fresh contiguous array
    in the shown space; all diffing/cropping then happens at this size.
    """
    h, w = pixels.shape[:2]
    if w <= max_width:
        return pixels
    new_h = max(1, round(h * (max_width / w)))
    im = Image.fromarray(pixels, mode="RGB").resize((max_width, new_h), Image.LANCZOS)
    return np.asarray(im, dtype=np.uint8)


def _changed_tile_grid(base: np.ndarray, cur: np.ndarray, tile: int) -> np.ndarray:
    """Return a boolean grid (tiles_y x tiles_x) marking changed tiles.

    Compares the two frames block-by-block on a ``tile``-px grid. Edge tiles on
    the right/bottom may be smaller than ``tile`` when W/H aren't multiples of
    it; we slice to the frame bounds so every pixel falls in exactly one tile.
    """
    h, w = cur.shape[:2]
    tiles_y = (h + tile - 1) // tile
    tiles_x = (w + tile - 1) // tile
    grid = np.zeros((tiles_y, tiles_x), dtype=bool)
    # Per-pixel inequality once, then reduce per tile block. `diff` is HxW bool
    # (any channel differing); cheap relative to PNG decode/encode.
    diff = np.any(base != cur, axis=2)
    for ty in range(tiles_y):
        y0 = ty * tile
        y1 = min(y0 + tile, h)
        row = diff[y0:y1]
        for tx in range(tiles_x):
            x0 = tx * tile
            x1 = min(x0 + tile, w)
            if row[:, x0:x1].any():
                grid[ty, tx] = True
    return grid


def _merge_tiles_to_rects(
    grid: np.ndarray, tile: int, w: int, h: int
) -> list[tuple[int, int, int, int]]:
    """Merge changed tiles into (x,y,w,h) rectangles, clamped to the frame.

    Simple per-tile-row horizontal run-merge (per the brief): each maximal run
    of changed tiles in a row becomes one rectangle. Adjacent rows are NOT
    merged vertically -- kept deliberately simple; it still covers every
    changed pixel, which is what reconstruction correctness requires.
    """
    rects: list[tuple[int, int, int, int]] = []
    tiles_y, tiles_x = grid.shape
    for ty in range(tiles_y):
        tx = 0
        while tx < tiles_x:
            if not grid[ty, tx]:
                tx += 1
                continue
            run_start = tx
            while tx < tiles_x and grid[ty, tx]:
                tx += 1
            run_end = tx  # exclusive
            x = run_start * tile
            y = ty * tile
            rw = min(run_end * tile, w) - x
            rh = min((ty + 1) * tile, h) - y
            rects.append((x, y, rw, rh))
    return rects


def _display_ready() -> bool:
    """Cheap probe that the X display is up (so screenshots won't fail)."""
    try:
        proc = subprocess.run(
            ["xdpyinfo"],
            env={**os.environ, "DISPLAY": DISPLAY},
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            timeout=5,
        )
        return proc.returncode == 0
    except Exception:
        return False


def _ok() -> JSONResponse:
    return JSONResponse({"ok": True})


def _err(message: str, status: int = 500) -> JSONResponse:
    return JSONResponse({"ok": False, "error": message}, status_code=status)


# ---------------------------------------------------------------------------
# Request models
# ---------------------------------------------------------------------------


class ScreenshotBody(BaseModel):
    since: int | None = None
    mode: Literal["full", "auto"] | None = None
    # Wire-encoding controls (protocol #1+#2). All optional; omitted -> server
    # defaults (max_width=native, jpeg, q75 -> a no-op-width lossy frame).
    max_width: int | None = Field(default=None, gt=0)
    format: Literal["jpeg", "webp", "png"] | None = None
    quality: int | None = Field(default=None, ge=1, le=100)


class ClickBody(BaseModel):
    x: int
    y: int
    button: Literal["left", "right", "middle"] = "left"


class MoveBody(BaseModel):
    x: int
    y: int


class TypeBody(BaseModel):
    text: str


class KeyBody(BaseModel):
    keys: str


class ScrollBody(BaseModel):
    x: int
    y: int
    dx: int = 0
    dy: int = 0


class ExecBody(BaseModel):
    command: str
    timeout: float | None = Field(default=120.0, gt=0)


class ActionsBody(BaseModel):
    """Batch/macro request: an ordered list of actions + an optional trailing
    screenshot, executed server-side in ONE exchange (protocol #5).

    ``actions`` is deliberately a list of *plain dicts* (not a typed union) so
    that an unknown ``action`` value or a missing per-action field becomes a
    RECORDED per-action error that stops the batch (see ``/actions``), rather
    than a FastAPI 422 that rejects the whole request before any action runs.
    Each item is ``{"action": "<click|move|type|key|scroll|exec>", ...}`` with
    the SAME fields as the matching single-action endpoint.

    ``screenshot`` mirrors ``ScreenshotBody`` (``since``/``mode``/``max_width``/
    ``format``/``quality``); it is validated leniently here (the sub-fields are
    resolved/clamped by ``_normalize_encoding`` when the capture runs) and is
    only honored when every action succeeded.
    """

    actions: list[dict]
    screenshot: dict | None = None


# ---------------------------------------------------------------------------
# Endpoints
# ---------------------------------------------------------------------------


@app.get("/healthz")
async def healthz() -> JSONResponse:
    """200 + {ok:true} once the X display is reachable.

    Returns 503 while the desktop is still coming up so callers (and the
    instance agent) can wait for a green light before issuing screenshots.
    """
    ready = await asyncio.to_thread(_display_ready)
    if not ready:
        return JSONResponse(
            {"ok": False, "error": "display not ready"}, status_code=503
        )
    return JSONResponse({"ok": True})


def _full_response(
    pixels: np.ndarray,
    native_w: int,
    native_h: int,
    pil_fmt: str,
    quality: int,
) -> dict:
    """Encode ``pixels`` (the downscaled, shown-space frame), set it as the new
    base, and build a ``full`` payload.

    ``pixels`` is what the client composites against, so the base we hold is the
    downscaled frame. We additionally report the *native* capture size so the
    client can scale model (shown-space) coordinates back to native pixels.
    """
    image = _encode_pixels(pixels, pil_fmt, quality)
    token = _frames.set(image, pixels)
    h, w = pixels.shape[:2]
    return {
        "mode": "full",
        "frame_token": token,
        "image": base64.b64encode(image).decode(),
        "width": w,
        "height": h,
        "native_width": native_w,
        "native_height": native_h,
    }


def _diff_or_full(
    pixels: np.ndarray,
    base_token: int,
    base_pixels: np.ndarray,
    native_w: int,
    native_h: int,
    pil_fmt: str,
    quality: int,
) -> dict:
    """Compute the diff response shape against the held base (sync/CPU-bound).

    ``pixels`` and ``base_pixels`` are both in the *shown* (downscaled) space.
    Returns ``nochange`` (no tile changed -- base/token untouched), ``diff``
    (changed tiles merged into lossy region images), or ``full`` (the diff
    would be >= a full frame, so sending it would be a pessimization). Runs off
    the event loop via ``asyncio.to_thread``.
    """
    h, w = pixels.shape[:2]

    # Shape mismatch (resolution / max_width change) -> can't tile-diff; full.
    if base_pixels.shape != pixels.shape:
        return _full_response(pixels, native_w, native_h, pil_fmt, quality)

    grid = _changed_tile_grid(base_pixels, pixels, TILE_SIZE)
    changed = int(grid.sum())
    if changed == 0:
        # Pixel-identical: keep the same token, don't move the base.
        return {"mode": "nochange", "frame_token": base_token}

    # Coverage full-fallback: a (near-)whole-screen change can't be cheaper as a
    # diff than as one full frame, so don't bother cropping/encoding it.
    if changed >= FULL_COVERAGE * grid.size:
        return _full_response(pixels, native_w, native_h, pil_fmt, quality)

    rects = _merge_tiles_to_rects(grid, TILE_SIZE, w, h)
    regions = []
    region_bytes = 0
    for (x, y, rw, rh) in rects:
        rimg = _encode_crop(pixels, x, y, rw, rh, pil_fmt, quality)
        region_bytes += len(rimg)
        regions.append(
            {"x": x, "y": y, "w": rw, "h": rh, "image": base64.b64encode(rimg).decode()}
        )

    # Byte full-fallback: never ship a diff that's >= one full frame on the wire.
    # Compared on the *lossy* sizes (both region crops and the full are encoded
    # with the same format/quality), so the rule stays apples-to-apples.
    full_image = _encode_pixels(pixels, pil_fmt, quality)
    if region_bytes >= len(full_image):
        token = _frames.set(full_image, pixels)
        return {
            "mode": "full",
            "frame_token": token,
            "image": base64.b64encode(full_image).decode(),
            "width": w,
            "height": h,
            "native_width": native_w,
            "native_height": native_h,
        }

    new_token = _frames.set(full_image, pixels)
    return {
        "mode": "diff",
        "frame_token": new_token,
        "base_token": base_token,
        "width": w,
        "height": h,
        "native_width": native_w,
        "native_height": native_h,
        "regions": regions,
    }


async def _capture_screenshot(
    since: int | None,
    mode: str | None,
    fmt: str | None,
    quality: int | None,
    max_width: int | None,
) -> dict:
    """Capture a frame and resolve it to a ``full``/``diff``/``nochange`` dict.

    The single source of truth for the screenshot protocol: the capture-gate
    grab, the downscale+decode off the loop, and the same full/diff/nochange
    decision tree. Both ``POST /screenshot`` and the trailing screenshot of
    ``POST /actions`` go through here so they produce byte-for-byte identical
    response shapes. Raises ``ToolActionError`` if the underlying capture fails
    (callers map that to an error response). ``mode``/``since`` mirror
    ``ScreenshotBody``; ``fmt``/``quality``/``max_width`` are resolved via
    ``_normalize_encoding`` (which clamps), exactly as ``/screenshot`` does.
    """
    # Capture through the single-flight gate so concurrent capture requests
    # collapse onto one scrot (and never starve the loop that input handlers
    # need). Input endpoints do NOT use the gate.
    png = await _capture_gate.capture()

    pil_fmt, q, mw = _normalize_encoding(fmt, quality, max_width)

    def _prepare() -> tuple[np.ndarray, int, int]:
        native = _decode_rgb(png)
        native_h, native_w = native.shape[:2]
        shown = _downscale_to_width(native, mw)
        return shown, native_w, native_h

    pixels, native_w, native_h = await asyncio.to_thread(_prepare)
    base_token, _base_image, base_pixels, base_width = _frames.snapshot()

    # Forced full / first frame / caller's base doesn't match ours, or the
    # requested max_width yields a different shown width than the held base (a
    # different base size can't be tile-diffed) -> full.
    if (
        mode == "full"
        or base_pixels is None
        or since != base_token
        or base_width != pixels.shape[1]
    ):
        return await asyncio.to_thread(
            _full_response, pixels, native_w, native_h, pil_fmt, q
        )

    return await asyncio.to_thread(
        _diff_or_full,
        pixels,
        base_token,
        base_pixels,
        native_w,
        native_h,
        pil_fmt,
        q,
    )


@app.post("/screenshot")
async def screenshot(body: ScreenshotBody) -> JSONResponse:
    """Capture a frame, downscale + lossy-encode it, and return it as ``full``,
    ``diff``, or ``nochange``.

    The captured frame is first downscaled to ``max_width`` (default: native, a
    no-op) so the held base and every region crop are in the *shown* space the
    client composites in; then encoded in ``format`` at ``quality``. The
    response carries both the shown ``width``/``height`` and the real
    ``native_width``/``native_height`` so the client can scale coordinates.

    Decision tree (single base frame per instance, see ``_FrameState``):
      * ``mode=="full"``, no base yet, ``since`` != the current base token, or a
        ``max_width`` that differs from the held base's width (different base
        size invalidates the diff) -> return ``full`` and set the new base.
      * else compare current vs base per 64px tile (in shown space):
          - no tile changed            -> ``nochange`` (token unchanged)
          - tiles changed              -> ``diff`` of merged region images
          - >=90% of tiles changed, or
            diff bytes >= full bytes   -> ``full`` (don't ship an oversized diff)
    The client composites diff regions onto its cached base to reconstruct the
    complete frame before showing it to a model (it never sees a diff).
    """
    try:
        payload = await _capture_screenshot(
            body.since, body.mode, body.format, body.quality, body.max_width
        )
    except ToolActionError as exc:
        return _err(str(exc))
    return JSONResponse(payload)


@app.post("/click")
async def click(body: ClickBody) -> JSONResponse:
    # Move-then-click in one xdotool invocation, WITHOUT --sync (which hangs ~16 s
    # on this WM) and WITHOUT the demo's post-action capture. ~0.15 s vs ~18 s.
    try:
        await _do_click(body.x, body.y, body.button)
    except ToolActionError as exc:
        return _err(str(exc))
    return _ok()


@app.post("/move")
async def move(body: MoveBody) -> JSONResponse:
    try:
        await _do_move(body.x, body.y)
    except ToolActionError as exc:
        return _err(str(exc))
    return _ok()


@app.post("/type")
async def type_(body: TypeBody) -> JSONResponse:
    # `xdotool type --delay 12` mirrors the demo's typing cadence, but built
    # directly so it skips the demo's trailing full screenshot (an input action
    # must not capture). The text is shlex-quoted; xdotool handles long strings.
    try:
        await _do_type(body.text)
    except ToolActionError as exc:
        return _err(str(exc))
    return _ok()


@app.post("/key")
async def key(body: KeyBody) -> JSONResponse:
    # `xdotool key -- <keys>`: xdotool chord syntax, e.g. "ctrl+l", "Return",
    # "alt+F4". Direct (no post-action capture); key has no --sync to worry about.
    try:
        await _do_key(body.keys)
    except ToolActionError as exc:
        return _err(str(exc))
    return _ok()


@app.post("/scroll")
async def scroll(body: ScrollBody) -> JSONResponse:
    """Scroll at (x, y): one wheel-button burst per nonzero axis.

    Mirrors the demo's scroll (move to the anchor, then ``click --repeat N
    <wheel-button>``: dy>0 down / dy<0 up, dx>0 right / dx<0 left) but built
    directly so it skips ``mousemove --sync`` (the ~16 s hang) and the demo's
    post-action capture. Both axes go in ONE xdotool call so the move applies to
    both bursts. (Builds the bursts via the shared ``_do_scroll`` executor.)
    """
    try:
        await _do_scroll(body.x, body.y, body.dx, body.dy)
    except ToolActionError as exc:
        return _err(str(exc))
    return _ok()


@app.post("/exec")
async def exec_(body: ExecBody) -> JSONResponse:
    """Run a shell command and return stdout/stderr/exit code.

    Implemented as a plain blocking ``sh -c`` (per the brief): the demo's
    BashTool keeps a persistent session and a sentinel protocol that does not
    surface a real per-command exit code, which is exactly what callers of
    /exec want. Timeouts/backgrounding semantics are intentionally deferred.
    The actual run lives in the shared ``_exec_run`` helper (also used by the
    batch ``exec`` action), driven off the loop via ``asyncio.to_thread``.
    """
    result = await asyncio.to_thread(_exec_run, body.command, body.timeout)
    return JSONResponse(result)


# --- batch / macro endpoint (protocol #5) ----------------------------------


def _require(item: dict, field: str, types: tuple) -> object:
    """Pull a required, correctly-typed field out of a batch action item.

    Raises ``ToolActionError`` (which the batch loop records as the failing
    action's error and then stops on) if the field is missing or the wrong
    type — so a malformed item is a recorded per-action error, NOT a 422 that
    would reject the whole batch before any action ran. ``bool`` is excluded
    from numeric types (``isinstance(True, int)`` is True in Python).
    """
    if field not in item:
        raise ToolActionError(f"missing required field {field!r}")
    val = item[field]
    if isinstance(val, bool) or not isinstance(val, types):
        want = "/".join(t.__name__ for t in types)
        raise ToolActionError(f"field {field!r} must be {want}")
    return val


async def _dispatch_action(item: dict) -> dict:
    """Execute ONE batch action item and return its result dict.

    Reuses the EXACT per-action executors the single-action endpoints use
    (``_do_click``/``_do_move``/``_do_type``/``_do_key``/``_do_scroll`` and the
    shared ``_exec_run``) — there is no second action implementation. A GUI
    action returns ``{"ok": true}``; ``exec`` returns ``{"stdout","stderr",
    "code"}``. Validation is done HERE (not by Pydantic) so an unknown action,
    a missing/ill-typed field, or an unknown button raises ``ToolActionError``
    and is recorded as the failing action that stops the batch.
    """
    action = item.get("action")
    if action == "click":
        x = _require(item, "x", (int,))
        y = _require(item, "y", (int,))
        button = item.get("button", "left")
        if button not in _XBUTTON:
            raise ToolActionError(f"unknown button: {button}")
        await _do_click(int(x), int(y), button)
        return {"ok": True}
    if action == "move":
        x = _require(item, "x", (int,))
        y = _require(item, "y", (int,))
        await _do_move(int(x), int(y))
        return {"ok": True}
    if action == "type":
        text = _require(item, "text", (str,))
        await _do_type(str(text))
        return {"ok": True}
    if action == "key":
        keys = _require(item, "keys", (str,))
        await _do_key(str(keys))
        return {"ok": True}
    if action == "scroll":
        x = _require(item, "x", (int,))
        y = _require(item, "y", (int,))
        # dx/dy default to 0 (a zero-zero scroll is a no-op that still counts ok).
        dx = item.get("dx", 0)
        dy = item.get("dy", 0)
        if isinstance(dx, bool) or not isinstance(dx, int):
            raise ToolActionError("field 'dx' must be int")
        if isinstance(dy, bool) or not isinstance(dy, int):
            raise ToolActionError("field 'dy' must be int")
        await _do_scroll(int(x), int(y), int(dx), int(dy))
        return {"ok": True}
    if action == "exec":
        command = _require(item, "command", (str,))
        # Honor an optional per-item timeout (default 120.0, like ExecBody).
        timeout = item.get("timeout", 120.0)
        if timeout is not None and (
            isinstance(timeout, bool) or not isinstance(timeout, (int, float))
        ):
            raise ToolActionError("field 'timeout' must be a number")
        return await asyncio.to_thread(_exec_run, str(command), timeout)
    # Unknown (or missing) action name -> a recorded error that stops the batch.
    raise ToolActionError(f"unknown action: {action!r}")


@app.post("/actions")
async def actions(body: ActionsBody) -> JSONResponse:
    """Execute an ordered batch of actions + an optional trailing screenshot in
    ONE request (protocol #5).

    Collapses a GUI sequence (e.g. click address bar -> type URL -> Enter ->
    screenshot) that today costs N reverse-tunnel round-trips into a single
    exchange: the actions run server-side IN ORDER, reusing the EXACT
    per-action logic the individual endpoints use (the capture-free, no-``--sync``
    ``_xdotool`` input path and the shared ``sh -c`` exec runner), and an
    optional trailing screenshot is captured with the SAME protocol ``POST
    /screenshot`` uses.

    Request body::

        { "actions": [ {"action":"click","x":100,"y":50},
                       {"action":"type","text":"mailon.ai"},
                       {"action":"key","keys":"Return"} ],
          "screenshot": {"format":"jpeg","quality":75,"max_width":1280,"since":123} }

    Each ``actions`` item is ``{"action": "<click|move|type|key|scroll|exec>",
    ...same fields as that individual endpoint...}``. A successful GUI action
    yields ``{"ok": true}``; a successful ``exec`` yields ``{"stdout","stderr",
    "code"}`` (honoring an optional per-item ``timeout``, default 120.0, exactly
    like ``/exec``).

    Error policy (stop-on-first-error): actions run in order; if one raises
    (``ToolActionError`` from a bad key name / display down, an unknown
    ``action`` value, a missing/ill-typed required field, or an unknown button),
    that action's result is recorded as ``{"ok": false, "error": "<message>"}``,
    the batch STOPS IMMEDIATELY (the remaining actions do NOT run), and the
    trailing screenshot is SKIPPED. The response then carries the partial
    ``results`` list (length = failed index + 1, ending in the error object) and
    NO ``screenshot`` key.

    Response body::

        { "results": [ {"ok":true}, {"ok":true}, {"ok":true} ],
          "screenshot": { ...the /screenshot response dict... } }

    The ``screenshot`` key is present ONLY when a trailing screenshot was
    requested (``screenshot`` in the body) AND every action succeeded; its value
    is EXACTLY the dict ``/screenshot`` would have returned (same ``mode``/
    ``frame_token``/``image``/``regions``/``width``/``height``/``native_width``/
    ``native_height`` shape).
    """
    results: list[dict] = []
    for item in body.actions:
        try:
            results.append(await _dispatch_action(item))
        except ToolActionError as exc:
            # Record the failure and STOP: the remaining actions do not run and
            # the trailing screenshot is skipped (partial results, no screenshot).
            results.append({"ok": False, "error": str(exc)})
            return JSONResponse({"results": results})

    # All actions succeeded. If a trailing screenshot was requested, capture it
    # now with the same protocol /screenshot uses. NOTE: this capture happens
    # immediately after the last action with NO settle/sleep, so the captured
    # frame MAY precede the full render of the last action's on-screen effect
    # (a capture-once-stable "settle" is a later protocol task).
    if body.screenshot is not None:
        shot = body.screenshot
        try:
            payload = await _capture_screenshot(
                shot.get("since"),
                shot.get("mode"),
                shot.get("format"),
                shot.get("quality"),
                shot.get("max_width"),
            )
        except ToolActionError as exc:
            # The actions all succeeded but the trailing capture itself failed
            # (e.g. display down). Surface it as a tool error; the caller can
            # re-screenshot. Results already reflect the successful actions.
            return _err(str(exc))
        return JSONResponse({"results": results, "screenshot": payload})

    return JSONResponse({"results": results})


# Live watch endpoint (Component 9): reverse-proxy the container's noVNC client
# (page + assets + the websockify WebSocket) under /watch, so a human can watch
# the desktop through the single tunnel target. The control plane proxies its
# token-gated /sessions/{id}/watch to this. This is a SEPARATE path from
# /screenshot -- it shares no framing state with the perception loop. See
# watch.py for the prefix-proxy / asset-path / websockify-bridge details.
register_watch_routes(app, NOVNC_PORT)
