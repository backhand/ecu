# ECU computer-use instance image

The disposable computer-use container for ECU. It builds on the
[anthropic computer-use-demo image][demo] and exposes that demo's **existing**
computer-use tools (`ComputerTool`, `BashTool` in `computer_use_demo/tools/`) as
a small localhost-bound HTTP API, replacing the demo's Streamlit chat UI and
agent loop. The Xfce-style desktop (Xvfb + tint2 + mutter), X11, VNC and noVNC
all keep running -- the tool server needs the X display, and noVNC is reused by
the future `/watch` live view.

This is **Component 1** of ECU. The control plane, tunnel agent and skill live
elsewhere in the repo; this directory is just the image.

[demo]: https://github.com/anthropics/anthropic-quickstarts/tree/main/computer-use-demo

## What's inside

```
image/
├── Dockerfile          FROM the demo image; installs FastAPI/uvicorn, our app + entrypoint
├── entrypoint.sh       runs the demo desktop/VNC/noVNC startup, then launches uvicorn (not Streamlit)
├── toolserver/         the FastAPI app
│   ├── app.py          endpoints; wraps the demo's ComputerTool, plain subprocess for /exec
│   ├── __main__.py     `python -m toolserver` convenience launcher
│   └── __init__.py
└── README.md
```

The tool server **wraps, never reimplements**, the demo's input/action logic.
Every GUI action is forwarded to `ComputerTool` (the newest concrete class in
the image, `ComputerTool20251124`, driving `xdotool`/`scrot`). Only `/exec`
(plain `sh -c`) and `/screenshot` framing add local logic.

## Endpoint contract

| Method & path     | Body                          | Response |
|-------------------|-------------------------------|----------|
| `POST /screenshot`| `{since?, mode?}`             | `nochange` / `diff` / `full` (see "Screenshot protocol" below) |
| `POST /click`     | `{x,y,button}` (left/right/middle) | `{"ok":true}` |
| `POST /move`      | `{x,y}`                       | `{"ok":true}` |
| `POST /type`      | `{text}`                      | `{"ok":true}` |
| `POST /key`       | `{keys}` e.g. `"ctrl+l"`, `"Return"` (xdotool syntax) | `{"ok":true}` |
| `POST /scroll`    | `{x,y,dx,dy}`                 | `{"ok":true}` |
| `POST /exec`      | `{command, timeout?}`         | `{"stdout":...,"stderr":...,"code":N}` |
| `GET  /healthz`   | --                            | `{"ok":true}` (200; 503 until the X display is up) |
| `GET  /watch`     | --                            | 302 redirect to the local noVNC client |

Notes:

- **`/screenshot`** is diff-aware (Component 6) — see the dedicated section
  below. `width`/`height` are the decoded frame dimensions (equal to
  `WIDTH`/`HEIGHT`).
- **`/scroll`** maps `dx`/`dy` onto the demo's scroll action: `dy>0` down /
  `dy<0` up, `dx>0` right / `dx<0` left, amount = `abs(value)`; one scroll is
  emitted per nonzero axis.
- **`/exec`** is a plain blocking `sh -c` capturing stdout/stderr/return code
  (default 120s timeout, `code` 124 on timeout). It does **not** use the demo's
  `BashTool` session protocol, which intentionally does not surface a clean
  per-command exit code.
- **`/healthz`** returns 503 until `xdpyinfo` can reach the display, so callers
  can wait for the desktop before screenshotting.

### Screenshot protocol (diff-aware, Component 6)

`POST /screenshot {since?, mode?}` returns one of three shapes so polling a
static screen is nearly free and only changed regions travel the wire. The
server keeps a single base frame for the instance (PNG bytes + decoded pixels +
a monotonic `frame_token`), guarded by a lock.

- `since` (int, optional): the `frame_token` the caller currently holds.
- `mode` (optional): `auto` (default — server decides) or `full` (force a
  complete frame).

Three responses, distinguished by `mode`:

| `mode`     | Shape | When |
|------------|-------|------|
| `nochange` | `{"mode":"nochange","frame_token":<same as since>}` | current frame is **pixel-identical** to `since` (token does **not** advance) |
| `diff`     | `{"mode":"diff","frame_token":<new>,"base_token":<since>,"width":W,"height":H,"regions":[{"x","y","w","h","image":<b64 png>}, …]}` | only some tiles changed; the caller composites each region PNG onto its cached base at `(x,y)` |
| `full`     | `{"mode":"full","frame_token":<new>,"width":W,"height":H,"image":<b64 png>}` | first capture, `mode:"full"` forced, `since` ≠ the server's current base, or a diff would be no cheaper than a full frame |

How the diff is computed:

- **Tile dirty-check (64px grid).** The current frame is compared to the base
  in `64x64` blocks via numpy (`ECU_DIFF_TILE` overrides the size); edge tiles
  on the right/bottom are sliced to the frame bounds so every pixel is covered.
- **Run-merge.** Changed tiles are merged into rectangles by a simple
  per-tile-row horizontal run (no vertical merge — kept deliberately simple);
  each rectangle is cropped from the current frame and PNG-encoded as a region.
- **Full-fallback (two triggers).** Return `full` instead of `diff` when either
  the changed tiles cover **≥ 90 %** of the frame (`ECU_DIFF_FULL_COVERAGE`) —
  a whole-screen change / page transition — **or** the diff region PNGs together
  weigh **≥ the full-frame PNG**. (With a row-merge a whole-screen diff
  re-encodes the same pixels as ~one full frame at near-identical size, so the
  byte rule sits on the boundary; the coverage rule makes the whole-screen case
  deterministic.)
- **Token semantics.** `nochange` returns the *same* token as `since` and does
  not move the base. `diff`/`full` advance the token and set the new base.

The **client always reconstructs a complete frame** (cache the base, paste the
regions) before handing anything to a model — vision models can't apply
patches. Diffing is purely a wire/latency optimization; downscaling the
reconstructed frame (client side) is the main *token* lever. See
`skill/ecu-computer-use/ecu_client.py` (`_apply_diff`).

### Coordinates and scaling

The demo's `ComputerTool` ships with coordinate/image scaling enabled, which
silently rescales screenshots and remaps coordinates to a standard target
(XGA/WXGA/FWXGA) for non-standard resolutions. We **disable** that on our tool
instance so the contract is 1:1: client coordinates map directly to screen
pixels, and the returned PNG is at full `WIDTH`x`HEIGHT`.

### Ports and the "localhost-only" property

uvicorn binds `0.0.0.0` **inside the container** on purpose -- docker
port-publishing cannot reach a `127.0.0.1` bind inside the container. The
localhost-only security property is enforced by how the *instance* publishes the
port (`docker run -p 127.0.0.1:PORT:PORT` + firewall), which is the control
plane's job (Component 4), not the container's.

## Configuration

| Env var               | Default   | Meaning |
|-----------------------|-----------|---------|
| `WIDTH` / `HEIGHT`    | 1280x800  | Desktop resolution (the upstream demo defaults to 1024x768). |
| `DISPLAY_NUM`         | 1         | X display number (`DISPLAY=:1`). |
| `ECU_TOOLSERVER_PORT` | 8000      | Port uvicorn binds inside the container. |
| `ECU_NOVNC_PORT`      | 6080      | noVNC port that `/watch` redirects to. |
| `ECU_WATCH_PATH`      | /vnc.html | noVNC client page for `/watch`. |
| `ECU_DIFF_TILE`       | 64        | Tile size (px) for the screenshot dirty-check grid. |
| `ECU_DIFF_FULL_COVERAGE` | 0.9    | Changed-tile fraction at/above which `/screenshot` falls back to `full`. |

## Build & run locally

The base image is **amd64-only**; pass `--platform linux/amd64` for both build
and run (on Apple Silicon this runs under emulation -- slower, but correct).

```bash
# from the repo root
docker build --platform linux/amd64 -t ecu-image:dev image/

docker run -d --name ecu1 --platform linux/amd64 \
  -p 127.0.0.1:8000:8000 \
  -p 127.0.0.1:6080:6080 \
  ecu-image:dev

# wait for the desktop + tool server to come up (poll /healthz)
until curl -sf localhost:8000/healthz >/dev/null; do sleep 1; done
```

## Smoke tests

```bash
# health
curl -s localhost:8000/healthz

# screenshot -> decode + save a PNG
curl -s -XPOST localhost:8000/screenshot -H 'content-type: application/json' -d '{}' \
  | python3 -c 'import sys,json,base64;d=json.load(sys.stdin);print(d["mode"],d["width"],d["height"],len(d["image"]));open("/tmp/shot.png","wb").write(base64.b64decode(d["image"]))'
file /tmp/shot.png

# exec
curl -s -XPOST localhost:8000/exec -H 'content-type: application/json' \
  -d '{"command":"echo hi; xdotool getdisplaygeometry"}'

# move + key
curl -s -XPOST localhost:8000/move -H 'content-type: application/json' -d '{"x":640,"y":400}'
curl -s -XPOST localhost:8000/key  -H 'content-type: application/json' -d '{"keys":"Return"}'

# prove the GUI reacts: launch an app, then re-screenshot and confirm it changed
curl -s -XPOST localhost:8000/exec -H 'content-type: application/json' \
  -d '{"command":"DISPLAY=:1 xterm & sleep 2; true"}'
curl -s -XPOST localhost:8000/screenshot -H 'content-type: application/json' -d '{}' \
  | python3 -c 'import sys,json,base64;d=json.load(sys.stdin);open("/tmp/shot2.png","wb").write(base64.b64decode(d["image"]))'

# watch (302 -> noVNC; use GET, the route only allows GET)
curl -s -o /dev/null -w '%{http_code} -> %{redirect_url}\n' localhost:8000/watch

docker rm -f ecu1
```

Watch the live desktop in a browser while testing: open
<http://localhost:6080/vnc.html> (or just hit `http://localhost:8000/watch`).

### Screenshot diff-protocol smoke test

This driver proves the four diff-protocol behaviors against a running container
(needs `numpy` + `Pillow` on the host — already present in the image, so you can
also run it *inside* the container). It uses deterministic, blink-free changes
(still `display` image windows for a small change; a Tk fullscreen window for
the whole-screen change) and reconstructs the diff exactly like the real client
(`ecu_client.py`): composite each region PNG onto the cached base, then assert
the result is pixel-identical to a forced-full of the same settled screen.

```bash
docker run -d --name ecu-c6 --platform linux/amd64 -p 127.0.0.1:8000:8000 ecu-image:dev
curl -s --retry 90 --retry-delay 2 --retry-all-errors --retry-connrefused \
  -o /dev/null -w 'healthz:%{http_code}\n' http://localhost:8000/healthz
python3 image/toolserver/diff_smoke.py        # the driver below
docker rm -f ecu-c6
```

`image/toolserver/diff_smoke.py` exercises, in order:

1. **full** — first capture (note token `T0`);
2. **nochange** — on the static idle desktop, `POST {"since":T0}` returns
   `mode:"nochange"` with the **same** `frame_token` (the desktop is verified
   empirically static at minute clock resolution);
3. **diff** — recolor a small still window, `POST {"since":<base>}` returns
   `mode:"diff"`; the driver reconstructs base + regions and asserts **0**
   differing pixels vs a forced-full, printing the byte savings (~93–95 %);
4. **full-fallback** — a Tk **fullscreen** window changes the whole screen;
   `POST {"since":<base>}` returns `mode:"full"` (≥ 90 % of tiles changed).

Expected tail: `RESULT: ALL CHECKS PASSED`.
