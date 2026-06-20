#!/usr/bin/env python3
"""
ecu_cli.py — CLI fallback for ECU (Easy Computer Use).

Same capabilities as the MCP server, exposed as shell subcommands, for custom
agent loops or environments without MCP. Reads ECU_URL / ECU_API_KEY from env.

Examples:
    export ECU_URL=https://ecu.example.com
    export ECU_API_KEY=...

    sid=$(python ecu_cli.py start)
    python ecu_cli.py screenshot "$sid" --out screen.png   # then look at screen.png
    python ecu_cli.py click "$sid" 640 400
    python ecu_cli.py type "$sid" "hello world"
    python ecu_cli.py key "$sid" ctrl+l
    python ecu_cli.py exec "$sid" "xdg-open https://example.com >/dev/null 2>&1 &"
    python ecu_cli.py watch "$sid"
    python ecu_cli.py end "$sid"

The standard image ships Firefox ESR (no Chrome): open a URL with `xdg-open
<url>` (image-agnostic) or `firefox-esr <url>`, backgrounded with `&` so exec
returns at once.

Screenshots are saved to disk (a file path is the friendliest thing for a shell
loop). Use --b64 to print base64 to stdout instead.

Each invocation is a fresh process, so the CLI keeps a small per-session sidecar
cache (under $XDG_CACHE_HOME/ecu, else ~/.cache/ecu) holding the last frame
token, the reconstructed base frame, the downscale scale, and the native
dimensions. This lets a repeated `screenshot` send `since` (getting the cheap
diff/nochange path) and lets `click`/`move`/`scroll` translate coordinates from
the downscaled screenshot space back to native pixels — so a click after a
`--max-width` screenshot lands where you saw the target. The cache is
best-effort; a missing or corrupt cache just falls back to a full frame.
"""

from __future__ import annotations

import argparse
import sys
from typing import Optional

from ecu_client import (
    ECUClient,
    ECUError,
    Session,
    load_session_cache,
    save_session_cache,
)


def _client(args=None) -> ECUClient:
    """Build the ECUClient, applying the shared --timeout/--retries overrides.

    Both flags live on the common parent parser (so every subcommand accepts
    them); they map straight onto ``ECUClient(timeout=, retries=)``. Each is
    passed only when the caller actually set it, so an unset flag falls back to
    the client's own default. ``args`` is optional so a bare ``_client()`` (no
    overrides) still works.
    """
    kwargs = {}
    if args is not None:
        if getattr(args, "timeout", None) is not None:
            kwargs["timeout"] = args.timeout
        if getattr(args, "retries", None) is not None:
            kwargs["retries"] = args.retries
    try:
        return ECUClient(**kwargs)
    except ECUError as e:
        print(f"error: {e}", file=sys.stderr)
        raise SystemExit(2)


def _scale_for_action(args) -> float:
    """Resolve the coordinate scale for a click/move/scroll subcommand.

    The CLI is stateless per process, so the scale comes from the sidecar cache
    written by the previous `screenshot` (Fix #5). Precedence:

      1. If `--max-width` is given AND the sidecar knows the native width, derive
         the scale the way the screenshot would have:
             scale = native_width / min(max_width, native_width)
         This makes a line like `click "$sid" 320 200 --max-width 640` land
         correctly even when invoked on its own, as long as a prior screenshot
         cached the native dimensions.
      2. Otherwise use the scale the prior screenshot persisted.
      3. If neither is available (cold cache), scale = 1.0 (no-op) — coordinates
         are sent through unchanged.

    `--full-res` forces native space (scale 1.0), matching a full-res screenshot.
    """
    if getattr(args, "full_res", False):
        return 1.0
    sess = load_session_cache(args.session_id)
    max_width: Optional[int] = getattr(args, "max_width", None)
    if max_width and sess.width:
        native_width = sess.width
        return native_width / min(max_width, native_width)
    return sess.screenshot_scale


def cmd_start(args):
    c = _client(args)
    sess = c.start_session(
        persistent=args.persistent, restore=args.restore, wait=not args.no_wait
    )
    # Print just the id on stdout so it's easy to capture in a shell variable;
    # extra detail goes to stderr.
    print(sess.session_id)
    print(
        f"status={sess.status} size={sess.width}x{sess.height} "
        f"persistent={sess.persistent}",
        file=sys.stderr,
    )


def cmd_status(args):
    c = _client(args)
    st = c.get_status(args.session_id)
    for k, v in st.items():
        print(f"{k}={v}")


def cmd_screenshot(args):
    c = _client(args)
    # Hydrate from the sidecar so this fresh process can send `since` and get the
    # cheap diff/nochange path, then persist the updated frame + scale + dims for
    # the next invocation (and for click/move/scroll coordinate scaling).
    sess = load_session_cache(args.session_id)
    # Settle controls (capture-once-stable): wait for the screen to stop changing
    # instead of grabbing a possibly mid-render frame. `--settle` is the on/off
    # switch; the *_ms flags default to None (off) and we map an explicit 0 to
    # None too, so "unset" and "0" both mean "let the server/default decide".
    settle_ms = args.settle_ms if args.settle_ms else None
    max_wait_ms = args.max_wait_ms if args.max_wait_ms else None
    shot = c.screenshot(
        sess,
        max_width=(None if args.full_res else args.max_width),
        settle=args.settle,
        settle_ms=settle_ms,
        max_wait_ms=max_wait_ms,
    )
    save_session_cache(args.session_id, sess)
    if args.b64:
        print(shot.b64())
    else:
        path = shot.save(args.out)
        print(path)
        print(
            f"mode={shot.mode} size={shot.width}x{shot.height} scale={shot.scale:g}",
            file=sys.stderr,
        )


def cmd_click(args):
    scale = _scale_for_action(args)
    _client(args).click(args.session_id, args.x, args.y, args.button, scale=scale)
    print("ok")


def cmd_move(args):
    scale = _scale_for_action(args)
    _client(args).move(args.session_id, args.x, args.y, scale=scale)
    print("ok")


def cmd_type(args):
    _client(args).type(args.session_id, args.text)
    print("ok")


def cmd_key(args):
    _client(args).key(args.session_id, args.keys)
    print("ok")


def cmd_scroll(args):
    scale = _scale_for_action(args)
    _client(args).scroll(args.session_id, args.x, args.y, args.dx, args.dy, scale=scale)
    print("ok")


def cmd_exec(args):
    # --timeout here is the SERVER-side run budget (seconds); unset -> None ->
    # the server's 120 s default. (The shared --timeout flag doubles as the exec
    # run budget; exec's own client HTTP timeout is derived from it in the
    # client.) stdout->stdout, stderr->stderr, and we exit with the command code.
    res = _client(args).exec(
        args.session_id, args.command, timeout=getattr(args, "timeout", None)
    )
    code = res.get("code", res.get("exit_code", 0))
    if res.get("stdout"):
        sys.stdout.write(res["stdout"])
    if res.get("stderr"):
        sys.stderr.write(res["stderr"])
    raise SystemExit(int(code) if isinstance(code, int) else 0)


def cmd_watch(args):
    st = _client(args).get_status(args.session_id)
    url = st.get("watch_url")
    print(url or "no watch URL available")


def cmd_end(args):
    res = _client(args).end_session(args.session_id)
    print(res.get("status", "terminated"))


def _add_scale_flags(s: argparse.ArgumentParser) -> None:
    """Add coordinate-scaling flags shared by click/move/scroll.

    Coordinates are normally taken in the space of the prior screenshot (the
    scale is read from the sidecar cache). These flags let a caller state the
    space explicitly, mirroring the screenshot flags:

      --max-width N  the prior screenshot was downscaled to width N; combined
                     with the cached native width this derives the exact scale
                     (scale = native / min(N, native)). Matches the E2E usage
                     `click "$sid" 320 200 --max-width 640`.
      --full-res     coordinates are native (scale 1.0); no rescale.
    """
    s.add_argument("--max-width", type=int, default=None,
                   help="coords are in a screenshot downscaled to this width")
    s.add_argument("--full-res", action="store_true",
                   help="coords are native resolution (no rescale)")


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(prog="ecu", description="ECU computer-use CLI")
    sub = p.add_subparsers(dest="cmd", required=True)

    # Shared options every subcommand accepts, via parents=[common]. Keeping them
    # on a parent (rather than only on the top-level parser) means they work
    # AFTER the subcommand name too — e.g. `ecu screenshot "$sid" --timeout 60`.
    #   --timeout  per-request HTTP timeout override -> ECUClient(timeout=). On
    #              the `exec` subcommand it doubles as the server-side run budget.
    #   --retries  total attempts for retryable (safe) ops -> ECUClient(retries=).
    common = argparse.ArgumentParser(add_help=False)
    common.add_argument(
        "--timeout", type=int, default=None,
        help="per-request HTTP timeout in seconds (also exec's run budget)",
    )
    common.add_argument(
        "--retries", type=int, default=None,
        help="total attempts for safe/idempotent calls (status/screenshot/end)",
    )

    s = sub.add_parser("start", parents=[common], help="provision a computer")
    s.add_argument("--persistent", action="store_true",
                   help="request a persistent session (privileged)")
    s.add_argument("--restore", default=None, help="restore from a persistent session id")
    s.add_argument("--no-wait", action="store_true", help="don't wait for ready")
    s.set_defaults(func=cmd_start)

    s = sub.add_parser("status", parents=[common], help="session status")
    s.add_argument("session_id")
    s.set_defaults(func=cmd_status)

    s = sub.add_parser("screenshot", parents=[common], help="capture the screen")
    s.add_argument("session_id")
    s.add_argument("--out", default="screenshot.png", help="output PNG path")
    s.add_argument("--b64", action="store_true", help="print base64 to stdout instead")
    s.add_argument("--max-width", type=int, default=1280, help="downscale width")
    s.add_argument("--full-res", action="store_true", help="no downscaling")
    # Capture-once-stable: wait for the screen to stop changing before grabbing.
    s.add_argument("--settle", action="store_true",
                   help="wait until the screen stops changing, then capture")
    s.add_argument("--settle-ms", type=int, default=None,
                   help="settle window in ms (implies --settle); unset=server default")
    s.add_argument("--max-wait-ms", type=int, default=None,
                   help="hard cap on the settle wait in ms (never hangs)")
    s.set_defaults(func=cmd_screenshot)

    s = sub.add_parser("click", parents=[common], help="click at x y")
    s.add_argument("session_id"); s.add_argument("x", type=int); s.add_argument("y", type=int)
    s.add_argument("--button", default="left", choices=["left", "right", "middle"])
    _add_scale_flags(s)
    s.set_defaults(func=cmd_click)

    s = sub.add_parser("move", parents=[common], help="move cursor to x y")
    s.add_argument("session_id"); s.add_argument("x", type=int); s.add_argument("y", type=int)
    _add_scale_flags(s)
    s.set_defaults(func=cmd_move)

    s = sub.add_parser("type", parents=[common], help="type text")
    s.add_argument("session_id"); s.add_argument("text")
    s.set_defaults(func=cmd_type)

    s = sub.add_parser("key", parents=[common], help="press a key/chord, e.g. ctrl+l")
    s.add_argument("session_id"); s.add_argument("keys")
    s.set_defaults(func=cmd_key)

    s = sub.add_parser("scroll", parents=[common], help="scroll at x y")
    s.add_argument("session_id"); s.add_argument("x", type=int); s.add_argument("y", type=int)
    s.add_argument("--dx", type=int, default=0); s.add_argument("--dy", type=int, default=0)
    _add_scale_flags(s)
    s.set_defaults(func=cmd_scroll)

    s = sub.add_parser("exec", parents=[common], help="run a shell command on the computer")
    s.add_argument("session_id"); s.add_argument("command")
    s.set_defaults(func=cmd_exec)

    s = sub.add_parser("watch", parents=[common], help="get a human watch URL")
    s.add_argument("session_id")
    s.set_defaults(func=cmd_watch)

    s = sub.add_parser("end", parents=[common], help="tear down the computer (always do this)")
    s.add_argument("session_id")
    s.set_defaults(func=cmd_end)

    return p


def main(argv=None):
    args = build_parser().parse_args(argv)
    try:
        args.func(args)
    except ECUError as e:
        print(f"error: {e}", file=sys.stderr)
        raise SystemExit(1)


if __name__ == "__main__":
    main()
