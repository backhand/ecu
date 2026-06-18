#!/bin/bash
#
# ECU instance entrypoint.
#
# Reuses the anthropic computer-use-demo desktop startup (Xvfb + tint2 + mutter
# + x11vnc + noVNC) verbatim, then launches the ECU FastAPI tool server with
# uvicorn -- replacing the demo's Streamlit chat UI / agent loop.
#
# Startup order matters: the X display must be fully up before the tool server
# starts serving screenshots, so we block on xdpyinfo before exec'ing uvicorn.

set -e

export DISPLAY_NUM=${DISPLAY_NUM:-1}
export HEIGHT=${HEIGHT:-800}
export WIDTH=${WIDTH:-1280}
export DISPLAY=:${DISPLAY_NUM}

ECU_TOOLSERVER_PORT=${ECU_TOOLSERVER_PORT:-8000}

cd "$HOME"

# Bring up the desktop stack (Xvfb, tint2, mutter, x11vnc on :5900) and the
# noVNC websocket proxy (on :6080). These are the demo's own scripts; we do not
# modify them -- noVNC is kept running for the future /watch endpoint.
./start_all.sh
./novnc_startup.sh

# The demo's static landing page (port 8080). Harmless and handy for humans;
# not part of the tool contract.
python http_server.py >/tmp/server_logs.txt 2>&1 &

# Wait for the X display to actually answer before serving the tool API, so the
# first /screenshot can't race the desktop coming up.
echo "waiting for display ${DISPLAY} ..."
for _ in $(seq 1 60); do
    if xdpyinfo >/dev/null 2>&1; then
        echo "display ${DISPLAY} is up"
        break
    fi
    sleep 0.5
done

echo "✨ ECU tool server starting on :${ECU_TOOLSERVER_PORT} (${WIDTH}x${HEIGHT}, ${DISPLAY})"

# Bind 0.0.0.0 INSIDE the container on purpose: docker port-publishing can't
# reach a 127.0.0.1 bind. The localhost-only security property is enforced by
# how the instance publishes the port (docker -p 127.0.0.1:PORT + firewall),
# which is the control plane's job, not the container's.
exec python -m uvicorn toolserver.app:app \
    --host 0.0.0.0 \
    --port "${ECU_TOOLSERVER_PORT}" \
    --app-dir "$HOME"
