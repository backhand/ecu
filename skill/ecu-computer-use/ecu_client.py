"""
ecu_client.py — shared, clear-source client for the ECU (Easy Computer Use)
control plane. Used by both the MCP server (mcp_server.py) and the CLI
(ecu_cli.py). No compiled dependencies: standard library + `requests` only.

This client is intentionally readable end-to-end. It does three things:
  1. Wraps the ECU control-plane HTTP API (start/end session, actions).
  2. Reconstructs full screenshots from the control plane's diff protocol
     (nochange / diff / full), so callers always get a complete PNG.
  3. Optionally downscales screenshots before they reach a vision model, since
     model context — not bandwidth — is the dominant cost in a computer-use loop.

The control plane is the only thing this client talks to. It never learns the
cloud instance's address; everything is proxied through the control plane over
the secure tunnel.

Environment:
  ECU_URL       Base URL of the control plane, e.g. https://ecu.example.com
  ECU_API_KEY   Provisioned API key (Bearer token)
"""

from __future__ import annotations

import base64
import io
import os
import time
from dataclasses import dataclass, field
from typing import Optional

import requests

try:
    # Pillow is used only for diff compositing and downscaling. If it's absent,
    # the client still works for non-screenshot actions and for `mode:"full"`
    # screenshots returned as-is; diff reconstruction/downscale will raise a
    # clear error telling the user to install Pillow.
    from PIL import Image  # type: ignore
    _HAVE_PIL = True
except Exception:  # pragma: no cover
    _HAVE_PIL = False


DEFAULT_TIMEOUT = 30          # seconds, per HTTP request
READY_POLL_INTERVAL = 2.0     # seconds between start_session readiness polls
READY_TIMEOUT = 300           # seconds to wait for a session to become ready


class ECUError(Exception):
    """Any error talking to the ECU control plane."""


@dataclass
class Session:
    session_id: str
    status: str
    persistent: bool = False
    width: int = 0
    height: int = 0
    # Cached last full frame (raw PNG bytes) + its frame_token, for diffing.
    _base_png: Optional[bytes] = field(default=None, repr=False)
    _base_token: Optional[str] = field(default=None, repr=False)


class ECUClient:
    def __init__(
        self,
        base_url: Optional[str] = None,
        api_key: Optional[str] = None,
        timeout: int = DEFAULT_TIMEOUT,
    ):
        self.base_url = (base_url or os.environ.get("ECU_URL", "")).rstrip("/")
        self.api_key = api_key or os.environ.get("ECU_API_KEY", "")
        self.timeout = timeout
        if not self.base_url:
            raise ECUError("ECU_URL is not set (control plane base URL).")
        if not self.api_key:
            raise ECUError("ECU_API_KEY is not set (provisioned API key).")
        self._session = requests.Session()
        self._session.headers.update({"Authorization": f"Bearer {self.api_key}"})

    # ---- low-level HTTP ---------------------------------------------------

    def _request(self, method: str, path: str, **kwargs) -> dict:
        url = f"{self.base_url}{path}"
        try:
            resp = self._session.request(
                method, url, timeout=self.timeout, **kwargs
            )
        except requests.RequestException as e:
            raise ECUError(f"network error calling {method} {path}: {e}") from e
        if resp.status_code == 401:
            raise ECUError("unauthorized — check ECU_API_KEY")
        if resp.status_code == 429:
            raise ECUError(
                "rate/quota limit hit (too many sessions?) — "
                f"{_safe_detail(resp)}"
            )
        if not resp.ok:
            raise ECUError(
                f"{method} {path} failed [{resp.status_code}]: "
                f"{_safe_detail(resp)}"
            )
        if resp.content:
            try:
                return resp.json()
            except ValueError:
                return {"raw": resp.text}
        return {}

    # ---- session lifecycle ----------------------------------------------

    def start_session(
        self,
        persistent: bool = False,
        restore: Optional[str] = None,
        wait: bool = True,
    ) -> Session:
        """Provision a computer. Returns a Session.

        persistent: request a persistent session (privileged; the operator's
                    API key must be authorized for it, else this is rejected).
        restore:    a prior persistent session_id to restore desktop state from.
        wait:       poll until the session is ready (default True).
        """
        body = {"persistent": persistent}
        if restore:
            body["restore"] = restore
        data = self._request("POST", "/sessions", json=body)
        sess = Session(
            session_id=data["session_id"],
            status=data.get("status", "provisioning"),
            persistent=bool(data.get("persistent", persistent)),
            width=int(data.get("width", 0)),
            height=int(data.get("height", 0)),
        )
        if wait:
            self.wait_until_ready(sess)
        return sess

    def wait_until_ready(self, sess: Session, timeout: int = READY_TIMEOUT) -> Session:
        deadline = time.time() + timeout
        while time.time() < deadline:
            status = self.get_status(sess.session_id)
            sess.status = status.get("status", sess.status)
            if sess.status == "ready":
                sess.width = int(status.get("width", sess.width))
                sess.height = int(status.get("height", sess.height))
                return sess
            if sess.status == "error":
                raise ECUError(
                    f"session {sess.session_id} failed to provision: "
                    f"{status.get('detail', 'unknown error')}"
                )
            time.sleep(READY_POLL_INTERVAL)
        raise ECUError(
            f"session {sess.session_id} not ready after {timeout}s "
            f"(last status: {sess.status})"
        )

    def get_status(self, session_id: str) -> dict:
        return self._request("GET", f"/sessions/{session_id}")

    def end_session(self, session_id: str) -> dict:
        """Tear down. Ephemeral sessions are destroyed; persistent sessions are
        snapshotted and stopped. Always call this when finished, even on error."""
        return self._request("DELETE", f"/sessions/{session_id}")

    # ---- actions ---------------------------------------------------------

    def click(self, session_id, x, y, button="left"):
        return self._action(session_id, "click", {"x": x, "y": y, "button": button})

    def move(self, session_id, x, y):
        return self._action(session_id, "move", {"x": x, "y": y})

    def type(self, session_id, text):
        return self._action(session_id, "type", {"text": text})

    def key(self, session_id, keys):
        return self._action(session_id, "key", {"keys": keys})

    def scroll(self, session_id, x, y, dx=0, dy=0):
        return self._action(session_id, "scroll", {"x": x, "y": y, "dx": dx, "dy": dy})

    def exec(self, session_id, command):
        return self._action(session_id, "exec", {"command": command})

    def _action(self, session_id: str, name: str, body: dict) -> dict:
        return self._request("POST", f"/sessions/{session_id}/{name}", json=body)

    # ---- screenshots (diff-aware) ---------------------------------------

    def screenshot(
        self,
        sess: Session,
        max_width: Optional[int] = 1280,
        force_full: bool = False,
    ) -> "Screenshot":
        """Capture the screen and return a complete, ready-to-show PNG.

        Handles the control plane's diff protocol transparently:
          - mode "nochange": the screen is identical to what we last held; we
            return the cached frame WITHOUT re-fetching a full image. This is
            the cheap path that makes 'did my click land yet?' polling almost
            free in both bytes and model tokens.
          - mode "diff": only changed regions came back; we composite them onto
            the cached base frame to reconstruct the full image.
          - mode "full": a complete frame; we cache it as the new base.

        max_width: if set, downscale the returned image so its width <= max_width
                   before handing it to a model. Downscaling is the primary
                   *token* lever; keep it modest enough to stay legible.
        force_full: ignore the cache and demand a full frame.
        """
        body = {"mode": "full" if force_full else "auto"}
        if sess._base_token and not force_full:
            body["since"] = sess._base_token
        data = self._request("POST", f"/sessions/{sess.session_id}/screenshot", json=body)
        mode = data.get("mode")

        if mode == "nochange":
            if sess._base_png is None:
                # We claimed to hold a frame but don't — recover by forcing full.
                return self.screenshot(sess, max_width=max_width, force_full=True)
            png = sess._base_png

        elif mode == "full":
            png = base64.b64decode(data["image"])
            sess._base_png = png
            sess._base_token = data.get("frame_token")
            sess.width = int(data.get("width", sess.width))
            sess.height = int(data.get("height", sess.height))

        elif mode == "diff":
            png = self._apply_diff(sess, data)
            sess._base_png = png
            sess._base_token = data.get("frame_token")

        else:
            raise ECUError(f"unexpected screenshot mode: {mode!r}")

        out_png, w, h = self._maybe_downscale(png, max_width)
        return Screenshot(png=out_png, width=w, height=h, mode=mode or "full")

    def _apply_diff(self, sess: Session, data: dict) -> bytes:
        if not _HAVE_PIL:
            raise ECUError(
                "screenshot diff reconstruction needs Pillow. "
                "Install it (`pip install Pillow`) or the control plane can be "
                "asked for full frames only."
            )
        if sess._base_png is None:
            # No base to composite onto — force a full frame instead.
            full = self.screenshot(sess, max_width=None, force_full=True)
            return full.png
        base = Image.open(io.BytesIO(sess._base_png)).convert("RGB")
        for region in data.get("regions", []):
            patch = Image.open(io.BytesIO(base64.b64decode(region["image"]))).convert("RGB")
            base.paste(patch, (int(region["x"]), int(region["y"])))
        buf = io.BytesIO()
        base.save(buf, format="PNG")
        return buf.getvalue()

    def _maybe_downscale(self, png: bytes, max_width: Optional[int]):
        if not max_width:
            if _HAVE_PIL:
                im = Image.open(io.BytesIO(png))
                return png, im.width, im.height
            return png, 0, 0
        if not _HAVE_PIL:
            # Can't measure/resize without Pillow; return as-is.
            return png, 0, 0
        im = Image.open(io.BytesIO(png)).convert("RGB")
        if im.width <= max_width:
            return png, im.width, im.height
        ratio = max_width / im.width
        new_size = (max_width, max(1, round(im.height * ratio)))
        im = im.resize(new_size, Image.LANCZOS)
        buf = io.BytesIO()
        im.save(buf, format="PNG")
        return buf.getvalue(), im.width, im.height


@dataclass
class Screenshot:
    png: bytes
    width: int
    height: int
    mode: str  # "full" | "diff" | "nochange" — informational

    def b64(self) -> str:
        return base64.b64encode(self.png).decode("ascii")

    def save(self, path: str) -> str:
        with open(path, "wb") as f:
            f.write(self.png)
        return path


def _safe_detail(resp) -> str:
    try:
        j = resp.json()
        return str(j.get("detail") or j.get("error") or j)
    except Exception:
        return resp.text[:200]
