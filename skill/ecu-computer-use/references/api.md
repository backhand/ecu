# ECU Control-Plane API Reference (as built)

This documents the ECU (Easy Computer Use) control-plane HTTP API that the
clients (`mcp_server.py`, `ecu_cli.py`) talk to. You normally don't call it
directly — use the MCP tools or the CLI. This is here for debugging and for
anyone building an alternate client. It matches what the control plane actually
implements.

All requests require:

```
Authorization: Bearer <ECU_API_KEY>
```

Base URL is `ECU_URL` (e.g. `https://ecu.example.com`). All bodies are JSON. The
control plane proxies actions and screenshots to the session's instance over a
reverse tunnel; the instance's address is never exposed to the client.

## Session lifecycle

Status values: `provisioning | ready | error | terminated | stopped`.

- `provisioning` — instance booting / awaiting its reverse tunnel.
- `ready` — usable; actions and screenshots are accepted.
- `error` — provisioning failed; any instance has been torn down.
- `terminated` — ended and destroyed (ephemeral end, or a persistent session
  aged out).
- `stopped` — a **persistent** session that was snapshotted and had its instance
  destroyed; restorable via `POST /sessions {"restore": "<id>"}`.

### POST /sessions
Provision (or restore) a computer.

Request (all fields optional; an empty body is valid):
```json
{ "persistent": false, "restore": null }
```
- `persistent` (bool): request a persistent session. **Privileged** — the API
  key must be authorized; otherwise `403` (REJECTED, never silently downgraded
  to ephemeral). Bounded by `ECU_MAX_PERSISTENT_SESSIONS` (`429`).
- `restore` (string): a prior **stopped** persistent `session_id` (owned by this
  key) to reactivate, booting a fresh instance from its saved snapshot and
  reusing the same id. Also privileged (same `403`). The named session must
  exist, be owned by the caller, and be a stopped persistent session, else `404`
  / `409` (see Errors).

Response (`200`):
```json
{ "session_id": "s_abc123", "status": "provisioning",
  "persistent": false, "width": 1280, "height": 800 }
```
A new ephemeral or persistent session starts `provisioning`; poll
`GET /sessions/{id}` until `ready`. (In the dev tool-server seam it returns
`ready` immediately.) The default desktop is 1280x800.

> Dev-only: when the server is built with `ECU_DEV_EXPOSE_TUNNEL_TOKEN=1`, the
> response also carries `tunnel_token` / `tunnel_url`. Production clients never
> see these (they are omitted entirely).

### GET /sessions/{session_id}
Status. Poll until `status` is `ready`.

Response (`200`):
```json
{ "status": "ready", "width": 1280, "height": 800,
  "watch_url": "https://ecu.example.com/sessions/s_abc123/watch?token=..." }
```
- `watch_url` is a fresh, short-lived (~minutes) human watch link. It is present
  **only when `status == "ready"`** and a public base URL is configured;
  otherwise it is `null`. Each status poll mints a new token.
- For an `error` session the body carries a `detail` string. (`width`/`height`
  are always present; `watch_url` is `null` for non-ready states.)

Unknown id → `404`.

### DELETE /sessions/{session_id}
Tear down. Behavior depends on the session:
- **Ephemeral** (or persistent with no live instance): marked `terminated` and
  the instance (if any) destroyed. Response `{"status":"terminated"}`.
- **Persistent with a live instance**: **snapshot-and-stop** — the instance is
  snapshotted, then destroyed, and the session becomes `stopped` (restorable).
  Response `{"status":"stopped"}`. If the snapshot fails the control plane
  *preserves state*: it returns `500` and leaves the instance and session
  exactly as they were (nothing destroyed) so no saved work is lost — retry.
- A second DELETE of an already-`stopped` session is idempotent: it does **not**
  re-snapshot and just returns `{"status":"stopped"}`.

Unknown id → `404`. Always call DELETE when finished, even on error.

## Actions

All actions are `POST /sessions/{session_id}/{action}`. The control plane
forwards the body verbatim to the instance's tool server and copies its status
and body back. On success the tool server returns `{"ok": true}` (a `200`);
`exec` returns a result object (below). Coordinates are in **native screen
pixels** (the clients translate downscaled-screenshot coordinates to native
before sending — see the screenshot protocol). The single-action names below
plus the `actions` batch endpoint (which runs several of them in one exchange,
documented after this table) make up the forwarded action surface; any other
`{action}` segment is rejected `400 unknown action`.

| Action   | Body                                  | Notes |
|----------|---------------------------------------|-------|
| `click`  | `{ "x", "y", "button" }`              | button: `left`/`right`/`middle` (default `left`) |
| `move`   | `{ "x", "y" }`                        | move cursor, no click |
| `type`   | `{ "text" }`                          | type a string at the focus |
| `key`    | `{ "keys": "ctrl+l" }`                | key/chord (xdotool syntax: `Return`, `ctrl+l`, `alt+F4`) |
| `scroll` | `{ "x", "y", "dx", "dy" }`            | anchor `(x,y)`; `+dy` down / `-dy` up, `+dx` right / `-dx` left; magnitudes are scroll clicks |
| `exec`   | `{ "command", "timeout"? }`           | runs `sh -c <command>`; see below |

`exec` response:
```json
{ "stdout": "...", "stderr": "...", "code": 0 }
```
`timeout` (seconds, default 120) bounds the run; on timeout `code` is `124` and
the timeout note is appended to `stderr`. `exec` is a one-shot `sh -c` (no
persistent shell session); background long-running things yourself
(`... >/dev/null 2>&1 &`).

A tool-level failure (e.g. the X display isn't up) comes back as the tool
server's `{ "ok": false, "error": "..." }` with a `5xx` status, copied through.

### POST /sessions/{session_id}/actions
Batch/macro endpoint: run an ordered list of actions **plus** an optional
trailing screenshot server-side in a **single** exchange, instead of paying the
reverse-tunnel round-trip for each step. Collapses a common GUI sequence (click
address bar → type URL → Enter → screenshot, normally 4 requests) into one.

Request:
```json
{ "actions": [ {"action":"click","x":100,"y":50},
               {"action":"type","text":"mailon.ai"},
               {"action":"key","keys":"Return"} ],
  "screenshot": {"format":"jpeg","quality":75,"max_width":1280,"since":123} }
```
- `actions` (array, required): each item is `{ "action": "<name>", ...}` where
  `action` is one of `click`/`move`/`type`/`key`/`scroll`/`exec` and the
  remaining fields are **exactly** that single action's body (from the table
  above): `click` `{x,y,button?}`, `move` `{x,y}`, `type` `{text}`, `key`
  `{keys}`, `scroll` `{x,y,dx?,dy?}`, `exec` `{command,timeout?}`. Coordinates
  are native screen pixels (the client scales each anchor before sending, same
  as the single-action path). A zero-`dx`/`dy` `scroll` is an accepted no-op.
- `screenshot` (object, optional): a trailing-screenshot request mirroring the
  `POST /screenshot` body (`since`, `mode`, `max_width`, `format`, `quality`,
  plus the capture-once-stable `settle` / `settle_ms` / `max_wait_ms` — all
  optional, same defaults and semantics as `POST /screenshot`). It is captured
  **only if every action succeeded**. Put `settle_ms` (or `settle: true`) here
  to make the trailing frame a **settled** capture (see below).

Execution + error policy (**in order, stop on first error**): the actions run
sequentially. If an action fails — a `ToolActionError` (bad key name, display
down), an **unknown `action` value**, a **missing/ill-typed required field**, or
an **unknown button** — that action's result is recorded as
`{ "ok": false, "error": "<message>" }`, the batch **stops immediately** (the
remaining actions do **not** run), and the trailing screenshot is **skipped**.
A bad item is a recorded per-action error, never a `422` that rejects the whole
batch.

Response:
```json
{ "results": [ {"ok":true}, {"ok":true}, {"ok":true} ],
  "screenshot": { "mode":"full", "frame_token":13, "width":1280, "height":800,
                  "native_width":1280, "native_height":800, "image":"<base64>" } }
```
- `results` (array): one entry per action that ran, in order. A successful GUI
  action is `{ "ok": true }`; a successful `exec` is its `{ "stdout", "stderr",
  "code" }` result object; a failed action is `{ "ok": false, "error": "..." }`
  and is the **last** entry (the batch stopped there). On failure the list
  length equals *(failed index + 1)*.
- `screenshot` (object): present **only** when a trailing `screenshot` was
  requested **and** all actions succeeded. Its value is **exactly** a
  screenshot-protocol response (`full`/`diff`/`nochange`, same shape as
  `POST /screenshot` returns — see below); the client reconstructs it into a
  full frame the same way. When any action failed, this key is **absent**.

By default the trailing screenshot is captured **immediately** after the last
action with **no settle/sleep**, so it may show the screen *before* the last
action's effect has fully rendered. To get a stable in-batch frame, pass
`settle_ms` (or `settle: true`) in the trailing `screenshot` dict — the server
then re-captures until the screen is unchanged for that window before returning
(capped by `max_wait_ms`, default ~2500 ms, so it never hangs). See the
capture-once-stable params under `POST /screenshot`. (Without settle, if the
in-batch frame looks pre-render, take a follow-up `POST /screenshot` to confirm.)

## Screenshot protocol (diff-aware, downscaled + lossy at the source)

### POST /sessions/{session_id}/screenshot
Request:
```json
{ "since": 12, "mode": "auto", "max_width": 1280, "format": "jpeg", "quality": 75 }
```
- `since` (integer, optional): the `frame_token` the caller currently holds.
  **Frame tokens are integers** (a monotonic per-instance counter), not opaque
  strings. Omit `since` on the first capture.
- `mode` (string, optional): `auto` (default — the server decides
  full/diff/nochange) or `full` (force a complete frame). Omitting it is treated
  as `auto`.
- `max_width` (integer, optional): the server **downscales the captured frame to
  this width** before tiling/diffing/encoding, so the wire carries the shown
  (smaller) image and never the full-res original. Omit it (or send the native
  width) for no downscale. **A `max_width` that differs from the size of the
  base the server currently holds forces a `full`** (the diff base is the
  downscaled frame, so a different shown size can't be tile-diffed).
- `format` (string, optional): wire codec — `jpeg` (default, universal), `webp`
  (smallest, ~25–35% under JPEG where the decoder supports it), or `png`
  (lossless escape hatch for crisp text — larger). Applied to the full frame
  **and** every diff region.
- `quality` (integer 1–100, optional): lossy quality (default `75`; ignored for
  `png`). ~75 balances legibility and size for UI screenshots.
- `settle` (bool, optional): **capture-once-stable.** Default **OFF** (omitted =
  capture immediately, today's behavior — one grab, no extra latency). When
  `true`, the server re-captures until the screen has been visually unchanged
  for the settle window, then returns that **settled** frame instead of a
  possibly mid-render one (a just-focused window before its content paints, a
  page still loading). `settle: true` with no `settle_ms` uses a ~300 ms window.
- `settle_ms` (integer ≥ 0, optional): the settle window in milliseconds
  (implies settle). The screen must be unchanged for this long — compared
  between **consecutive captures** on the same 64px tile grid the diff protocol
  uses — to count as settled. `0`/omitted = settle OFF.
- `max_wait_ms` (integer > 0, optional): hard cap on the **total** settle wait
  (default ~2500 ms). An endlessly-animating screen (spinner, video, redraw
  loop) returns the **latest** frame at the cap and **never hangs**. The cap is
  checked every poll; it is floored at the settle window.

When settle is on, the returned (settled or capped) frame still goes through the
normal **full / diff / nochange** decision and the **downscale + lossy** encode,
exactly like a non-settle capture — settle only changes *which* frame is
resolved, never the response shape. (Settle compares consecutive captures to ask
"is the screen still moving?"; the full/diff/nochange decision separately
compares the final frame against the caller's `since` base.) The clients bump the
per-request HTTP timeout when a large `max_wait_ms` is asked for, so the wait
can't trip the client before the server responds.

The defaults make a bare `{}` request return a lossy JPEG full-res frame
(~20–60 KB) instead of the old ~1 MB PNG, captured immediately (no settle).

Three response shapes, distinguished by `mode`. All non-`nochange` shapes carry
the shown `width`/`height` (the downscaled size the images are in) **and** the
real captured `native_width`/`native_height` (see "Coordinates" below):

**No change** — the screen is pixel-identical to `since`:
```json
{ "mode": "nochange", "frame_token": 12 }
```
The caller keeps showing its cached frame; the token is unchanged. Cheapest
path — use it freely when polling. (On the MCP front-end this surfaces as a
short text note instead of re-sending the image; the CLI re-saves its cached
frame.)

**Diff** — only changed regions are returned, in the **shown** (downscaled)
pixel space:
```json
{ "mode": "diff", "frame_token": 13, "base_token": 12,
  "width": 1280, "height": 800, "native_width": 1280, "native_height": 800,
  "regions": [ { "x":100, "y":100, "w":120, "h":90, "image":"<base64 image>" } ] }
```
The caller composites each region (a small lossy image in the requested
`format`) onto its cached base frame at `(x,y)` to reconstruct the complete
image. `base_token` is the frame the diff is against; it equals the `since` the
caller sent. If a caller's base ever fails to match, request `mode:"full"`.

**Full** — a complete frame (first capture, forced `mode:"full"`, a `since` that
doesn't match the server's current base, a resolution **or `max_width`** change,
or when a diff would be ≥ a full frame, e.g. a page transition):
```json
{ "mode": "full", "frame_token": 13,
  "width":1280, "height":800, "native_width":1280, "native_height":800,
  "image":"<base64 image>" }
```

How the server decides (single base frame per instance): a forced full / first
frame / `since` mismatch / `max_width` differing from the held base's width →
`full`; otherwise it downscales the new frame to `max_width` and compares it
against the (also downscaled) base on a 64px tile grid — no tile changed →
`nochange`; ≥ ~90% of tiles changed, or the diff region images together weigh ≥
the full-frame image → `full`; else → `diff` of the changed regions. The
full-fallback byte rule is evaluated on the **lossy** sizes (regions and full
encoded with the same `format`/`quality`), so it stays apples-to-apples.

**Coordinates.** Region coordinates and the `width`/`height` are in the **shown
image's pixel space** (post-downscale) — exactly the space the client
composites and the model clicks in. `native_width`/`native_height` report the
real captured desktop size. The client records `scale = native_width /
width` and multiplies model-supplied (shown-space) click/move/scroll anchors by
it before sending them to the action endpoints, so a click at `max_width=640` on
a 1280-native desktop sends `(2x, 2y)`. Scroll **deltas** (`dx`/`dy`) are scroll
clicks, not coordinates, and are never scaled.

Important: the **client always reconstructs a full frame before showing it to a
model** — a vision model cannot apply a diff. The reconstructed base is held as
**decoded pixels in memory**; only changed regions are pasted onto it between
diffs, so unchanged content is never re-run through the lossy codec and repeated
diffs do not accumulate compression damage. Diffing + downscaling + lossy
encoding are purely wire/latency/token optimizations; the model only ever sees a
complete still.

## Live watch (human oversight)

### GET /sessions/{session_id}/watch
A live noVNC view of the desktop for a human to watch, gated by a short-lived
view token (the `token` query param from `watch_url` in session status; the
handler also sets a scoped cookie so subsequent asset/WebSocket requests carry
auth). It is proxied through the control plane over the session's reverse tunnel
— the instance has no public inbound ports. This is **separate** from the
screenshot perception path (it shares no framing state) and is for human
oversight only; agents perceive the screen with `screenshot`. This route is
browser-facing and is **not** behind API-key auth — it is gated by the view
token/cookie instead.

## Errors

Standard HTTP status codes with a JSON body `{ "detail": "..." }` (control
plane) or `{ "ok": false, "error": "..." }` (tool-server-originated):

- `400` — malformed JSON body, or an unknown action segment
  (`unknown action: <name>`).
- `401` — missing/malformed `Authorization`, or an unknown/disabled API key.
- `403` — persistence not authorized for this key. Exact detail:
  `persistence not authorized for this API key`. Sent for a `persistent: true`
  create OR a `restore` from a key without the capability. The request is
  rejected, never downgraded.
- `404` — unknown session (also returned when restoring/accessing a session not
  owned by your key, so ids can't be probed across accounts).
- `409` — session not in a usable state: `session is not ready` (acting on a
  non-`ready` session), or `session is not a restorable stopped persistent
  session` (restoring something that isn't a stopped persistent session you
  own).
- `429` — capacity cap reached. Two distinct messages:
  `session cap reached: N active sessions (max M)` (the `ECU_MAX_SESSIONS`
  active cap) or `persistent session cap reached: ...` (the
  `ECU_MAX_PERSISTENT_SESSIONS` cap). Back off or end an existing session.
- `500` — control-plane error, or a persistent DELETE whose snapshot failed
  (state preserved — retry).
- `502` — the session's tool server is unreachable (`tool server unreachable`).
- `5xx` with `{ "ok": false, "error": ... }` — a tool-level failure forwarded
  from the instance (e.g. the X display isn't up yet). Safe to retry with
  backoff.
