"""Launch the ECU tool server with uvicorn.

Usage: ``python -m toolserver`` (the image entrypoint calls uvicorn directly,
but this makes the package runnable standalone for local debugging).

Binds 0.0.0.0 on purpose -- see app.py. Port is configurable via
``ECU_TOOLSERVER_PORT`` (default 8000).
"""

import os

import uvicorn

if __name__ == "__main__":
    uvicorn.run(
        "toolserver.app:app",
        host="0.0.0.0",  # noqa: S104 - intentional; localhost-binding is done at publish time
        port=int(os.getenv("ECU_TOOLSERVER_PORT") or 8000),
        log_level=os.getenv("ECU_TOOLSERVER_LOG_LEVEL", "info"),
    )
