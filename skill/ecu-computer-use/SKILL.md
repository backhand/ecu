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

Python deps (clear-source, no compiled binaries). Install only what your
front-end needs:

- **MCP server**: `pip install "mcp[cli]" requests Pillow`
- **CLI or direct `ecu_client` use** (no MCP): `pip install requests Pillow`

(`requests` is the HTTP client; `Pillow` is needed for screenshot diff
reconstruction and downscaling; `mcp[cli]` is needed ONLY for the MCP server.)

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
python ecu_cli.py exec "$sid" "xdg-open https://example.com >/dev/null 2>&1 &"
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
"xdg-open https://example.com >/dev/null 2>&1 &"` (or click the browser icon) →
screenshot (with `settle` to wait for the page to finish loading) → click the
address bar → `type_text` a URL → `key Return` → `settle` screenshot to read the
page → … → `end_session`.

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
- **Wait for the screen to settle instead of sleeping.** When you've just done
  something that triggers a render — opened a window, clicked a link, submitted a
  form — don't screenshot immediately (you may catch a half-painted frame) and
  don't guess a `sleep`. Pass **`settle`** to `screenshot` and the server waits
  until the screen has stopped changing, then returns that stable frame: MCP
  `screenshot(session_id, settle=true)` / CLI `screenshot "$sid" --settle`. This
  is the preferred way to wait for a page load or a window to finish opening. It
  never hangs — an endlessly-animating screen (a spinner or video) returns the
  latest frame at a cap (`max_wait_ms`, ~2500 ms by default). Tune the window
  with `settle_ms` (how long unchanged counts as "stable") and the ceiling with
  `max_wait_ms` if you need to.
- **Screenshot format is a wire codec, not what the model sees.** The model
  always receives a PNG; `format` only controls how the frame is encoded on the
  wire to save bytes/tokens. `jpeg` (default) is right for normal use; `webp` is
  smallest; `png` is lossless (crisp text) but large and slow — the client
  auto-raises this call's timeout for it, so don't reach for `png` on photo-heavy
  screens where the size cost isn't worth it.
- **Coordinates are in the screenshot's pixel space — the client handles
  scaling.** Click, move, and scroll where you see the target in the screenshot
  you were just shown. If that screenshot was downscaled (the `max_width`
  option, default 1280), the tools automatically translate your coordinates back
  to the desktop's native pixels before sending them, so a click lands on what
  you saw. You do not rescale anything yourself. (For scroll, the `(x, y)` anchor
  is rescaled; `dx`/`dy` are scroll "clicks", not coordinates, and are sent as-is.)
- **Read before you click.** Take a screenshot and actually locate the target
  before issuing a click; don't guess coordinates blind.

## Opening URLs and the browser

The standard desktop image ships **Firefox ESR** — there is **no Chrome**. To
open a URL, prefer the image-agnostic launcher:

```
xdg-open https://example.com >/dev/null 2>&1 &     # opens in the default browser
```

or launch Firefox directly with `firefox-esr https://example.com >/dev/null 2>&1
&`. **Never use `google-chrome`** — it isn't installed. Background the launch
with a trailing `&` so `exec` returns immediately instead of blocking while the
browser runs (see below). After opening a page, take a **`settle`** screenshot so
you read it once it has finished loading rather than mid-render.

## Running commands (`exec`)

`exec` runs a shell command on the desktop and is often the fastest, most
reliable way to do something (launch an app, move files, install a package,
check whether a tool exists) — prefer it over hunting for icons with the mouse.

- **It returns a structured result: stdout, stderr, and an exit code.** Use them.
  The MCP `exec_command` tool prints `exit=<code>`, then `stdout:` / `stderr:`
  blocks; the CLI `exec` writes stdout to stdout, stderr to stderr, and exits the
  process with the command's own exit code (so `&&`, `||`, `$?` work in a shell
  loop). The direct client returns `{"stdout", "stderr", "code"}`.
- **It runs `sh -c <command>`** — so pipes, `&&`, `||`, loops, globs, and
  redirects all work in one command string — as the unprivileged user
  **`computeruse`** with cwd **`/home/computeruse`**. It is one-shot: there is no
  shell that persists between calls (set env / `cd` within the single command).
- **Foreground commands return when they finish**, bounded by a server run budget
  (**120 s** by default); a command that overruns is killed and comes back with
  exit code **124**. Raise the budget for a legitimately long job via MCP
  `exec_command(timeout=<seconds>)`, CLI `exec ... --timeout <seconds>`, or
  `ECUClient.exec(timeout=<seconds>)`.
- **Background long-running / GUI apps with a trailing `&`** so `exec` returns at
  once instead of blocking for the whole 120 s (e.g. `firefox-esr ... &`).
- **Read stdout to make decisions.** For example, verify a browser is present
  before launching it:

  ```
  command -v firefox-esr        # exit 0 + a path on stdout if it's installed
  ```

  then branch on the exit code / path rather than blindly launching and
  screenshotting to see what happened.

## First action after `ready` (latency and retries)

The very first screenshot or `exec` against a freshly-`ready` session can be
slow — the cloud desktop is still warming up its caches even though X is up. The
client smooths this over for you, so it's mostly invisible:

- **`start` warms the screenshot + exec paths** before it returns, so by the time
  you get the `session_id`, `ready` genuinely means *action-ready* — your first
  real screenshot/click isn't the one that eats the cold start. (This warm-up is
  skipped when you start without waiting — CLI `--no-wait` / `wait=False`.)
- **Idempotent calls auto-retry on transient failures.** `screenshot`, `status`,
  and `end_session` are safe to re-issue, so on a transient timeout or a `503`
  (server briefly warming) the client retries them with backoff. Actions that
  have side effects — `click`/`move`/`type_text`/`key`/`scroll`/`exec_command`
  and `start_session` — are **not** retried (re-issuing could double-click, run a
  command twice, or orphan a second paid instance); they raise on a transient
  failure so you can decide whether redoing them is safe.
- **Timeout defaults** (override if you hit an edge): per-request HTTP timeout is
  **30 s**; `exec` gets up to ~**150 s** of client budget (server run budget
  120 s); a `png` or full-res (`--full-res` / `max_width` unset) screenshot
  auto-raises this call to **120 s**; a `settle` screenshot raises to its
  `max_wait_ms` cap. Override globally with the CLI `--timeout` / `--retries`
  flags (on every subcommand) or `ECUClient(timeout=..., retries=...)`.

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

## Tool and command reference

Both front-ends cover the same actions. MCP tools take typed params; CLI
subcommands take positional args + flags. Key params are listed so nothing has to
be guessed.

| MCP tool | CLI subcommand | Key params / flags |
|----------|----------------|--------------------|
| `start_session` | `start` | `persistent` (bool), `restore` (id); CLI `--no-wait` to skip waiting (also skips warm-up) |
| `screenshot` | `screenshot` | `max_width` (default 1280; CLI `--max-width`, or `--full-res` for native), `force_full`; settle: `settle` (bool / `--settle`), `settle_ms` (`--settle-ms`), `max_wait_ms` (`--max-wait-ms`); CLI `--out` / `--b64` |
| `click` | `click` | `x`, `y`, `button` (`left`/`right`/`middle`) |
| `move` | `move` | `x`, `y` |
| `type_text` | `type` | `text` |
| `key` | `key` | `keys` (xdotool syntax: `Return`, `ctrl+l`, `alt+F4`) |
| `scroll` | `scroll` | `x`, `y`, `dx`, `dy` (deltas are scroll clicks, never rescaled) |
| `exec_command` | `exec` | `command`; `timeout` (server run budget, seconds; MCP `timeout=`, CLI `--timeout`) |
| `watch_url` | `watch` | — (human watch link) |
| `end_session` | `end` | — (always call when finished) |

Shared on **every** CLI subcommand (the client knobs): `--timeout <seconds>`
(per-request HTTP timeout; doubles as `exec`'s run budget) and `--retries <n>`
(total attempts for the safe/idempotent calls). The same two map to
`ECUClient(timeout=..., retries=...)`. Coordinates for `click`/`move`/`scroll`
are in the last screenshot's pixel space and rescaled automatically; CLI
`--max-width` / `--full-res` on those subcommands state the space explicitly when
running stateless one-offs.

## More detail

`references/api.md` documents the underlying control-plane HTTP API directly
(endpoints, request/response shapes, the `full`/`diff`/`nochange` screenshot
protocol, watch, error codes). Read it only if you need to debug the clients or
talk to the control plane without them — normal use goes through the MCP tools
or the CLI above.
