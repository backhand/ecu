"""
mcp_server.py — canonical ECU (Easy Computer Use) MCP server.

This is the trustable, clear-source agent-side surface: it exposes a remote
Linux desktop to an MCP host (Claude Code, Claude Desktop, Cowork, etc.) as a
small set of typed tools. It is a thin client of the ECU control plane — it
holds the API key, talks HTTPS to the control plane, and lets the host model do
all the vision and reasoning.

Run it (stdio transport, the usual MCP host integration):
    ECU_URL=https://ecu.example.com ECU_API_KEY=... python mcp_server.py

Dependencies (no compiled deps beyond Pillow, which is only needed for
screenshot diff compositing / downscaling):
    pip install "mcp[cli]" requests Pillow

Design notes:
  * Screenshots are returned as MCP image content, already reconstructed from
    the control plane's diff protocol. The frame is downscaled AND lossy-encoded
    at the source (server-side) for both wire and token efficiency, then handed
    to the model as a complete PNG still — the model never sees a diff.
  * `screenshot` is an explicit tool, NOT auto-attached to every action result.
    This keeps image tokens bounded. Take a screenshot when you need to look.
  * When polling after an action ("did it land yet?"), the control plane returns
    a cheap "nochange" under the hood. On that path the `screenshot` tool returns
    a short TEXT sentinel instead of an Image, so repeated polls of a static
    screen cost almost no model tokens (no image re-tokenized). `force_full`
    forces a fresh image when the model truly wants pixels again.
  * Click/move/scroll coordinates are taken in the LAST screenshot's pixel
    space. If that screenshot was downscaled (`max_width`), the action tools
    read the session's recorded scale and translate the coordinates back up to
    native resolution, so the model can click exactly where it sees things.
"""

from __future__ import annotations

import os
from typing import Optional

from mcp.server.fastmcp import FastMCP, Image

from ecu_client import ECUClient, ECUError, Session


mcp = FastMCP("ecu-computer-use")

# A single client; sessions are tracked by id. (One MCP server process typically
# serves one host/agent. Multiple concurrent sessions are supported by id.)
_client: Optional[ECUClient] = None
_sessions: dict[str, Session] = {}


def client() -> ECUClient:
    global _client
    if _client is None:
        _client = ECUClient()  # reads ECU_URL / ECU_API_KEY from env
    return _client


def _get(session_id: str) -> Session:
    sess = _sessions.get(session_id)
    if sess is None:
        # Re-hydrate a lightweight Session if the host references one we don't
        # have cached (e.g. server restarted). Screenshot caching restarts cold.
        sess = Session(session_id=session_id, status="ready")
        _sessions[session_id] = sess
    return sess


@mcp.tool()
def start_session(persistent: bool = False, restore: str = "") -> str:
    """Provision a fresh cloud computer (Linux desktop) and wait until it's ready.

    Returns the session_id you pass to every other tool. Call end_session when
    you're done — always, even if the task failed.

    This blocks until the desktop is ACTION-READY: it not only waits for the
    instance to come up but also warms the screenshot + exec paths, so your very
    first screenshot/click isn't slowed by the cold-desktop start. (Transient
    timeouts during the wait are retried automatically.)

    persistent: request a session whose desktop state survives end_session.
                This is a PRIVILEGED capability: it only works if this
                deployment's API key is authorized for it, otherwise the request
                is rejected with an explanation. Use only when you genuinely
                need state to persist across sessions.
    restore:    a prior persistent session_id to restore desktop state from.
    """
    sess = client().start_session(
        persistent=persistent, restore=(restore or None), wait=True
    )
    _sessions[sess.session_id] = sess
    return (
        f"session_id={sess.session_id} status={sess.status} "
        f"size={sess.width}x{sess.height} persistent={sess.persistent}"
    )


# structured_output=False: this tool returns CONTENT (an Image, or a short text
# sentinel on the 'nochange' path), not a structured result — so FastMCP should
# not try to build an output JSON schema from the Image|str return annotation.
@mcp.tool(structured_output=False)
def screenshot(
    session_id: str,
    max_width: int = 1280,
    force_full: bool = False,
    settle: bool = False,
    settle_ms: int = 0,
    max_wait_ms: int = 0,
) -> Image | str:
    """Capture the current screen and return it as an image to look at.

    Take a screenshot when you need to SEE the screen — after a navigation, to
    verify an action landed, or to decide what to do next. You don't need one
    after every single action.

    Repeated screenshots of an unchanged screen are cheap: when nothing has
    changed since your last screenshot, this returns a short TEXT note
    ("no change since last screenshot ...") INSTEAD of re-sending the image, so
    polling 'did it land yet?' costs almost no tokens. When the screen has
    changed you get a fresh image as usual.

    max_width:  the image is downscaled to this width for legibility/cost.
                1280 is a good default; lower it if you only need a rough look.
                Subsequent click/move/scroll calls are automatically translated
                into this same (downscaled) coordinate space, so click where you
                see things.
    force_full: ignore the 'no change' optimization and always return a fresh
                full image — use it when you want to re-examine the pixels even
                though the screen hasn't changed.
    settle:     capture-once-stable. When True, the server waits until the screen
                has STOPPED CHANGING (a window finished painting, a page finished
                loading) and returns that stable frame instead of a possibly
                mid-render one. PREFER this over taking a screenshot, seeing it's
                still loading, and manually waiting/polling — set settle=True
                right after the action that triggers a load or a window open. It
                never hangs: it gives up at max_wait_ms and returns the latest
                frame.
    settle_ms:  how long (ms) the screen must be unchanged to count as settled
                (implies settle). 0 = use the server default (~300 ms).
    max_wait_ms: hard cap (ms) on the total settle wait. 0 = server default
                (~2500 ms). An endlessly-animating screen returns at this cap.
    """
    sess = _get(session_id)
    # The MCP surface uses 0 to mean "unset" for the millisecond knobs (the wire
    # wants None there); convert so an omitted arg is the server default.
    shot = client().screenshot(
        sess,
        max_width=max_width,
        force_full=force_full,
        settle=settle,
        settle_ms=(settle_ms or None),
        max_wait_ms=(max_wait_ms or None),
    )
    if shot.mode == "nochange":
        # Don't re-tokenize an identical frame: hand back a tiny text sentinel.
        # `force_full=True` forces a real image if the model wants pixels again.
        return f"no change since last screenshot (frame {shot.frame_token})"
    return Image(data=shot.png, format="png")


@mcp.tool()
def click(session_id: str, x: int, y: int, button: str = "left") -> str:
    """Click at pixel coordinates (x, y). button is 'left', 'right', or 'middle'.
    Coordinates are in the pixel space of the last screenshot you were shown
    (the tools rescale to native resolution automatically if it was downscaled)."""
    sess = _get(session_id)
    client().click(session_id, x, y, button, scale=sess.screenshot_scale)
    return f"clicked {button} at ({x},{y})"


@mcp.tool()
def move(session_id: str, x: int, y: int) -> str:
    """Move the mouse cursor to (x, y) without clicking. Coordinates are in the
    last screenshot's pixel space (rescaled to native automatically)."""
    sess = _get(session_id)
    client().move(session_id, x, y, scale=sess.screenshot_scale)
    return f"moved to ({x},{y})"


@mcp.tool()
def type_text(session_id: str, text: str) -> str:
    """Type a string of text at the current focus (as if typed on the keyboard)."""
    client().type(session_id, text)
    return f"typed {len(text)} chars"


@mcp.tool()
def key(session_id: str, keys: str) -> str:
    """Press a key or chord, e.g. 'Return', 'ctrl+l', 'alt+Tab', 'ctrl+shift+t'.
    Use this for shortcuts and special keys rather than type_text."""
    client().key(session_id, keys)
    return f"pressed {keys}"


@mcp.tool()
def scroll(session_id: str, x: int, y: int, dx: int = 0, dy: int = 0) -> str:
    """Scroll at (x, y). Positive dy scrolls down, negative up; dx scrolls
    horizontally. Magnitudes are scroll 'clicks'. The (x, y) anchor is in the
    last screenshot's pixel space (rescaled to native automatically); dx/dy are
    scroll clicks and are not rescaled."""
    sess = _get(session_id)
    client().scroll(session_id, x, y, dx, dy, scale=sess.screenshot_scale)
    return f"scrolled dx={dx} dy={dy} at ({x},{y})"


@mcp.tool()
def exec_command(session_id: str, command: str, timeout: int = 0) -> str:
    """Run a shell command on the computer and return stdout/stderr/exit code.

    Prefer this over GUI clicking when a task has a clean command-line path
    (launching apps, file ops, installing packages) — it's faster and more
    reliable than driving the mouse. Use the GUI for things only the GUI can do.

    The command runs as ``sh -c <command>`` (so pipes, ``&&``, loops, and
    redirects work) as the unprivileged user ``computeruse`` in
    ``/home/computeruse``. It is one-shot — there is no shell that persists
    between calls. The result is returned as text: an ``exit=<code>`` line, then
    a ``stdout:`` block and a ``stderr:`` block when non-empty.

    A FOREGROUND command returns when it finishes (server run budget 120 s by
    default; if it overruns it is killed and comes back with ``exit=124``).
    Background a long-running GUI app with a trailing ``&`` (e.g.
    ``firefox-esr >/dev/null 2>&1 &``) so exec returns immediately instead of
    blocking on it.

    timeout: server-side run budget in seconds for THIS command. 0 (default)
             uses the server's 120 s default; raise it for a legitimately long
             job (a big install, a slow build).
    """
    # 0 -> None: let the server apply its own default budget when unset.
    res = client().exec(session_id, command, timeout=(timeout or None))
    out = res.get("stdout", "")
    err = res.get("stderr", "")
    code = res.get("code", res.get("exit_code", "?"))
    parts = [f"exit={code}"]
    if out:
        parts.append(f"stdout:\n{out}")
    if err:
        parts.append(f"stderr:\n{err}")
    return "\n".join(parts)


@mcp.tool()
def watch_url(session_id: str) -> str:
    """Get a URL a HUMAN can open in a browser to watch this session live.

    This is for human oversight only (looking over the agent's shoulder); it is
    not part of how you perceive the screen — use screenshot for that.
    """
    status = client().get_status(session_id)
    url = status.get("watch_url")
    if not url:
        return "no watch URL available for this session"
    return url


@mcp.tool()
def end_session(session_id: str) -> str:
    """Tear down the computer. ALWAYS call this when finished, even on failure.

    Ephemeral sessions are destroyed (freeing the cloud instance and stopping
    billing). Persistent sessions are snapshotted and stopped so they can be
    restored later.
    """
    res = client().end_session(session_id)
    _sessions.pop(session_id, None)
    return f"ended session {session_id}: {res.get('status', 'terminated')}"


if __name__ == "__main__":
    # Fail fast with a clear message if config is missing.
    try:
        client()
    except ECUError as e:
        raise SystemExit(f"ECU MCP server cannot start: {e}")
    mcp.run()
