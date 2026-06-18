"""
ecu_client.py — shared, clear-source client for the ECU (Easy Computer Use)
control plane. Used by both the MCP server (mcp_server.py) and the CLI
(ecu_cli.py). No compiled dependencies: standard library + `requests` only.

This client is intentionally readable end-to-end. It does three things:
  1. Wraps the ECU control-plane HTTP API (start/end session, actions).
  2. Reconstructs full screenshots from the control plane's diff protocol
     (nochange / diff / full), so callers always get a complete PNG.
  3. Asks the server to downscale + lossy-encode each frame at the source
     (max_width / format / quality on the request), so the wire carries ~20-60
     KB instead of a ~1 MB full-res PNG, and records the native/shown scale so
     click/move/scroll coordinates translate back to native pixels. Both
     bandwidth AND model-context cost are reduced, at the source.

The reconstructed base is held as DECODED pixels in memory and only changed
regions are pasted onto it between diffs — unchanged content is never re-encoded
through the lossy codec, so repeated diffs don't accumulate compression damage.

The control plane is the only thing this client talks to. It never learns the
cloud instance's address; everything is proxied through the control plane over
the secure tunnel.

Environment:
  ECU_URL       Base URL of the control plane, e.g. https://ecu.example.com
  ECU_API_KEY   Provisioned API key (Bearer token)
"""

from __future__ import annotations

import base64
import contextlib
import io
import json
import os
import threading
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Optional, Union

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
    # Scale factor of the LAST screenshot returned for this session:
    # scale = native_width / shown_width. 1.0 means the last frame was shown at
    # native resolution (no downscale). It is > 1.0 when the frame was downscaled
    # (e.g. a 1280px-native frame shown at max_width=640 has scale 2.0). The
    # front-ends multiply model-supplied (shown-space) click/move/scroll
    # coordinates by this factor to recover native-space coordinates for the
    # server, keeping the coordinate space consistent with what the model saw.
    screenshot_scale: float = 1.0
    # Cached last full frame (PNG bytes) + its frame_token, for diffing and for
    # the sidecar cache. The token is an integer on the wire; we keep the field
    # permissive (it may arrive as int or str) and echo back whatever value we
    # were given as `since`. These bytes are a *lossless* (PNG) snapshot of the
    # reconstructed frame, so persisting/restoring the base never adds loss.
    _base_png: Optional[bytes] = field(default=None, repr=False)
    _base_token: Optional[Union[int, str]] = field(default=None, repr=False)
    # In-memory decoded base (a PIL RGB Image), kept alongside the bytes so diff
    # compositing pastes changed regions onto live pixels rather than re-decoding
    # /re-encoding the whole frame each time. Never persisted (rebuilt on load
    # from _base_png). Absent when Pillow isn't installed.
    _base_image: object = field(default=None, repr=False, compare=False)


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
        # Per-session request serialization (protocol #4). The single-tenant tool
        # server processes one input action at a time; firing two requests at the
        # SAME session concurrently is never useful and only invites surprises
        # (an action queued behind a slow screenshot). We therefore hold a
        # per-session lock for the duration of each request so all calls to one
        # session are serialized, while DIFFERENT sessions still run fully in
        # parallel (each has its own lock). `_locks_guard` is a tiny global lock
        # held only to get-or-create a session's lock — never during the HTTP
        # call itself. The per-session lock is an RLock so the screenshot diff/
        # nochange recovery path (which re-enters screenshot() -> _request on the
        # same thread/session) doesn't self-deadlock.
        self._locks_guard = threading.Lock()
        self._session_locks: dict[str, threading.RLock] = {}

    # ---- per-session serialization ---------------------------------------

    def _session_lock(self, session_id: str) -> threading.RLock:
        """Get (or lazily create) the lock that serializes one session's calls."""
        with self._locks_guard:
            lock = self._session_locks.get(session_id)
            if lock is None:
                lock = threading.RLock()
                self._session_locks[session_id] = lock
            return lock

    @contextlib.contextmanager
    def _serialize(self, session_id: Optional[str]):
        """Hold the given session's lock for the wrapped block.

        ``session_id is None`` (e.g. the session-less ``POST /sessions`` create)
        means "nothing to serialize against" — we yield without locking so
        session creation never blocks behind another session's traffic.
        """
        if not session_id:
            yield
            return
        with self._session_lock(session_id):
            yield

    @staticmethod
    def _session_id_from_path(path: str) -> Optional[str]:
        """Extract the session id from a control-plane path, or None.

        Paths are ``/sessions`` (create — no id) or ``/sessions/{id}[/...]``. We
        key the per-session lock on ``{id}`` so every action/screenshot/status
        call for one session serializes, while the id-less create does not.
        """
        parts = [p for p in path.split("/") if p]
        if len(parts) >= 2 and parts[0] == "sessions":
            return parts[1]
        return None

    # ---- low-level HTTP ---------------------------------------------------

    def _request(self, method: str, path: str, **kwargs) -> dict:
        # Serialize per session: the lock is keyed off the {id} in the path so
        # two concurrent calls to the same session can't overlap on the wire,
        # while different sessions proceed in parallel. Held across the whole
        # request/response (this is what prevents a click racing a screenshot
        # in-process); released as soon as we return.
        with self._serialize(self._session_id_from_path(path)):
            return self._do_request(method, path, **kwargs)

    def _do_request(self, method: str, path: str, **kwargs) -> dict:
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
    #
    # Coordinate scaling (`scale`): when the last screenshot was downscaled for
    # the model, the model reasons in SHOWN-space pixels, but the server expects
    # NATIVE-space pixels. Pass `scale = native_width / shown_width` (i.e. the
    # `Session.screenshot_scale` from the last screenshot) and these methods
    # translate the anchor coordinate up to native space before sending:
    #   native = round(shown * scale)
    # `scale=1.0` (the default) is a no-op, so callers that never downscale —
    # or that already work in native space — are unaffected. Scroll *deltas*
    # (dx, dy) are scroll "clicks", not coordinates, and are NEVER scaled.

    def click(self, session_id, x, y, button="left", scale: float = 1.0):
        x, y = _scale_xy(x, y, scale)
        return self._action(session_id, "click", {"x": x, "y": y, "button": button})

    def move(self, session_id, x, y, scale: float = 1.0):
        x, y = _scale_xy(x, y, scale)
        return self._action(session_id, "move", {"x": x, "y": y})

    def type(self, session_id, text):
        return self._action(session_id, "type", {"text": text})

    def key(self, session_id, keys):
        return self._action(session_id, "key", {"keys": keys})

    def scroll(self, session_id, x, y, dx=0, dy=0, scale: float = 1.0):
        # Only the (x, y) anchor is a coordinate; dx/dy are scroll clicks.
        x, y = _scale_xy(x, y, scale)
        return self._action(session_id, "scroll", {"x": x, "y": y, "dx": dx, "dy": dy})

    def exec(self, session_id, command):
        return self._action(session_id, "exec", {"command": command})

    # Anchor fields scaled (shown-space -> native) for batch actions, by action.
    # Only the pointer ANCHOR is a coordinate; scroll dx/dy are scroll clicks and
    # are never scaled (the same rule click/move/scroll above follow).
    _BATCH_ANCHOR_ACTIONS = frozenset({"click", "move", "scroll"})

    def actions(
        self,
        sess: Session,
        actions: list[dict],
        screenshot: Optional[dict] = None,
        scale: Optional[float] = None,
    ) -> dict:
        """Run an ordered batch of actions + an optional trailing screenshot in
        ONE round-trip (protocol #5), and reconstruct the trailing frame.

        Collapses a GUI sequence (e.g. click address bar -> type URL -> Enter ->
        screenshot) that today costs N reverse-tunnel round-trips into a single
        ``POST /sessions/{id}/actions`` exchange. The server executes the actions
        in order (reusing the same per-action logic as the individual endpoints)
        and, if a trailing ``screenshot`` is requested AND every action
        succeeded, captures one frame with the same diff protocol as
        ``screenshot()``.

        ``actions``: a list of ``{"action": "<click|move|type|key|scroll|exec>",
            ...}`` items, each carrying the SAME fields as the matching
            single-action method. The list is NOT mutated — a new list of scaled
            item dicts is built and sent.

        Coordinate scaling: every item whose ``action`` is ``click``/``move``/
            ``scroll`` has its ``x``/``y`` ANCHOR scaled from shown-space to
            native via ``_scale_xy(x, y, scale)`` BEFORE sending, exactly like
            ``click()``/``move()``/``scroll()`` do. Scroll ``dx``/``dy`` (scroll
            clicks) and all non-anchor fields (``text``/``keys``/``command``/
            ``button``/``timeout``) are left untouched. ``scale`` defaults to
            ``None`` meaning "use ``sess.screenshot_scale``" (what the MCP/CLI
            front-ends pass after a downscaled screenshot); pass an explicit
            ``scale`` to override, or ``1.0`` for a no-op.

        ``screenshot``: an optional trailing-screenshot request dict mirroring
            the ``POST /screenshot`` body (``since``/``mode``/``max_width``/
            ``format``/``quality``); passed through verbatim in the POST body. It
            is honored by the server only if every action succeeded.

        Returns ``{"results": [...], "screenshot": <Screenshot or None>}``:
          * ``results`` is the server's per-action results list VERBATIM — each
            entry is ``{"ok": true}`` (a GUI action), an exec result
            ``{"stdout","stderr","code"}``, or ``{"ok": false, "error": ...}``
            for the action that failed (the batch stops there; later actions did
            not run). Inspect it for the per-action outcome.
          * ``screenshot`` is a fully reconstructed ``Screenshot`` when the
            response carried a trailing screenshot (all actions succeeded and one
            was requested), else ``None``. It is reconstructed through the SAME
            diff/nochange/full path ``screenshot()`` uses (the shared
            ``_screenshot_from_response`` helper), so a ``diff`` composites onto
            the session base, a ``nochange`` returns the cached bytes, and the
            coordinate scale is recorded on ``sess``.

        The whole call goes through ``_request`` so it holds the per-session lock
        (serialized against other calls to this session) for the round-trip.
        """
        # Resolve the scale source: explicit arg wins, else the session's last
        # screenshot scale (the front-ends' default), else a 1.0 no-op.
        eff_scale = sess.screenshot_scale if scale is None else scale

        # Build a NEW list of (possibly) scaled item dicts; never mutate caller's.
        scaled: list[dict] = []
        for item in actions:
            new_item = dict(item)
            if (
                new_item.get("action") in self._BATCH_ANCHOR_ACTIONS
                and "x" in new_item
                and "y" in new_item
            ):
                nx, ny = _scale_xy(new_item["x"], new_item["y"], eff_scale)
                new_item["x"], new_item["y"] = nx, ny
            scaled.append(new_item)

        body: dict = {"actions": scaled}
        if screenshot is not None:
            body["screenshot"] = screenshot

        data = self._request(
            "POST", f"/sessions/{sess.session_id}/actions", json=body
        )

        results = data.get("results", [])
        shot: Optional[Screenshot] = None
        raw_shot = data.get("screenshot")
        if raw_shot is not None:
            # Reconstruct the trailing frame identically to screenshot(): compose
            # diffs onto the session base, honor nochange, derive + record scale.
            # Use the trailing-screenshot request's encoding params (so the
            # no-base recovery path re-fetches a matching full frame).
            sshot = screenshot or {}
            shot = self._screenshot_from_response(
                sess,
                raw_shot,
                max_width=sshot.get("max_width"),
                format=sshot.get("format", "jpeg"),
                quality=sshot.get("quality", 75),
            )
        return {"results": results, "screenshot": shot}

    def _action(self, session_id: str, name: str, body: dict) -> dict:
        """Low-level action POST. Sends `body` verbatim — no coordinate scaling.
        The scale-aware translation lives in click/move/scroll above; this raw
        path is kept intact for callers that already have native coordinates."""
        return self._request("POST", f"/sessions/{session_id}/{name}", json=body)

    # ---- screenshots (diff-aware) ---------------------------------------

    def screenshot(
        self,
        sess: Session,
        max_width: Optional[int] = 1280,
        force_full: bool = False,
        format: str = "jpeg",
        quality: int = 75,
    ) -> "Screenshot":
        """Capture the screen and return a complete, ready-to-show frame.

        The server does the downscale + lossy encode at the source, so the image
        already arrives sized and compressed — this client only reconstructs and
        records the coordinate scale. Handles the diff protocol transparently:
          - mode "nochange": the screen is identical to what we last held; we
            return the cached frame WITHOUT re-fetching a full image. This is
            the cheap path that makes 'did my click land yet?' polling almost
            free in both bytes and model tokens.
          - mode "diff": only changed regions came back; we composite them onto
            the in-memory decoded base to reconstruct the full image.
          - mode "full": a complete frame; we cache it as the new base.

        max_width: ask the server to downscale the frame so its width <=
                   max_width before encoding. This is the primary bandwidth AND
                   token lever; keep it modest enough to stay legible. The server
                   reports native_width/native_height, from which we set
                   `scale = native_width / shown_width` on both the Screenshot
                   and the Session so a later click/move/scroll can translate
                   model (shown-space) coordinates back to native pixels (see
                   ECUClient.click). `None` means native resolution (no
                   downscale).
        format:    wire codec — "jpeg" (default, universal), "webp" (smallest),
                   or "png" (lossless escape hatch for crisp text).
        quality:   lossy quality 1..100 (ignored for png). ~75 is a good
                   legibility/size tradeoff for UI screenshots.
        force_full: ignore the cache and demand a full frame.
        """
        body: dict = {
            "mode": "full" if force_full else "auto",
            "format": format,
            "quality": quality,
        }
        # Omit max_width entirely for native res (the server treats absent as
        # "native"); otherwise pass it so the server downscales at the source.
        if max_width is not None:
            body["max_width"] = max_width
        if sess._base_token is not None and not force_full:
            # Echo back whatever token value we hold (int or str) as `since`.
            body["since"] = sess._base_token
        data = self._request("POST", f"/sessions/{sess.session_id}/screenshot", json=body)
        return self._screenshot_from_response(
            sess, data, max_width=max_width, format=format, quality=quality
        )

    def _screenshot_from_response(
        self,
        sess: Session,
        data: dict,
        max_width: Optional[int],
        format: str,
        quality: int,
    ) -> "Screenshot":
        """Turn a raw screenshot-protocol response dict into a full ``Screenshot``.

        This is the diff/nochange/full reconstruction core, factored out of
        ``screenshot()`` so the trailing screenshot returned by a batch
        ``actions()`` reconstructs IDENTICALLY: it composites a ``diff`` onto
        the session's in-memory base, returns the cached bytes on ``nochange``,
        caches a ``full`` as the new base, and derives ``scale = native_width /
        shown_width`` (recording it on the Session) — exactly as a standalone
        ``screenshot()`` would. The ``max_width``/``format``/``quality`` are the
        request encoding params, threaded through only so the no-base recovery
        path can re-fetch a matching full frame. ``data`` is the response body
        (already through ``_request``); we never issue the original request here.
        """
        mode = data.get("mode")
        frame_token = data.get("frame_token")

        if mode == "nochange":
            if sess._base_png is None:
                # We claimed to hold a frame but don't — recover by forcing full.
                return self.screenshot(
                    sess, max_width=max_width, force_full=True,
                    format=format, quality=quality,
                )
            out_png = sess._base_png
            # Keep our cached token current (the server reports the live frame).
            if frame_token is not None:
                sess._base_token = frame_token

        elif mode == "full":
            self._set_base(sess, base64.b64decode(data["image"]))
            out_png = sess._base_png
            sess._base_token = frame_token
            sess.width = int(data.get("width", sess.width))
            sess.height = int(data.get("height", sess.height))

        elif mode == "diff":
            recovered = self._apply_diff(
                sess, data, max_width=max_width, format=format, quality=quality
            )
            if recovered is not None:
                # We had no base to composite onto and re-fetched a full frame;
                # that Screenshot is already complete and correctly stamped.
                return recovered
            out_png = sess._base_png
            sess._base_token = frame_token
            sess.width = int(data.get("width", sess.width))
            sess.height = int(data.get("height", sess.height))

        else:
            raise ECUError(f"unexpected screenshot mode: {mode!r}")

        # The image already arrives at the shown size. The server reports the
        # native capture size; the scale the front-ends need to translate model
        # coordinates back to native pixels is native_width / shown_width.
        shown_w = int(data.get("width") or _png_width(out_png))
        shown_h = int(data.get("height") or _png_height(out_png))
        native_w = int(data.get("native_width") or shown_w)
        scale = (native_w / shown_w) if shown_w else 1.0
        sess.screenshot_scale = scale
        return Screenshot(
            png=out_png,
            width=shown_w,
            height=shown_h,
            mode=mode or "full",
            scale=scale,
            frame_token=sess._base_token,
        )

    def _set_base(self, sess: Session, image_bytes: bytes) -> None:
        """Adopt a server full-frame as the new base.

        The server frame may be JPEG/WebP/PNG. We decode it once to pixels (the
        in-memory base future diffs paste onto) and normalize the cached/returned
        bytes to *PNG* so ``Screenshot.png`` has a single, correctly-labeled
        format regardless of the wire codec, and so the sidecar ``.png`` cache is
        genuinely a PNG. This is a single lossless re-encode of the frame the
        server already sent — it adds no compression damage and never repeats on
        unchanged content. Without Pillow we cannot decode, so we keep the raw
        bytes as-is (full-frame-only, no diffing) — see _HAVE_PIL guards."""
        if _HAVE_PIL:
            base = Image.open(io.BytesIO(image_bytes)).convert("RGB")
            sess._base_image = base
            buf = io.BytesIO()
            base.save(buf, format="PNG")
            sess._base_png = buf.getvalue()
        else:
            sess._base_image = None
            sess._base_png = image_bytes

    def _apply_diff(
        self,
        sess: Session,
        data: dict,
        max_width: Optional[int],
        format: str,
        quality: int,
    ) -> Optional["Screenshot"]:
        """Composite a ``diff`` onto the in-memory decoded base, then refresh the
        cached PNG bytes from those pixels.

        Only the changed regions are pasted; the rest of the frame keeps its
        previously-decoded pixels and is never run back through the lossy codec,
        so repeated diffs do not accumulate compression damage. The cached bytes
        are re-encoded as *PNG* (lossless), so the persisted/returned base adds
        no further loss on top of what the server already sent.

        Returns ``None`` on the normal path. If there is no base to composite
        onto (we lost it), it re-fetches a full frame and returns that complete
        Screenshot so the caller can hand it back directly."""
        if not _HAVE_PIL:
            raise ECUError(
                "screenshot diff reconstruction needs Pillow. "
                "Install it (`pip install Pillow`) or the control plane can be "
                "asked for full frames only."
            )
        base = sess._base_image
        if base is None:
            if sess._base_png is None:
                # No base to composite onto — force a full frame instead, with
                # the SAME encoding so the recovered frame matches the request.
                return self.screenshot(
                    sess, max_width=max_width, force_full=True,
                    format=format, quality=quality,
                )
            # Bytes but no decoded image (e.g. freshly hydrated from cache):
            # decode once, then composite onto those pixels.
            base = Image.open(io.BytesIO(sess._base_png)).convert("RGB")
        for region in data.get("regions", []):
            patch = Image.open(
                io.BytesIO(base64.b64decode(region["image"]))
            ).convert("RGB")
            base.paste(patch, (int(region["x"]), int(region["y"])))
        sess._base_image = base
        buf = io.BytesIO()
        base.save(buf, format="PNG")
        sess._base_png = buf.getvalue()
        return None


@dataclass
class Screenshot:
    png: bytes
    width: int          # shown width (after any downscale)
    height: int         # shown height (after any downscale)
    mode: str           # "full" | "diff" | "nochange" — informational
    # scale = native_width / shown_width for this frame (1.0 when shown at native
    # resolution). Persist it (CLI sidecar) / read it (MCP) so a subsequent
    # click/move/scroll can translate model (shown-space) coords to native.
    scale: float = 1.0
    # The frame_token this image corresponds to (int on the wire). Useful for
    # the "nochange" sentinel and for debugging which frame the model is seeing.
    frame_token: Optional[Union[int, str]] = None

    def b64(self) -> str:
        return base64.b64encode(self.png).decode("ascii")

    def save(self, path: str) -> str:
        with open(path, "wb") as f:
            f.write(self.png)
        return path


# ---- coordinate / image helpers -----------------------------------------

def _scale_xy(x, y, scale: float):
    """Translate a shown-space (x, y) anchor to native-space pixels.

    `scale = native_width / shown_width` (>= 1.0 after a downscale). A scale of
    1.0 returns the inputs unchanged. Returns ints (the wire expects integers).
    """
    if scale == 1.0:
        return x, y
    return round(x * scale), round(y * scale)


def _png_width(png: bytes) -> int:
    """Width in pixels of an encoded image, or 0 if unmeasurable (no Pillow)."""
    if not _HAVE_PIL:
        return 0
    return Image.open(io.BytesIO(png)).width


def _png_height(png: bytes) -> int:
    """Height in pixels of an encoded image, or 0 if unmeasurable (no Pillow)."""
    if not _HAVE_PIL:
        return 0
    return Image.open(io.BytesIO(png)).height


# ---- CLI sidecar cache ---------------------------------------------------
#
# The CLI runs as a fresh process per invocation, so without a sidecar it can
# never send `since` (every screenshot comes back `full` — no diff/nochange
# savings) and has nowhere to remember the downscale `scale` needed to translate
# click coordinates. This small on-disk cache, keyed by session id, persists the
# last frame_token, the reconstructed base PNG, the scale factor, and the native
# width/height. Both front-ends share THIS one implementation.
#
# Layout (honoring $XDG_CACHE_HOME, else ~/.cache):
#   <cache>/ecu/<session>.json   token + scale + native width/height
#   <cache>/ecu/<session>.png    the reconstructed base frame
#
# Loading is best-effort: any error (missing/corrupt file, unreadable PNG)
# yields a cold Session (no base, no token) so the next screenshot is a clean
# full frame. The cache must never crash a command.


def session_cache_dir() -> Path:
    """Directory holding the sidecar cache, honoring $XDG_CACHE_HOME."""
    base = os.environ.get("XDG_CACHE_HOME") or os.path.join(
        os.path.expanduser("~"), ".cache"
    )
    return Path(base) / "ecu"


def _cache_paths(session_id: str) -> tuple[Path, Path]:
    d = session_cache_dir()
    return d / f"{session_id}.json", d / f"{session_id}.png"


def load_session_cache(session_id: str) -> Session:
    """Hydrate a Session from the sidecar cache, or return a cold one.

    Sets `_base_png`, `_base_token`, `screenshot_scale`, and width/height from
    disk when available so the next `screenshot()` can send `since` (cheap
    diff/nochange path) and click/move/scroll can scale coordinates. On ANY
    load error this returns a cold Session (status "ready", no base/token) so a
    bad or absent cache never breaks the command — it just costs a full frame.
    """
    sess = Session(session_id=session_id, status="ready")
    json_path, png_path = _cache_paths(session_id)
    try:
        meta = json.loads(json_path.read_text())
        sess._base_token = meta.get("frame_token")
        sess.screenshot_scale = float(meta.get("scale", 1.0))
        sess.width = int(meta.get("width", 0))
        sess.height = int(meta.get("height", 0))
        if png_path.exists():
            sess._base_png = png_path.read_bytes()
        else:
            # Token without a base frame is unusable; force a cold full frame.
            sess._base_token = None
    except Exception:
        # Missing or corrupt cache — fall back to cold (full-frame) behavior.
        return Session(session_id=session_id, status="ready")
    return sess


def save_session_cache(session_id: str, sess: Session) -> None:
    """Persist a Session's base frame + token + scale + native dims to disk.

    Best-effort: creates the cache dir if missing and ignores write errors (a
    failed cache write must not break the command — it only forfeits the cheap
    path next time).
    """
    json_path, png_path = _cache_paths(session_id)
    try:
        json_path.parent.mkdir(parents=True, exist_ok=True)
        if sess._base_png is not None:
            png_path.write_bytes(sess._base_png)
        meta = {
            "frame_token": sess._base_token,
            "scale": sess.screenshot_scale,
            "width": sess.width,
            "height": sess.height,
        }
        json_path.write_text(json.dumps(meta))
    except Exception:
        # Caching is an optimization; never fail the caller because of it.
        pass


def _safe_detail(resp) -> str:
    try:
        j = resp.json()
        return str(j.get("detail") or j.get("error") or j)
    except Exception:
        return resp.text[:200]
