# ECU computer-use instance image.
#
# Builds on the anthropic computer-use-demo image, which already ships the full
# desktop stack (Xvfb + tint2 + mutter + x11vnc + noVNC) and -- critically --
# the computer-use tool executors in computer_use_demo/tools/ (ComputerTool,
# BashTool). We inherit all of that and layer on:
#
#   * a small FastAPI tool server (toolserver/) that wraps the demo's EXISTING
#     ComputerTool, exposing screenshot/click/move/type/key/scroll/exec/watch
#     over a localhost-bound HTTP API; and
#   * an entrypoint that runs the demo's desktop+VNC+noVNC startup and then
#     launches uvicorn instead of Streamlit (we want the tools as a network API,
#     not the demo's human chat UI / agent loop).
#
# The base image is amd64-only; build (and run) with --platform linux/amd64.
FROM ghcr.io/anthropics/anthropic-quickstarts:computer-use-demo-latest

# The base image runs as the unprivileged "computeruse" user with HOME at
# /home/computeruse and a pyenv-managed Python on PATH. Match it.
WORKDIR /home/computeruse

# Install the tool-server deps into the same pyenv Python the demo tools use.
# Pinned for reproducibility; versions verified compatible with the image's
# pydantic 2.x / anyio 4.x / starlette range. httpx + websockets back the
# /watch noVNC reverse proxy (Component 9): httpx streams the noVNC page/assets,
# websockets bridges the websockify WebSocket. Both already ship transitively in
# the base image, but we pin them explicitly so the tool server's runtime deps
# are self-documenting and reproducible.
RUN python -m pip install --no-cache-dir \
        "fastapi==0.115.6" \
        "uvicorn[standard]==0.34.0" \
        "httpx==0.28.1" \
        "websockets==16.0"

# Our additions. Copy with the right ownership so the unprivileged user can
# read them.
COPY --chown=computeruse:computeruse toolserver/ /home/computeruse/toolserver/
COPY --chown=computeruse:computeruse entrypoint.sh /home/computeruse/ecu_entrypoint.sh
RUN chmod +x /home/computeruse/ecu_entrypoint.sh

# Resolution and tool-server port. WIDTH/HEIGHT default to the ECU brief's
# 1280x800 (the base image defaults to 1024x768); override at build or run.
ENV WIDTH=1280 \
    HEIGHT=800 \
    ECU_TOOLSERVER_PORT=8000

# Tool server (8000) and noVNC (6080, inherited). These are documentation only;
# the instance publishes them bound to 127.0.0.1 (Component 4).
EXPOSE 8000 6080

# Replace the demo's ENTRYPOINT (which launched Streamlit) with ours (uvicorn).
ENTRYPOINT ["./ecu_entrypoint.sh"]
