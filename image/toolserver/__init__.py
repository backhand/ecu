"""ECU instance tool server.

A thin FastAPI wrapper around the anthropic-quickstarts computer-use-demo tool
implementations (``computer_use_demo.tools``). It exposes the computer-use tools
(screenshot / click / move / type / key / scroll / exec) as a localhost-bound
HTTP API, replacing the demo's Streamlit chat UI + agent loop.

The ASGI app lives at ``toolserver.app:app`` (the uvicorn target). See
``app.py`` for the application and ``README.md`` for the endpoint contract.

We intentionally do NOT re-export ``app`` here: doing so would shadow the
``toolserver.app`` submodule as a package attribute, which is a confusing
footgun. Always reference the app as ``toolserver.app:app``.
"""
