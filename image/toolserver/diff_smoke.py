#!/usr/bin/env python3
"""Smoke test for the diff-aware /screenshot protocol (Component 6).

Run against a tool-server container that is already up and healthy (see the
"Screenshot diff-protocol smoke test" section in image/README.md):

    docker run -d --name ecu-c6 --platform linux/amd64 \\
        -p 127.0.0.1:8000:8000 ecu-image:dev
    # wait for /healthz ...
    python3 image/toolserver/diff_smoke.py
    docker rm -f ecu-c6

It proves the four protocol behaviors:
  1. full       — first capture (note token T0)
  2. nochange   — static idle desktop, since=T0 -> nochange, token unchanged
  3. diff       — small static change; base + regions reconstructs EXACTLY
                  (0 differing pixels vs a forced-full of the same state),
                  printing the on-the-wire byte savings
  4. full       — whole-screen change falls back to full (>=90% of tiles changed)

Reconstruction mirrors skill/ecu-computer-use/ecu_client.py._apply_diff:
composite each region PNG onto the cached base at (x,y), then compare to a
forced-full of the same settled screen.

Needs numpy + Pillow (both ship in the image, so this can also run *inside* the
container). The container name is read from $ECU_SMOKE_CONTAINER (default
"ecu-c6") and the base URL from $ECU_SMOKE_URL (default http://localhost:8000).

Test discipline worth knowing: once a base is set with a forced-full we take NO
more full captures until the change is made and the single diff request is
issued -- every forced-full advances the server's base token, and the server
(correctly) returns `full` when `since` != its current base. Changes are made
deterministic and blink-free (still `display` image windows; a Tk fullscreen
window) and allowed to settle before measuring.
"""
from __future__ import annotations

import base64
import io
import json
import os
import subprocess
import sys
import time
import urllib.request

import numpy as np
from PIL import Image

BASE = os.getenv("ECU_SMOKE_URL", "http://localhost:8000")
CTR = os.getenv("ECU_SMOKE_CONTAINER", "ecu-c6")


def post(path: str, body: dict) -> dict:
    data = json.dumps(body).encode()
    req = urllib.request.Request(
        BASE + path, data=data, headers={"content-type": "application/json"}
    )
    with urllib.request.urlopen(req, timeout=60) as r:
        return json.loads(r.read().decode())


def dexec_bg(cmd: str) -> None:
    subprocess.run(
        ["docker", "exec", "-d", CTR, "bash", "-lc", f"export DISPLAY=:1; {cmd}"],
        check=True,
    )


def dexec(cmd: str):
    return subprocess.run(
        ["docker", "exec", CTR, "bash", "-lc", f"export DISPLAY=:1; {cmd}"],
        capture_output=True, text=True,
    )


def rgb(b64: str) -> np.ndarray:
    return np.asarray(
        Image.open(io.BytesIO(base64.b64decode(b64))).convert("RGB"), dtype=np.uint8
    )


def reconstruct(base_rgb: np.ndarray, regions: list[dict]) -> np.ndarray:
    """Client-side _apply_diff: paste each region PNG onto the cached base."""
    base = Image.fromarray(base_rgb.copy(), mode="RGB")
    for r in regions:
        patch = Image.open(io.BytesIO(base64.b64decode(r["image"]))).convert("RGB")
        base.paste(patch, (int(r["x"]), int(r["y"])))
    return np.asarray(base, dtype=np.uint8)


def diffpix(a: np.ndarray, b: np.ndarray) -> int:
    return int(np.count_nonzero(np.any(a != b, axis=2)))


def settle_base() -> tuple[int, np.ndarray]:
    """Force fulls until two consecutive frames are pixel-identical; return
    (token, rgb) of the final full. The screen is now static, so `since=token`
    reads `nochange` until something actually changes."""
    prev = None
    d = None
    for _ in range(30):
        d = post("/screenshot", {"mode": "full"})
        cur = rgb(d["image"])
        if prev is not None and diffpix(prev, cur) == 0:
            return d["frame_token"], cur
        prev = cur
        time.sleep(0.7)
    raise RuntimeError("screen never went stable")


def settle_changed_screen(timeout: float = 20) -> None:
    """Wait until the screen is visually static again after a change, using
    forced-fulls. These advance the server base, so call this BEFORE setting the
    base we will diff against."""
    prev = None
    deadline = time.time() + timeout
    while time.time() < deadline:
        cur = rgb(post("/screenshot", {"mode": "full"})["image"])
        if prev is not None and diffpix(prev, cur) == 0:
            return
        prev = cur
        time.sleep(0.7)
    raise RuntimeError("changed screen never went stable")


# Deterministic whole-screen change: a borderless, 100%-coverage Tk fullscreen
# window (Tk honours EWMH fullscreen under mutter, unlike a plain `display`
# window which mutter clamps and letterboxes). Random-noise content dirties the
# entire frame so the coverage full-fallback (>=90% of tiles) fires.
_TKNOISE = (
    "cat > /tmp/tknoise.py <<'PY'\n"
    "import tkinter as tk\n"
    "import numpy as np\n"
    "from PIL import Image, ImageTk\n"
    "arr = np.random.default_rng(7).integers(0,256,size=(800,1280,3),dtype=np.uint8)\n"
    "r = tk.Tk(); r.title('tknoise'); r.attributes('-fullscreen', True); r.configure(cursor='none')\n"
    "img = ImageTk.PhotoImage(Image.fromarray(arr,'RGB'))\n"
    "lbl = tk.Label(r, image=img, borderwidth=0, highlightthickness=0); lbl.pack(fill='both', expand=True)\n"
    "r.update(); r.mainloop()\n"
    "PY\n"
    "python3 /tmp/tknoise.py"
)


def main() -> int:
    fail = 0

    print("=" * 72)
    print("1) FULL — first capture")
    r0 = post("/screenshot", {})
    assert r0["mode"] == "full", r0
    t0 = r0["frame_token"]
    print(f"   mode={r0['mode']} frame_token={t0} {r0['width']}x{r0['height']} "
          f"png_bytes={len(base64.b64decode(r0['image']))}")

    print("=" * 72)
    print("2) NOCHANGE — static idle screen")
    tb, _ = settle_base()
    rnc = post("/screenshot", {"since": tb})
    print(f"   settled token={tb}; POST since={tb} -> mode={rnc['mode']} "
          f"frame_token={rnc.get('frame_token')}")
    if rnc["mode"] == "nochange" and rnc["frame_token"] == tb:
        print("   OK: nochange and token did NOT advance")
    else:
        print(f"   FAIL: expected nochange with frame_token=={tb}")
        fail += 1

    print("=" * 72)
    print("3) DIFF — small static change; reconstruct must be pixel-identical")
    # Put a small still window up FIRST and let it settle so the base contains
    # it; then recolor only that window -> a tiny dirty area.
    dexec_bg("convert -size 200x150 xc:'#cc2244' /tmp/p.png && "
             "display -geometry 200x150+250+200 -title ecupatch -immutable /tmp/p.png")
    time.sleep(4)
    settle_changed_screen()
    base_token, base_rgb = settle_base()
    print(f"   base set (token={base_token}); making a small static change")
    dexec("pkill -f 'display.*ecupatch' || true")
    time.sleep(0.5)
    dexec_bg("convert -size 200x150 xc:'#22aa55' /tmp/p.png && "
             "display -geometry 200x150+250+200 -title ecupatch -immutable /tmp/p.png")
    # Let the recolor settle, then take exactly ONE diff request against the held
    # base (a diff advances the base, so there is no second shot -- this mirrors
    # the real client's re-base).
    time.sleep(6)
    rd = post("/screenshot", {"since": base_token})
    print(f"   POST since={base_token} -> mode={rd['mode']} "
          f"frame_token={rd.get('frame_token')} base_token={rd.get('base_token')} "
          f"regions={len(rd.get('regions', []))}")
    if rd["mode"] != "diff":
        print(f"   FAIL: expected diff, got {rd['mode']}")
        fail += 1
    else:
        if rd["base_token"] != base_token:
            print(f"   FAIL: base_token {rd['base_token']} != since {base_token}")
            fail += 1
        region_bytes = sum(len(base64.b64decode(x["image"])) for x in rd["regions"])
        recon = reconstruct(base_rgb, rd["regions"])
        truth = post("/screenshot", {"mode": "full"})
        truth_rgb = rgb(truth["image"])
        truth_bytes = len(base64.b64decode(truth["image"]))
        dp = diffpix(recon, truth_rgb)
        print("   regions(x,y,w,h): "
              f"{[(x['x'], x['y'], x['w'], x['h']) for x in rd['regions']]}")
        print(f"   reconstruct(base+regions) vs forced-full differing pixels = {dp}")
        print(f"   byte savings: region_bytes={region_bytes}  full_bytes={truth_bytes} "
              f" -> {100 * (1 - region_bytes / truth_bytes):.1f}% smaller on the wire")
        if dp == 0:
            print("   OK: reconstruction is PIXEL-IDENTICAL")
        else:
            print(f"   FAIL: {dp} pixels differ after reconstruction")
            fail += 1

    print("=" * 72)
    print("4) FULL-FALLBACK — whole-screen change (diff would exceed a full frame)")
    dexec("pkill -f 'display|tk' || true")
    time.sleep(2)
    settle_changed_screen()
    wt, _ = settle_base()
    print(f"   base set (token={wt}); changing the WHOLE screen")
    dexec_bg(_TKNOISE)
    time.sleep(7)
    rf = post("/screenshot", {"since": wt})
    print(f"   POST since={wt} -> mode={rf['mode']} frame_token={rf.get('frame_token')}")
    if rf["mode"] == "full":
        print("   OK: whole-screen change fell back to FULL")
    elif rf["mode"] == "diff":
        rb = sum(len(base64.b64decode(x["image"])) for x in rf["regions"])
        fb = len(base64.b64decode(post("/screenshot", {"mode": "full"})["image"]))
        print(f"   FAIL: expected full, got diff region_bytes={rb} full_bytes={fb}")
        fail += 1
    else:
        print(f"   FAIL: expected full, got {rf['mode']}")
        fail += 1

    print("=" * 72)
    print("RESULT:", "ALL CHECKS PASSED" if fail == 0 else f"{fail} CHECK(S) FAILED")
    return 1 if fail else 0


if __name__ == "__main__":
    sys.exit(main())
