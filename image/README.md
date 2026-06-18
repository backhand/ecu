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
| `POST /screenshot`| `{since?, mode?}`             | `{"mode":"full","frame_token":N,"image":"<base64 png>","width":W,"height":H}` |
| `POST /click`     | `{x,y,button}` (left/right/middle) | `{"ok":true}` |
| `POST /move`      | `{x,y}`                       | `{"ok":true}` |
| `POST /type`      | `{text}`                      | `{"ok":true}` |
| `POST /key`       | `{keys}` e.g. `"ctrl+l"`, `"Return"` (xdotool syntax) | `{"ok":true}` |
| `POST /scroll`    | `{x,y,dx,dy}`                 | `{"ok":true}` |
| `POST /exec`      | `{command, timeout?}`         | `{"stdout":...,"stderr":...,"code":N}` |
| `GET  /healthz`   | --                            | `{"ok":true}` (200; 503 until the X display is up) |
| `GET  /watch`     | --                            | 302 redirect to the local noVNC client |

Notes:

- **`/screenshot`** always returns a full frame in this component. The server
  keeps the last frame bytes and a monotonic `frame_token`, so the Component 6
  diff protocol (`nochange`/`diff` against `since`) slots in without changing
  the response shape. `width`/`height` are read from the actual PNG (equal to
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

# watch (302 -> noVNC)
curl -sI localhost:8000/watch | head -2

docker rm -f ecu1
```

Watch the live desktop in a browser while testing: open
<http://localhost:6080/vnc.html> (or just hit `http://localhost:8000/watch`).
