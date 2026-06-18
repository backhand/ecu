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
    the control plane's diff protocol and downscaled for model legibility. The
    model never sees a diff — it always gets a complete still.
  * `screenshot` is an explicit tool, NOT auto-attached to every action result.
    This keeps image tokens bounded. Take a screenshot when you need to look.
  * When polling after an action ("did it land yet?"), the control plane returns
    a cheap "nochange" under the hood, so repeated screenshots of a static
    screen cost almost nothing.
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


@mcp.tool()
def screenshot(session_id: str, max_width: int = 1280) -> Image:
    """Capture the current screen and return it as an image to look at.

    Take a screenshot when you need to SEE the screen — after a navigation, to
    verify an action landed, or to decide what to do next. You don't need one
    after every single action. Repeated screenshots of an unchanged screen are
    cheap (the system detects 'no change' automatically).

    max_width: the image is downscaled to this width for legibility/cost.
               1280 is a good default; lower it if you only need a rough look.
    """
    sess = _get(session_id)
    shot = client().screenshot(sess, max_width=max_width)
    return Image(data=shot.png, format="png")


@mcp.tool()
def click(session_id: str, x: int, y: int, button: str = "left") -> str:
    """Click at pixel coordinates (x, y). button is 'left', 'right', or 'middle'.
    Coordinates are in the screenshot's pixel space."""
    client().click(session_id, x, y, button)
    return f"clicked {button} at ({x},{y})"


@mcp.tool()
def move(session_id: str, x: int, y: int) -> str:
    """Move the mouse cursor to (x, y) without clicking."""
    client().move(session_id, x, y)
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
    horizontally. Magnitudes are scroll 'clicks'."""
    client().scroll(session_id, x, y, dx, dy)
    return f"scrolled dx={dx} dy={dy} at ({x},{y})"


@mcp.tool()
def exec_command(session_id: str, command: str) -> str:
    """Run a shell command on the computer and return stdout/stderr/exit code.

    Prefer this over GUI clicking when a task has a clean command-line path
    (launching apps, file ops, installing packages) — it's faster and more
    reliable than driving the mouse. Use the GUI for things only the GUI can do.
    """
    res = client().exec(session_id, command)
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
