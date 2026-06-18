"""Live-watch reverse proxy: serve the container's noVNC client under ``/watch``.

This is the tool-server half of ECU Component 9 (the human-watchable live VNC
view). The container already runs noVNC + websockify on :6080 (the noVNC web
client plus a ``/websockify`` WebSocket that bridges to the VNC server on :5900),
but the reverse tunnel only reaches the tool server on :8000 -- it has no route
to :6080. So the tool server reverse-proxies noVNC under a ``/watch`` prefix,
making the whole viewer (HTML page, JS/CSS assets, AND the websockify WebSocket)
reachable through the single tunnel target.

Why a *prefix* proxy works for the assets:

  noVNC's ``vnc.html`` references every asset with a RELATIVE URL (``app/...``,
  ``app/styles/base.css``, ...). When the page is entered at ``/watch/vnc.html``
  the browser resolves those against ``/watch/``, i.e. ``/watch/app/...``. We
  strip the ``/watch`` prefix and forward to noVNC on :6080, so ``/watch/app/ui.js``
  -> ``:6080/app/ui.js`` and the page loads intact.

Why the WebSocket needs a query parameter:

  noVNC builds the websockify URL as ``ws(s)://<host>:<port>/<path>`` where
  ``host``/``port`` default to the page's own location but ``path`` defaults to
  the bare ``websockify`` (no prefix). So out of the box the client would dial
  ``/websockify`` at the page host, NOT ``/watch/websockify`` -- wrong through a
  prefix. noVNC reads ``path`` from the query string (``?path=...``), so the
  ``/watch`` entry point redirects to ``vnc.html`` with ``path`` set to the
  websockify route RELATIVE to the page host (e.g. ``sessions/<id>/watch/websockify``
  through the control plane, or ``watch/websockify`` when hit directly on :8000).
  The redirect computes that prefix from the request path itself, so it is
  agnostic to whatever prefix it is mounted under (``/watch`` on :8000 OR
  ``/sessions/<id>/watch`` through the tunneled control plane).

This path is deliberately SEPARATE from ``/screenshot``: the agent perception
loop stays discrete stills; this is a streaming video feed for human eyeballs and
shares nothing with the screenshot framing state.
"""

from __future__ import annotations

import asyncio
from urllib.parse import urlencode

import httpx
import websockets
from starlette.requests import Request
from starlette.responses import RedirectResponse, Response, StreamingResponse
from starlette.websockets import WebSocket, WebSocketDisconnect

# The prefix the viewer is served under on the tool server. The control plane
# proxies its own ``/sessions/{id}/watch`` to this, so end to end the browser
# sees ``/sessions/{id}/watch`` and the tool server sees ``/watch``.
WATCH_PREFIX = "/watch"

# The websockify route, relative to WATCH_PREFIX. noVNC connects its RFB
# WebSocket here (the bridge to the VNC server on :5900).
WEBSOCKIFY_SEGMENT = "websockify"

# Hop-by-hop headers (RFC 7230 6.1) that must NOT be forwarded across a proxy.
# httpx/uvicorn manage framing themselves, so copying these through corrupts the
# response (e.g. a forwarded Content-Length fighting chunked transfer).
_HOP_BY_HOP = frozenset(
    h.lower()
    for h in (
        "connection",
        "keep-alive",
        "proxy-authenticate",
        "proxy-authorization",
        "te",
        "trailers",
        "transfer-encoding",
        "upgrade",
        "content-length",
        "content-encoding",
    )
)


def _novnc_base(port: int) -> str:
    return f"http://localhost:{port}"


def _watch_prefix_from_path(path: str) -> str:
    """Return the request path up to and including the ``/watch`` segment.

    Used to compute the websockify ``path`` query for the entry redirect so the
    viewer self-configures regardless of the prefix it is mounted under. For
    ``/watch`` -> ``/watch``; for ``/sessions/s_abc/watch`` (the control-plane
    facing path, which the CP forwards verbatim) -> ``/sessions/s_abc/watch``.
    Falls back to WATCH_PREFIX if ``/watch`` is somehow absent.
    """
    marker = "/watch"
    idx = path.rfind(marker)
    if idx == -1:
        return WATCH_PREFIX
    return path[: idx + len(marker)]


def register_watch_routes(app, novnc_port: int) -> None:
    """Wire the ``/watch`` HTTP proxy, entry redirect, and websockify bridge.

    Registered on the given FastAPI/Starlette ``app``:
      * ``GET /watch`` and ``GET /watch/`` -> 302 to ``vnc.html`` with
        autoconnect + the websockify ``path`` (so the page actually connects).
      * ``WS  /watch/websockify``          -> bridged to noVNC's websockify.
      * ``GET /watch/{rest}``              -> strip-prefix reverse proxy to noVNC.

    The websockify WS route is registered BEFORE the catch-all so the upgrade is
    not swallowed by the HTTP proxy. Other tool-server endpoints are untouched.
    """

    # A single pooled async client for all proxied HTTP asset/page fetches. noVNC
    # serves small static files, so default limits are ample.
    client = httpx.AsyncClient(base_url=_novnc_base(novnc_port), timeout=30.0)

    @app.on_event("shutdown")
    async def _close_watch_client() -> None:  # pragma: no cover - lifecycle
        await client.aclose()

    def _entry_redirect(request: Request) -> RedirectResponse:
        """302 to vnc.html with autoconnect + a prefix-correct websockify path."""
        prefix = _watch_prefix_from_path(request.url.path)
        # noVNC builds ws(s)://host:port/<path>; it wants the path WITHOUT a
        # leading slash, relative to the page host. Strip the leading slash.
        ws_path = (prefix + "/" + WEBSOCKIFY_SEGMENT).lstrip("/")
        query = urlencode(
            {
                "autoconnect": "true",
                "resize": "scale",
                "path": ws_path,
                "reconnect": "true",
            }
        )
        # Target is relative to the prefix so it works behind the control plane
        # too (the browser keeps the public host/scheme).
        return RedirectResponse(url=f"{prefix}/vnc.html?{query}", status_code=302)

    @app.get(WATCH_PREFIX)
    async def watch_root(request: Request) -> RedirectResponse:
        return _entry_redirect(request)

    @app.get(WATCH_PREFIX + "/")
    async def watch_root_slash(request: Request) -> RedirectResponse:
        return _entry_redirect(request)

    @app.websocket(WATCH_PREFIX + "/" + WEBSOCKIFY_SEGMENT)
    async def watch_websockify(ws: WebSocket) -> None:
        """Bridge the browser's RFB WebSocket to noVNC's websockify on :6080.

        noVNC speaks the ``binary`` subprotocol; we echo whichever subprotocol the
        client offered back on accept (websockify expects ``binary``), connect to
        the upstream websockify, and pump frames both directions until either side
        closes. Bytes are opaque (RFB), so we never parse them.
        """
        # websockify negotiates the "binary" subprotocol; accept the one the
        # client offered so the RFB handshake matches.
        subprotocols = ws.scope.get("subprotocols") or []
        chosen = subprotocols[0] if subprotocols else None
        await ws.accept(subprotocol=chosen)

        upstream_url = f"ws://localhost:{novnc_port}/{WEBSOCKIFY_SEGMENT}"
        try:
            upstream = await websockets.connect(
                upstream_url,
                subprotocols=["binary"],
                # RFB frames are small but the limit must not truncate; disable it
                # to mirror the tunnel's disabled WS frame-size limit (C3).
                max_size=None,
                open_timeout=10,
            )
        except Exception:
            await ws.close(code=1011)
            return

        async def client_to_upstream() -> None:
            try:
                while True:
                    msg = await ws.receive()
                    if msg["type"] == "websocket.disconnect":
                        break
                    if (data := msg.get("bytes")) is not None:
                        await upstream.send(data)
                    elif (text := msg.get("text")) is not None:
                        await upstream.send(text)
            except (WebSocketDisconnect, websockets.ConnectionClosed):
                pass
            finally:
                await upstream.close()

        async def upstream_to_client() -> None:
            try:
                async for message in upstream:
                    if isinstance(message, (bytes, bytearray)):
                        await ws.send_bytes(bytes(message))
                    else:
                        await ws.send_text(message)
            except (websockets.ConnectionClosed, WebSocketDisconnect):
                pass
            finally:
                try:
                    await ws.close()
                except RuntimeError:
                    pass  # already closed

        await asyncio.gather(
            client_to_upstream(), upstream_to_client(), return_exceptions=True
        )

    @app.get(WATCH_PREFIX + "/{rest:path}")
    async def watch_proxy(request: Request, rest: str) -> Response:
        """Strip-prefix reverse proxy for the noVNC page and its assets.

        ``/watch/<rest>`` -> ``:6080/<rest>``. The body is streamed so larger
        assets don't buffer in memory; hop-by-hop headers are dropped.
        """
        # Preserve the query string (noVNC's vnc.html reads ?autoconnect/?path).
        upstream_path = "/" + rest
        upstream_req = client.build_request(
            "GET",
            upstream_path,
            params=dict(request.query_params),
        )
        try:
            upstream_resp = await client.send(upstream_req, stream=True)
        except httpx.HTTPError:
            return Response(content=b"noVNC upstream unreachable", status_code=502)

        headers = {
            k: v
            for k, v in upstream_resp.headers.items()
            if k.lower() not in _HOP_BY_HOP
        }

        async def body_iter():
            try:
                async for chunk in upstream_resp.aiter_raw():
                    yield chunk
            finally:
                await upstream_resp.aclose()

        return StreamingResponse(
            body_iter(),
            status_code=upstream_resp.status_code,
            headers=headers,
            media_type=upstream_resp.headers.get("content-type"),
        )
