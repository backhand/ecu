# ecu — Easy Computer Use

Self-hosted, auto-bootstrapped **computer use in the cloud**.

ECU lets any agentic platform (Claude Code, Cowork, a custom agent) gain a real
Linux desktop "computer" on demand by dropping in a single **skill**. The agent
calls a tool to start a session; behind the scenes a disposable cloud instance
boots, runs a computer-use container, and exposes a secure outbound tunnel back
to a small **control plane** you operate. The agent drives the desktop
(screenshot / click / type / exec) through the control plane's API and tears the
session down when done.

- **One fixed point.** The control plane has a stable hostname + TLS; instances
  are disposable cattle with **no inbound public ports**.
- **Outbound-only tunnel.** Each instance dials out to the control plane over
  WebSocket + yamux. Nothing listens publicly on the box.
- **Thin, portable skill.** The skill teaches the host model the control-plane
  tool surface; the host model supplies all vision + reasoning. Drops in across
  platforms unchanged.
- **Cloud-agnostic seam, Hetzner first.** All cloud ops go through a `Provider`
  interface; Hetzner is the only implementation shipped initially.

> Status: under active development. Build follows the order in `references/BRIEF.md`.

## Repository layout

```
ecu/
├── cmd/ecu/              # main: control-plane mode vs --agent mode
├── internal/
│   ├── controlplane/     # HTTP API, auth, session registry, tool proxy, reaper
│   ├── agent/            # --agent mode: reverse tunnel + registration/heartbeat
│   ├── config/           # env + wizard loading (precedence rule)
│   ├── store/            # embedded SQLite
│   ├── tunnel/           # WSS+yamux reverse tunnel (transport-swappable)
│   └── provider/         # Provider interface
│       └── hcloud/       # Hetzner implementation (only one shipped)
├── image/                # instance container: Dockerfile + FastAPI tool server
├── skill/ecu-computer-use/   # SKILL.md, mcp_server.py, ecu_cli.py, ecu_client.py
├── deploy/k3s/           # Deployment + Service + Ingress + Secret manifests
└── install.sh            # curl | sh installer
```

## Quickstart

_Coming with the packaging milestone (`curl | sh` install + k3s manifests)._

The control plane is a single static Go binary that bootstraps itself: run it,
answer the wizard (or set `ECU_*` env vars for headless), and it acquires TLS and
starts provisioning. See `references/BRIEF.md` for the full design.
