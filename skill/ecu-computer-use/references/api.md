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
before sending — see the screenshot protocol).

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

## Screenshot protocol (diff-aware)

### POST /sessions/{session_id}/screenshot
Request:
```json
{ "since": 12, "mode": "auto" }
```
- `since` (integer, optional): the `frame_token` the caller currently holds.
  **Frame tokens are integers** (a monotonic per-instance counter), not opaque
  strings. Omit `since` on the first capture.
- `mode` (string, optional): `auto` (default — the server decides
  full/diff/nochange) or `full` (force a complete frame). Omitting it is treated
  as `auto`.

Three response shapes, distinguished by `mode`:

**No change** — the screen is pixel-identical to `since`:
```json
{ "mode": "nochange", "frame_token": 12 }
```
The caller keeps showing its cached frame; the token is unchanged. Cheapest
path — use it freely when polling. (On the MCP front-end this surfaces as a
short text note instead of re-sending the image; the CLI re-saves its cached
frame.)

**Diff** — only changed regions are returned:
```json
{ "mode": "diff", "frame_token": 13, "base_token": 12,
  "width": 1280, "height": 800,
  "regions": [ { "x":100, "y":100, "w":120, "h":90, "image":"<base64 png>" } ] }
```
The caller composites each region (a small PNG) onto its cached base frame at
`(x,y)` to reconstruct the complete image. `base_token` is the frame the diff is
against; it equals the `since` the caller sent. If a caller's base ever fails to
match, request `mode:"full"`.

**Full** — a complete frame (first capture, forced `mode:"full"`, a `since` that
doesn't match the server's current base, a resolution change, or when a diff
would be ≥ a full frame, e.g. a page transition):
```json
{ "mode": "full", "frame_token": 13, "width":1280, "height":800,
  "image":"<base64 png>" }
```

How the server decides (single base frame per instance): a forced full / first
frame / `since` mismatch → `full`; otherwise it compares the new frame against
the base on a 64px tile grid — no tile changed → `nochange`; ≥ ~90% of tiles
changed, or the diff region PNGs together weigh ≥ the full-frame PNG → `full`;
else → `diff` of the changed regions.

Important: the **client always reconstructs a full frame before showing it to a
model** — a vision model cannot apply a diff. Diffing is purely a
wire/latency/token optimization. The client then optionally **downscales** the
reconstructed frame to `max_width` (the main lever for reducing model token
cost) and records the scale factor so subsequent click/move/scroll coordinates,
given in the downscaled space, are translated back to native pixels.

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
