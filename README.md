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
  interface; Hetzner provisions remote VMs, and a built-in `local` provider runs
  each desktop as a Docker container on the control-plane box itself
  (`ECU_PROVIDER=local`) for single-machine setups.

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
│       ├── hcloud/       # Hetzner implementation (remote VMs)
│       └── local/        # co-located Docker containers (ECU_PROVIDER=local)
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

### Single box (`ECU_PROVIDER=local`)

For local development, a self-contained demo, or a single-machine deployment you
don't have to provision cloud instances at all. With `ECU_PROVIDER=local` the
control plane runs **each disposable desktop as a Docker container on the same
box**, co-located with the control plane, instead of booting a remote VM. There
is no reverse tunnel: the container's tool-server port is published bound to
`127.0.0.1` and the control plane talks to it directly.

Requirements: a working **Docker** daemon and the computer-use **container image
available locally** (the control plane does not pull it). Build or pull it first,
then point `ECU_CONTAINER_IMAGE` at it:

```sh
export ECU_API_KEY="$(openssl rand -hex 32)"   # bootstrap admin key
export ECU_PROVIDER=local
export ECU_CONTAINER_IMAGE=ecu-image:dev        # a desktop image present on this host

ecu                                             # serves plain HTTP on ECU_LISTEN (127.0.0.1:8080)
```

A `POST /sessions` spins up a container (image's tool server comes up in seconds;
the control plane waits for its `/healthz` before marking the session `ready`),
and `screenshot` / `click` / `type` / `exec` / `actions` proxy straight to it.
`DELETE /sessions/{id}` removes the container. No Hetzner token, instance type,
or region is needed — only `ECU_API_KEY`.

Notes and limits:

- **Localhost-bound.** Every container's tool-server port is published with
  `-p 127.0.0.1::8000`, so it is never reachable off-box. Front the control
  plane itself with TLS/auth if you expose it.
- **No persistence.** Snapshot-and-restore is a cloud-instance feature; on the
  local provider a `persistent:true` or `restore` request is rejected with
  `400 persistence is not supported with the local provider`. Ephemeral sessions
  work normally.
- The image is run with `--platform linux/amd64`; on an arm64 host it runs under
  emulation (slower to boot, still works), which the readiness wait allows for.

### k3s

The control-plane image is `ghcr.io/backhand/ecu-controlplane`. In a cluster,
**the Ingress terminates TLS** (traefik is the k3s default, optionally with
cert-manager), so the Deployment runs `ECU_TLS=off` and serves plain HTTP — no
privileged ports in the pod. The manifests in [`deploy/k3s/`](deploy/k3s/) are
wired together with Kustomize — per-deployment knobs (namespace, image tag,
hostname, Hetzner instance type/region) all live in `kustomization.yaml`.

```sh
# 1. Provide the secret (gitignored; copy the template and fill it in):
cp deploy/k3s/secret.yaml.example deploy/k3s/secret.yaml
#    ECU_API_KEY / ECU_SIGNING_KEY -> openssl rand -hex 32; ECU_HCLOUD_TOKEN -> Hetzner token

# 2. Set ECU_HOSTNAME once in deploy/k3s/kustomization.yaml (it is stamped into
#    the ConfigMap and the Ingress for you), then render + apply:
kubectl apply -k deploy/k3s

# 3. Point a DNS A record at the Ingress's external address.
```

TLS issuance is wired in `deploy/k3s/ingress.yaml` (cert-manager or traefik's
built-in ACME — pick one; see the comments there). The agent tunnel
(`/agent/connect`) and live watch (`/sessions/{id}/watch`) ride WebSocket, which
traefik proxies transparently — no extra Ingress configuration needed. The
control plane is a stateful singleton (embedded SQLite + in-memory tunnel
registry): run **one** replica, backed by the `ecu-data` PVC. See
[`deploy/k3s/README.md`](deploy/k3s/README.md) for details.
