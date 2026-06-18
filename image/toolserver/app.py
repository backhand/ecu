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
    """Raised when the wrapped ComputerTool reports an error."""


async def _computer_action(**kwargs) -> object:
    """Invoke the demo ComputerTool and surface tool-level errors.

    ``ComputerTool.__call__`` is async and returns a ``ToolResult`` (frozen
    dataclass with .output/.error/.base64_image) or raises ``ToolError``.
    """
    try:
        result = await _computer(**kwargs)
    except Exception as exc:  # ToolError and friends
        raise ToolActionError(str(getattr(exc, "message", exc))) from exc
    # A populated .error with no image generally means the xdotool/scrot call
    # failed (e.g. display not up). Treat it as an error for action endpoints.
    if getattr(result, "error", None) and not getattr(result, "base64_image", None):
        raise ToolActionError(result.error or "tool error")
    return result


async def _capture_png() -> bytes:
    """Take a screenshot via the demo tool and return raw PNG bytes."""
    result = await _computer(action="screenshot")
    b64 = getattr(result, "base64_image", None)
    if not b64:
        raise ToolActionError(getattr(result, "error", None) or "screenshot failed")
    return base64.b64decode(b64)


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


# ---------------------------------------------------------------------------
# Endpoints
# ---------------------------------------------------------------------------

_CLICK_ACTION = {
    "left": "left_click",
    "right": "right_click",
    "middle": "middle_click",
}


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
        png = await _capture_png()
    except ToolActionError as exc:
        return _err(str(exc))

    pil_fmt, quality, max_width = _normalize_encoding(
        body.format, body.quality, body.max_width
    )

    def _prepare() -> tuple[np.ndarray, int, int]:
        native = _decode_rgb(png)
        native_h, native_w = native.shape[:2]
        shown = _downscale_to_width(native, max_width)
        return shown, native_w, native_h

    pixels, native_w, native_h = await asyncio.to_thread(_prepare)
    base_token, _base_image, base_pixels, base_width = _frames.snapshot()

    # Forced full / first frame / caller's base doesn't match ours, or the
    # requested max_width yields a different shown width than the held base (a
    # different base size can't be tile-diffed) -> full.
    if (
        body.mode == "full"
        or base_pixels is None
        or body.since != base_token
        or base_width != pixels.shape[1]
    ):
        payload = await asyncio.to_thread(
            _full_response, pixels, native_w, native_h, pil_fmt, quality
        )
        return JSONResponse(payload)

    payload = await asyncio.to_thread(
        _diff_or_full,
        pixels,
        base_token,
        base_pixels,
        native_w,
        native_h,
        pil_fmt,
        quality,
    )
    return JSONResponse(payload)


@app.post("/click")
async def click(body: ClickBody) -> JSONResponse:
    try:
        await _computer_action(
            action=_CLICK_ACTION[body.button], coordinate=[body.x, body.y]
        )
    except ToolActionError as exc:
        return _err(str(exc))
    return _ok()


@app.post("/move")
async def move(body: MoveBody) -> JSONResponse:
    try:
        await _computer_action(action="mouse_move", coordinate=[body.x, body.y])
    except ToolActionError as exc:
        return _err(str(exc))
    return _ok()


@app.post("/type")
async def type_(body: TypeBody) -> JSONResponse:
    try:
        await _computer_action(action="type", text=body.text)
    except ToolActionError as exc:
        return _err(str(exc))
    return _ok()


@app.post("/key")
async def key(body: KeyBody) -> JSONResponse:
    # Maps to the demo's "key" action -> `xdotool key -- <keys>`. xdotool
    # syntax, e.g. "ctrl+l", "Return", "alt+F4".
    try:
        await _computer_action(action="key", text=body.keys)
    except ToolActionError as exc:
        return _err(str(exc))
    return _ok()


@app.post("/scroll")
async def scroll(body: ScrollBody) -> JSONResponse:
    """Map dx/dy onto the demo's scroll action.

    The demo scroll takes coordinate + scroll_direction + scroll_amount, so we
    emit one scroll per nonzero axis: dy>0 down / dy<0 up, dx>0 right / dx<0
    left, amount = abs(value).
    """
    try:
        if body.dy:
            await _computer_action(
                action="scroll",
                coordinate=[body.x, body.y],
                scroll_direction="down" if body.dy > 0 else "up",
                scroll_amount=abs(body.dy),
            )
        if body.dx:
            await _computer_action(
                action="scroll",
                coordinate=[body.x, body.y],
                scroll_direction="right" if body.dx > 0 else "left",
                scroll_amount=abs(body.dx),
            )
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
    """

    def _run() -> dict:
        try:
            proc = subprocess.run(
                ["sh", "-c", body.command],
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                timeout=body.timeout,
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
                "stderr": err + f"\ncommand timed out after {body.timeout}s",
                "code": 124,
            }

    result = await asyncio.to_thread(_run)
    return JSONResponse(result)


# Live watch endpoint (Component 9): reverse-proxy the container's noVNC client
# (page + assets + the websockify WebSocket) under /watch, so a human can watch
# the desktop through the single tunnel target. The control plane proxies its
# token-gated /sessions/{id}/watch to this. This is a SEPARATE path from
# /screenshot -- it shares no framing state with the perception loop. See
# watch.py for the prefix-proxy / asset-path / websockify-bridge details.
register_watch_routes(app, NOVNC_PORT)
