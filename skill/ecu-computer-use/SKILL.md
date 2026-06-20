---
name: ecu-computer-use
description: >-
  Give the agent a real cloud Linux desktop it can see and control — take
  screenshots, move/click the mouse, type, press keys, scroll, and run shell
  commands on a disposable remote computer via the ECU (Easy Computer Use)
  control plane. Use this skill WHENEVER the task requires operating a graphical
  computer, a browser, or a desktop app: driving a web app or website that has
  no API, clicking through a GUI, filling forms visually, logging in somewhere,
  testing or demoing software, automating a desktop application, scraping a page
  that needs a real browser, or taking screenshots of live pages. Trigger it
  even when the user never says "computer use" — if doing the task means seeing
  a screen and clicking/typing on it, reach for this skill. Requires ECU_URL and
  ECU_API_KEY to be set.
---

# ECU — Easy Computer Use

This skill lets you operate a real, disposable Linux desktop in the cloud. You
perceive it by taking screenshots and act on it with the mouse, keyboard, and
shell. You supply all the vision and reasoning; this skill is just the controls.

There are two front-ends over the same control-plane API:

- **MCP server** (`mcp_server.py`) — preferred when your environment speaks MCP.
  A clean, typed tool surface; it remembers your session and screenshot state
  for the life of the process.
- **CLI** (`ecu_cli.py`) — for custom agent loops or non-MCP environments. Each
  invocation is a fresh process; it keeps a tiny on-disk sidecar cache so it
  still gets the cheap screenshot path and consistent click coordinates.

Both do the same thing. Prefer the MCP server if you can.

## Setup

<!-- ECU_PRECONFIGURED -->

Set two environment variables (get these from whoever runs your ECU control
plane):

```
ECU_URL=https://your-ecu-control-plane.example.com
ECU_API_KEY=<your provisioned key>
```

Python deps (clear-source, no compiled binaries): `pip install "mcp[cli]"
requests Pillow`. (`requests` is the HTTP client; `Pillow` is needed for
screenshot diff reconstruction and downscaling; `mcp[cli]` only for the MCP
server.)

### Option A — MCP server (preferred)

Point your MCP host at `mcp_server.py` over stdio. Typical config:

```json
{
  "mcpServers": {
    "ecu-computer-use": {
      "command": "python",
      "args": ["/path/to/ecu-computer-use/mcp_server.py"],
      "env": {
        "ECU_URL": "https://your-ecu-control-plane.example.com",
        "ECU_API_KEY": "your-key"
      }
    }
  }
}
```

You then get these tools: `start_session`, `screenshot`, `click`, `move`,
`type_text`, `key`, `scroll`, `exec_command`, `watch_url`, `end_session`.

### Option B — CLI

```bash
export ECU_URL=https://your-ecu-control-plane.example.com
export ECU_API_KEY=your-key

sid=$(python ecu_cli.py start)
python ecu_cli.py screenshot "$sid" --out screen.png   # then look at screen.png
python ecu_cli.py click "$sid" 640 400
python ecu_cli.py type "$sid" "hello"
python ecu_cli.py key "$sid" Return
python ecu_cli.py exec "$sid" "google-chrome >/dev/null 2>&1 &"
python ecu_cli.py end "$sid"
```

Run `python ecu_cli.py --help` for all subcommands. The CLI prints the session
id (and only the id) on stdout for `start`, so `sid=$(... start)` works; status
detail goes to stderr.

## The loop

It is always the same:

1. **Start** a session: `start_session` (MCP) / `ecu_cli.py start`. You get a
   `session_id`; the tools wait until the desktop is ready. Pass that id to
   every later call.
2. **Look, then act.** Take a `screenshot` when you need to see the screen, read
   it, decide, then `click` / `type_text` / `key` / `scroll` / `exec_command`.
   Repeat.
3. **End** the session with `end_session` / `ecu_cli.py end` when finished —
   **always do this, even if the task failed.** An ephemeral session holds a
   paid cloud instance for as long as it is alive.

A minimal flow: start → screenshot → see the desktop → `exec_command
"google-chrome >/dev/null 2>&1 &"` (or click the Chrome icon) → screenshot →
click the address bar → `type_text` a URL → `key Return` → screenshot to read
the page → … → `end_session`.

## Using the computer well

- **Prefer the shell when there's a clean command path.** `exec_command` to
  launch apps, manage files, or install packages is faster and more reliable
  than hunting for icons with the mouse. Reach for the GUI only for things that
  truly need it (operating a web app, a page with no API, visual verification).
- **Screenshot on demand, not after every action.** You don't need a fresh
  image after every keystroke. Take one when the screen has meaningfully changed
  or when you need to verify or decide. Each image you look at costs model
  tokens — that, not bandwidth, is the dominant cost of a computer-use loop.
- **Lean on `nochange` when polling.** If you screenshot again and nothing has
  changed (e.g. waiting for a page to load), the system detects it and the MCP
  `screenshot` tool returns a short *text* note like `no change since last
  screenshot (frame 12)` instead of re-sending the identical image — so polling
  "did it land yet?" is almost free. You already hold the prior image; keep
  using it. Space repeated checks a second or two apart. When you genuinely want
  to re-examine the pixels of an unchanged screen, pass `force_full=true` to
  `screenshot` to force a fresh image. (The CLI gets the same cheap path via its
  sidecar cache; a `nochange` screenshot just re-saves the cached frame.)
- **Coordinates are in the screenshot's pixel space — the client handles
  scaling.** Click, move, and scroll where you see the target in the screenshot
  you were just shown. If that screenshot was downscaled (the `max_width`
  option, default 1280), the tools automatically translate your coordinates back
  to the desktop's native pixels before sending them, so a click lands on what
  you saw. You do not rescale anything yourself. (For scroll, the `(x, y)` anchor
  is rescaled; `dx`/`dy` are scroll "clicks", not coordinates, and are sent as-is.)
- **Read before you click.** Take a screenshot and actually locate the target
  before issuing a click; don't guess coordinates blind.

## Persistent sessions (privileged — usually skip)

By default a session is **ephemeral**: when you call `end_session` the computer
is destroyed and nothing is kept. This is what you want almost always.

A **persistent** session keeps its desktop state (logged-in apps, files) across
sessions: ending it snapshots and *stops* it (status `stopped`) rather than
destroying it, and you resume it later by passing its id as `restore` to
`start_session`. It is a privileged capability the operator must authorize for
your API key — if you request `persistent: true` (or a `restore`) without
authorization, the request is **rejected** with an explanation (it is never
silently downgraded to ephemeral). Only request it when the task genuinely needs
state to survive across sessions (e.g. a long multi-session workflow the user
has set up for a trusted agent). When in doubt, don't.

## Watching (for humans)

`watch_url` returns a link a person can open in a browser to watch the session
live. This is for human oversight only — it is **not** how you perceive the
screen. You always use `screenshot` to see. The link is only available while the
session is `ready` and carries a short-lived view token.

## More detail

`references/api.md` documents the underlying control-plane HTTP API directly
(endpoints, request/response shapes, the `full`/`diff`/`nochange` screenshot
protocol, watch, error codes). Read it only if you need to debug the clients or
talk to the control plane without them — normal use goes through the MCP tools
or the CLI above.
