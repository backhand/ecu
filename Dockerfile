# ECU control-plane image.
#
# This builds the `ecu` control-plane BINARY (cmd/ecu) into a minimal,
# rootless container. It is DISTINCT from image/Dockerfile, which builds the
# computer-use INSTANCE image (the desktop + tool server a session runs); this
# is the long-lived server you operate.
#
# Two stages:
#   * builder: a full Go toolchain compiles a static, stripped linux binary.
#     modernc.org/sqlite is pure Go, so CGO stays OFF — the resulting binary has
#     no libc dependency and drops straight into a `scratch`-class base.
#   * final: gcr.io/distroless/static-debian12:nonroot — no shell, no package
#     manager, just the binary, CA certificates, and an unprivileged user.
#
# Build (from the repo root, so the build context includes go.mod + source):
#   docker build -t ghcr.io/backhand/ecu-controlplane:latest -f Dockerfile .
#
# Run (plain HTTP behind a TLS-terminating Ingress — the k3s path):
#   docker run -e ECU_API_KEY=... -e ECU_HCLOUD_TOKEN=... -e ECU_TLS=off \
#              -e ECU_LISTEN=0.0.0.0:8080 -p 8080:8080 \
#              ghcr.io/backhand/ecu-controlplane:latest

# --- builder ---------------------------------------------------------------
FROM golang:1.26-bookworm AS builder
WORKDIR /src

# Download modules first, on their own layer, so the (slow) module fetch is
# cached and only re-runs when go.mod/go.sum change — not on every source edit.
COPY go.mod go.sum ./
RUN go mod download

# Now the source. The build is fully static (CGO_ENABLED=0): modernc.org/sqlite
# is pure Go, so nothing links libc. -trimpath strips local paths from the
# binary; -ldflags="-s -w" drops the symbol table and DWARF for a smaller image.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/ecu ./cmd/ecu

# --- final -----------------------------------------------------------------
# distroless/static-debian12 is the right base for a static CGO-free binary: no
# shell, no package manager, tiny attack surface. The ":nonroot" tag runs as an
# unprivileged user (uid/gid 65532) by default.
#
# CA certificates: distroless/static BUNDLES /etc/ssl/certs/ca-certificates.crt.
# The control plane NEEDS them — outbound TLS to the Hetzner Cloud API and to
# Let's Encrypt (ECU_TLS=auto) both verify against the system trust store — so
# this base being CA-equipped is load-bearing, not incidental. (A `scratch`
# base would need the certs copied in explicitly.)
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/ecu /ecu

# Documentation only (does not publish anything): 443/80 are the autocert
# HTTPS + HTTP-01 ports used when ECU_TLS=auto; 8080 is the default plain-HTTP
# ECU_LISTEN used when ECU_TLS=off.
#
# Privileged-port note: the :nonroot user CANNOT bind :443/:80 in the default
# Linux setup. That is fine for the k3s path, which runs ECU_TLS=off behind the
# traefik Ingress (the Ingress terminates TLS, the pod only binds 8080 — no
# privileged ports in-cluster). The autocert (ECU_TLS=auto) path is the
# standalone `curl | sh` binary an operator runs on a box directly (as root, or
# with CAP_NET_BIND_SERVICE via setcap), NOT this in-cluster image.
EXPOSE 443 80 8080

ENTRYPOINT ["/ecu"]
