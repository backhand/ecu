"""FastAPI tool server for an ECU computer-use instance.

This wraps the *existing* tool implementations shipped in the anthropic
computer-use-demo image (``computer_use_demo.tools``) and exposes them over
HTTP. We deliberately do NOT reimplement the input/action logic: every
GUI action is forwarded to the demo's ``ComputerTool`` (xdotool/scrot under
the hood). Only ``/exec`` and the ``/screenshot`` framing add local logic.

Component 1 of ECU. The screenshot diff protocol (``nochange``/``diff``) is
Component 6; here we always return ``mode:"full"`` but keep the last frame
bytes and a monotonic ``frame_token`` so diffing can slot in without changing
the wire contract.

Security note: this binds to 0.0.0.0 *inside the container* on purpose. The
"localhost-only" property is enforced by how the instance publishes the port
(``docker -p 127.0.0.1:PORT`` + firewall, Component 4), not here.
"""

from __future__ import annotations

import asyncio
import base64
import os
import struct
import subprocess
import threading
from typing import Literal

from fastapi import FastAPI
from fastapi.responses import JSONResponse, RedirectResponse
from pydantic import BaseModel, Field

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
# serving /opt/noVNC, so vnc.html is the client page). /watch redirects here
# until the control plane proxies it (Component 9).
NOVNC_PORT = int(os.getenv("ECU_NOVNC_PORT") or 6080)
WATCH_PATH = os.getenv("ECU_WATCH_PATH", "/vnc.html")

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
    """Holds the most recent captured frame and a monotonic token.

    Kept here (rather than thrown away) so the Component 6 diff protocol can
    compare against the last frame and emit ``nochange``/``diff`` without any
    change to this server's external shape.
    """

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._token = 0
        self._last_png: bytes | None = None

    def next(self, png: bytes) -> int:
        with self._lock:
            self._token += 1
            self._last_png = png
            return self._token

    @property
    def last_png(self) -> bytes | None:
        with self._lock:
            return self._last_png

    @property
    def token(self) -> int:
        with self._lock:
            return self._token


_frames = _FrameState()


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _png_dimensions(png: bytes) -> tuple[int, int] | None:
    """Return (width, height) parsed from a PNG header, or None."""
    # PNG signature (8 bytes) + IHDR length+type (8 bytes) then W,H as uint32be.
    if len(png) >= 24 and png[:8] == b"\x89PNG\r\n\x1a\n":
        width, height = struct.unpack(">II", png[16:24])
        return width, height
    return None


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


@app.post("/screenshot")
async def screenshot(body: ScreenshotBody) -> JSONResponse:
    """Capture a frame.

    Component 1 always returns a full frame. The handler keeps the last frame
    bytes and a monotonic frame_token so the Component 6 diff protocol
    (nochange/diff against ``since``) can be added here without changing the
    response shape callers already depend on.
    """
    try:
        png = await _capture_png()
    except ToolActionError as exc:
        return _err(str(exc))

    token = _frames.next(png)

    dims = _png_dimensions(png)
    width, height = dims if dims else (WIDTH, HEIGHT)

    return JSONResponse(
        {
            "mode": "full",
            "frame_token": token,
            "image": base64.b64encode(png).decode(),
            "width": width,
            "height": height,
        }
    )


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


@app.get("/watch")
async def watch() -> RedirectResponse:
    """Redirect to the container's noVNC client.

    For now this is a 302 to the local noVNC page; the full tunneled proxy is
    Component 9. autoconnect/resize make the embedded viewer usable directly.
    """
    target = (
        f"http://localhost:{NOVNC_PORT}{WATCH_PATH}"
        "?autoconnect=true&resize=scale"
    )
    return RedirectResponse(url=target, status_code=302)
