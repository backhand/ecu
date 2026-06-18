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

The control plane is a single static Go binary. Run it headless by setting
`ECU_*` env vars, or run it on a TTY and answer the setup wizard. There are two
deployment shapes: a **standalone box** (the binary terminates TLS itself via
Let's Encrypt) and **k3s** (a fronting Ingress terminates TLS). See
`references/BRIEF.md` for the full design.

### Standalone (`curl | sh`)

Install the binary:

```sh
curl -fsSL https://github.com/backhand/ecu/releases/latest/download/install.sh | sh
```

Configure and run. The control plane needs an admin API key and (on the default
Hetzner provider) a Hetzner Cloud token:

```sh
export ECU_API_KEY="$(openssl rand -hex 32)"   # bootstrap admin key
export ECU_HCLOUD_TOKEN="<hetzner-cloud-api-token>"

# Automatic TLS via Let's Encrypt:
export ECU_HOSTNAME="ecu.example.com"           # a real DNS A record -> this box
export ECU_TLS=auto

ecu
```

With `ECU_TLS=auto` the binary uses [autocert](https://pkg.go.dev/golang.org/x/crypto/acme/autocert):
it binds **:443** (HTTPS, the control-plane API plus the WebSocket tunnel and
watch endpoints) and **:80** (the Let's Encrypt HTTP-01 challenge, plus a
redirect of all other HTTP traffic to HTTPS). Both ports must be reachable from
the internet, and `ECU_TLS=auto` **ignores `ECU_LISTEN`** (autocert owns the
well-known ports). Issued certificates are cached under `ECU_TLS_CACHE_DIR`
(`~/.local/share/ecu/tls` by default) so restarts don't re-request them.

> **No DNS yet? nip.io fallback.** If you set `ECU_TLS=auto` but leave
> `ECU_HOSTNAME` unset, the control plane detects the box's public IPv4 and uses
> `<dashed-ip>.nip.io` (e.g. `203-0-113-7.nip.io`) as the hostname — zero DNS
> setup, good for trying ECU out. Caveat: nip.io is a **shared domain**, so
> Let's Encrypt's per-domain certificate rate limits apply across all nip.io
> users. Use it to kick the tires, not as a long-lived production hostname —
> point real DNS at the box and set `ECU_HOSTNAME` for anything durable.

**Dev / no TLS.** Leave `ECU_TLS` unset (or `=off`) and the control plane serves
plain HTTP on `ECU_LISTEN` (default `127.0.0.1:8080`) — handy for local
development and the mode you use behind a TLS-terminating proxy.

### k3s

The control-plane image is `ghcr.io/backhand/ecu-controlplane`. In a cluster,
**the Ingress terminates TLS** (traefik is the k3s default, optionally with
cert-manager), so the Deployment runs `ECU_TLS=off` and serves plain HTTP — no
privileged ports in the pod. Manifests live in [`deploy/k3s/`](deploy/k3s/).

```sh
# 1. Create the secret (do NOT commit real values):
kubectl create secret generic ecu-secrets \
  --from-literal=ECU_API_KEY="$(openssl rand -hex 32)" \
  --from-literal=ECU_HCLOUD_TOKEN="<hetzner-cloud-api-token>" \
  --from-literal=ECU_SIGNING_KEY="$(openssl rand -hex 32)"

# 2. Set your hostname in deployment.yaml (ECU_HOSTNAME) and ingress.yaml,
#    then apply the manifests:
kubectl apply -f deploy/k3s/deployment.yaml \
              -f deploy/k3s/service.yaml \
              -f deploy/k3s/ingress.yaml

# 3. Point a DNS A record at the Ingress's external address.
```

TLS issuance is wired in `deploy/k3s/ingress.yaml` (cert-manager or traefik's
built-in ACME — pick one; see the comments there). The agent tunnel
(`/agent/connect`) and live watch (`/sessions/{id}/watch`) ride WebSocket, which
traefik proxies transparently — no extra Ingress configuration needed. The
control plane is a stateful singleton (embedded SQLite + in-memory tunnel
registry): run **one** replica, backed by the `ecu-data` PVC. See
[`deploy/k3s/README.md`](deploy/k3s/README.md) for details.
