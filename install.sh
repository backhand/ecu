#!/bin/sh
# ECU control-plane installer.
#
# Downloads the prebuilt `ecu` binary for this OS/arch from the GitHub release
# and installs it to /usr/local/bin/ecu. Intended to be piped from curl:
#
#   curl -fsSL https://github.com/backhand/ecu/releases/latest/download/install.sh | sh
#
# Overridable via environment:
#   ECU_INSTALL_BASE   release asset base URL
#                      (default https://github.com/backhand/ecu/releases/latest/download)
#   ECU_INSTALL_DIR    install directory (default /usr/local/bin)
#
# POSIX sh only (no bashisms): runs under dash/ash/busybox sh, not just bash.
set -eu

# ---- configuration --------------------------------------------------------
INSTALL_BASE="${ECU_INSTALL_BASE:-https://github.com/backhand/ecu/releases/latest/download}"
INSTALL_DIR="${ECU_INSTALL_DIR:-/usr/local/bin}"
BIN_NAME="ecu"

# ---- helpers --------------------------------------------------------------
# err prints a message to stderr and exits non-zero.
err() {
	printf 'ecu install: error: %s\n' "$1" >&2
	exit 1
}

info() {
	printf 'ecu install: %s\n' "$1"
}

# ---- detect OS ------------------------------------------------------------
uname_s="$(uname -s)"
case "$uname_s" in
	Linux) os="linux" ;;
	Darwin) os="darwin" ;;
	*) err "unsupported OS '$uname_s' (only Linux and Darwin are supported)" ;;
esac

# ---- detect architecture --------------------------------------------------
uname_m="$(uname -m)"
case "$uname_m" in
	x86_64 | amd64) arch="amd64" ;;
	aarch64 | arm64) arch="arm64" ;;
	*) err "unsupported architecture '$uname_m' (only x86_64/amd64 and aarch64/arm64 are supported)" ;;
esac

asset="${BIN_NAME}-${os}-${arch}"
url="${INSTALL_BASE}/${asset}"

# ---- download to a temp file ----------------------------------------------
tmp="$(mktemp)" || err "could not create a temporary file"
# Clean up the temp file on any exit (success, error, or signal).
trap 'rm -f "$tmp"' EXIT INT TERM

info "downloading ${asset} from ${url}"
if command -v curl >/dev/null 2>&1; then
	curl -fsSL -o "$tmp" "$url" || err "download failed (curl) from $url"
elif command -v wget >/dev/null 2>&1; then
	wget -qO "$tmp" "$url" || err "download failed (wget) from $url"
else
	err "neither curl nor wget is available; cannot download $url"
fi

# Sanity-check we actually got a non-empty file.
if [ ! -s "$tmp" ]; then
	err "downloaded file is empty; expected a binary at $url"
fi

chmod +x "$tmp" || err "could not make the downloaded file executable"

# ---- install --------------------------------------------------------------
dest="${INSTALL_DIR}/${BIN_NAME}"
info "installing to ${dest}"
if mv "$tmp" "$dest" 2>/dev/null; then
	# Moved into place; the EXIT trap's rm is now a harmless no-op.
	:
elif command -v sudo >/dev/null 2>&1; then
	info "${INSTALL_DIR} is not writable; retrying with sudo"
	sudo mv "$tmp" "$dest" || err "could not install to $dest (even with sudo)"
else
	err "cannot write to $dest and sudo is not available; re-run as root or set ECU_INSTALL_DIR to a writable directory"
fi

# ---- verify ---------------------------------------------------------------
# Do NOT execute the binary to "verify" it: `ecu` with no/unknown args starts
# the control-plane server (which would block / start listening). Instead just
# confirm the installed file exists and is executable.
if [ ! -x "$dest" ]; then
	err "installed file $dest is not executable"
fi

info "installed ${BIN_NAME} -> ${dest}"
printf '\n'
printf 'Next steps:\n'
printf '  1. Set the required configuration (environment variables):\n'
printf '       export ECU_API_KEY=<your-admin-api-key>\n'
printf '       export ECU_HCLOUD_TOKEN=<your-hetzner-cloud-token>\n'
printf '  2. For automatic TLS (Let'\''s Encrypt), point DNS at this box and set:\n'
printf '       export ECU_HOSTNAME=your.host.example.com   # or rely on the nip.io fallback\n'
printf '       export ECU_TLS=auto                          # binds :443 (HTTPS) + :80 (HTTP-01)\n'
printf '     The box must have ports 443 and 80 reachable from the internet.\n'
printf '     (Dev/no-TLS: leave ECU_TLS unset/off; it serves plain HTTP on ECU_LISTEN.)\n'
printf '  3. Run it:\n'
printf '       %s\n' "$dest"
printf '\n'
