"""
test_ecu_client.py — stdlib unittest coverage for the three correctness fixes
in the ECU Python clients. No pytest dependency; run with:

    python -m unittest test_ecu_client -v          # from this directory
    python -m unittest discover -s . -p 'test_*.py' -v

The HTTP layer is mocked by replacing ``ECUClient._request`` with a small
scripted stub that (a) returns canned ``full``/``diff``/``nochange`` responses
and (b) records every outbound request body so we can assert exactly what hit
the wire. Full frames and diff region crops are real PNGs built with Pillow, so
diff reconstruction and downscaling are genuinely exercised — not stubbed.

Which test proves which fix:
  * Fix #3 (coordinate scaling)  -> test_coordinate_scaling_after_downscale
                                     (+ test_screenshot_records_scale_factor)
  * Fix #4 (nochange sentinel)   -> test_nochange_returns_cached_frame_without_refetch
                                     (client side) and
                                     test_mcp_screenshot_nochange_returns_text_sentinel
                                     (MCP tool side)
  * Fix #5 (CLI sidecar cache)   -> test_sidecar_cache_round_trip and
                                     test_sidecar_second_screenshot_sends_since
  * Supporting:                     test_diff_reconstruction_composites_region
"""

from __future__ import annotations

import base64
import io
import json
import os
import sys
import tempfile
import threading
import time
import unittest
from pathlib import Path

# Import the modules under test from the skill directory, regardless of where
# the test runner is invoked from, by adding this file's own directory to path.
_HERE = Path(__file__).resolve().parent
if str(_HERE) not in sys.path:
    sys.path.insert(0, str(_HERE))

from PIL import Image as PILImage  # noqa: E402

import ecu_client  # noqa: E402
from ecu_client import (  # noqa: E402
    ECUClient,
    Screenshot,
    Session,
    load_session_cache,
    save_session_cache,
)


# --------------------------------------------------------------------------
# Helpers
# --------------------------------------------------------------------------

def _png(width: int, height: int, color) -> bytes:
    """Build a solid-color RGB PNG of the given size and return its bytes."""
    im = PILImage.new("RGB", (width, height), color)
    buf = io.BytesIO()
    im.save(buf, format="PNG")
    return buf.getvalue()


def _jpeg(width: int, height: int, color, quality: int = 75) -> bytes:
    """Build a solid-color RGB JPEG (models the server's lossy wire encoding)."""
    im = PILImage.new("RGB", (width, height), color)
    buf = io.BytesIO()
    im.save(buf, format="JPEG", quality=quality)
    return buf.getvalue()


def _b64(data: bytes) -> str:
    return base64.b64encode(data).decode("ascii")


def _make_client() -> ECUClient:
    """An ECUClient with env satisfied and HTTP disabled (we patch _request)."""
    return ECUClient(base_url="https://ecu.test", api_key="k_test")


class ScriptedClient(ECUClient):
    """ECUClient whose _request is driven by a scripted list of responses and
    which records every outbound (method, path, json-body) tuple."""

    def __init__(self, responses):
        super().__init__(base_url="https://ecu.test", api_key="k_test")
        self._responses = list(responses)
        self.calls = []  # list of dicts: {method, path, body}

    def _request(self, method, path, **kwargs):  # type: ignore[override]
        self.calls.append(
            {"method": method, "path": path, "body": kwargs.get("json")}
        )
        if not self._responses:
            raise AssertionError(
                f"unexpected extra request: {method} {path} {kwargs.get('json')}"
            )
        return self._responses.pop(0)

    # Convenience: count how many screenshot POSTs were issued.
    def screenshot_calls(self):
        return [c for c in self.calls if c["path"].endswith("/screenshot")]


# --------------------------------------------------------------------------
# Supporting: diff reconstruction
# --------------------------------------------------------------------------

class DiffReconstructionTest(unittest.TestCase):
    def test_diff_reconstruction_composites_region(self):
        """A diff's region PNG must be composited onto the cached base at (x,y),
        leaving the rest of the frame unchanged. (Proves the diff machinery the
        other fixes build on still works.)"""
        # Base frame: 128x96 solid black.
        base_png = _png(128, 96, (0, 0, 0))
        # Diff region: a 20x10 solid-red patch placed at (40, 30).
        patch_png = _png(20, 10, (255, 0, 0))

        client = ScriptedClient([
            {"mode": "full", "frame_token": 1, "width": 128, "height": 96,
             "image": _b64(base_png)},
            {"mode": "diff", "frame_token": 2, "base_token": 1,
             "width": 128, "height": 96,
             "regions": [{"x": 40, "y": 30, "w": 20, "h": 10,
                          "image": _b64(patch_png)}]},
        ])
        sess = Session(session_id="s1", status="ready")

        # First the full frame establishes the base, then the diff.
        client.screenshot(sess, max_width=None)
        shot = client.screenshot(sess, max_width=None)
        self.assertEqual(shot.mode, "diff")

        out = PILImage.open(io.BytesIO(shot.png)).convert("RGB")
        # Pixel inside the patch is red.
        self.assertEqual(out.getpixel((45, 35)), (255, 0, 0))
        # Corner of the patch (top-left) is red.
        self.assertEqual(out.getpixel((40, 30)), (255, 0, 0))
        # Just outside the patch is still black.
        self.assertEqual(out.getpixel((39, 30)), (0, 0, 0))
        self.assertEqual(out.getpixel((60, 30)), (0, 0, 0))
        self.assertEqual(out.getpixel((0, 0)), (0, 0, 0))

    def test_lossy_diff_round_trip_with_native_dims(self):
        """A lossy (JPEG) full frame + lossy diff region reconstructs coherently,
        the returned image is normalized to PNG, and native dims drive scale.

        Models the server-side-downscale protocol: a 1280-native desktop shown
        at width 640, JPEG-encoded. Reconstruction must place the patch in the
        right spot (allowing for JPEG noise) and leave the rest near-background.
        """
        base_jpg = _jpeg(640, 400, (20, 20, 20))
        patch_jpg = _jpeg(40, 30, (200, 50, 50))
        client = ScriptedClient([
            {"mode": "full", "frame_token": 1, "width": 640, "height": 400,
             "native_width": 1280, "native_height": 800, "image": _b64(base_jpg)},
            {"mode": "diff", "frame_token": 2, "base_token": 1,
             "width": 640, "height": 400,
             "native_width": 1280, "native_height": 800,
             "regions": [{"x": 100, "y": 80, "w": 40, "h": 30,
                          "image": _b64(patch_jpg)}]},
        ])
        sess = Session(session_id="s1", status="ready")

        full = client.screenshot(sess, max_width=640)
        self.assertEqual(full.mode, "full")
        self.assertAlmostEqual(full.scale, 2.0)        # 1280 / 640
        # Returned bytes are normalized to PNG regardless of the JPEG wire format.
        self.assertEqual(PILImage.open(io.BytesIO(full.png)).format, "PNG")

        shot = client.screenshot(sess, max_width=640)
        self.assertEqual(shot.mode, "diff")
        self.assertAlmostEqual(shot.scale, 2.0)
        out = PILImage.open(io.BytesIO(shot.png)).convert("RGB")
        # Center of the patch is reddish (lossy, so compare with tolerance).
        r, g, b = out.getpixel((120, 95))
        self.assertGreater(r, 150)
        self.assertLess(g, 110)
        # A pixel well outside the patch stays near the dark background.
        r2, _, _ = out.getpixel((10, 10))
        self.assertLess(r2, 80)

    def test_diffs_do_not_accumulate_lossy_damage(self):
        """Cumulative-loss guard: unchanged content is held as decoded pixels and
        only changed regions are pasted, so an UNCHANGED corner is byte-identical
        across many successive diffs (never re-run through the lossy codec)."""
        base_jpg = _jpeg(128, 96, (30, 60, 90))
        # Each diff touches only a small patch far from the sampled corner.
        diffs = []
        for i in range(6):
            patch = _jpeg(16, 16, (10 * i, 20, 30))
            diffs.append(
                {"mode": "diff", "frame_token": i + 2, "base_token": i + 1,
                 "width": 128, "height": 96,
                 "regions": [{"x": 80, "y": 60, "w": 16, "h": 16,
                              "image": _b64(patch)}]}
            )
        client = ScriptedClient(
            [{"mode": "full", "frame_token": 1, "width": 128, "height": 96,
              "image": _b64(base_jpg)}] + diffs
        )
        sess = Session(session_id="s1", status="ready")

        first = client.screenshot(sess, max_width=None)
        corner0 = PILImage.open(io.BytesIO(first.png)).convert("RGB").getpixel((5, 5))
        for _ in range(6):
            shot = client.screenshot(sess, max_width=None)
            self.assertEqual(shot.mode, "diff")
            corner = PILImage.open(io.BytesIO(shot.png)).convert("RGB").getpixel((5, 5))
            # Pixel-exact: the corner never degrades no matter how many diffs.
            self.assertEqual(corner, corner0)


# --------------------------------------------------------------------------
# Fix #4 (client side): nochange returns cached frame without refetch
# --------------------------------------------------------------------------

class NoChangeTest(unittest.TestCase):
    def test_nochange_returns_cached_frame_without_refetch(self):
        """After a full frame, a ``nochange`` response returns the SAME cached
        PNG bytes and issues no extra image fetch."""
        base_png = _png(64, 64, (10, 20, 30))
        client = ScriptedClient([
            {"mode": "full", "frame_token": 7, "width": 64, "height": 64,
             "image": _b64(base_png)},
            {"mode": "nochange", "frame_token": 7},
        ])
        sess = Session(session_id="s1", status="ready")

        first = client.screenshot(sess, max_width=None)
        self.assertEqual(first.mode, "full")
        self.assertEqual(first.png, base_png)

        second = client.screenshot(sess, max_width=None)
        self.assertEqual(second.mode, "nochange")
        # Same cached bytes handed back.
        self.assertEqual(second.png, base_png)
        self.assertEqual(second.png, sess._base_png)

        # Exactly two screenshot POSTs total — the nochange path did NOT trigger
        # a recovery full-frame fetch (the cache was present).
        self.assertEqual(len(client.screenshot_calls()), 2)
        # And the script was fully consumed (no extra request smuggled in).
        self.assertEqual(client._responses, [])


# --------------------------------------------------------------------------
# Fix #3: coordinate scaling after a downscaled screenshot
# --------------------------------------------------------------------------

class CoordinateScalingTest(unittest.TestCase):
    # Under the server-side-downscale protocol the SERVER returns the already
    # downscaled image and reports both the shown `width`/`height` and the real
    # `native_width`/`native_height`. The client derives scale = native/shown
    # from the response (it no longer downscales locally), so these fixtures
    # model a 1280-native desktop shown at max_width=640 -> shown width 640.

    def test_screenshot_records_scale_factor(self):
        """A 1280-native frame shown at max_width=640 (server-downscaled to a
        640-wide image) yields scale 2.0 on both the Screenshot and Session."""
        shown = _png(640, 400, (5, 5, 5))   # server already downscaled
        client = ScriptedClient([
            {"mode": "full", "frame_token": 1, "width": 640, "height": 400,
             "native_width": 1280, "native_height": 800, "image": _b64(shown)},
        ])
        sess = Session(session_id="s1", status="ready")
        shot = client.screenshot(sess, max_width=640)

        self.assertEqual(shot.width, 640)              # shown width
        self.assertAlmostEqual(shot.scale, 2.0)
        self.assertAlmostEqual(sess.screenshot_scale, 2.0)

    def test_screenshot_forwards_encoding_params(self):
        """max_width/format/quality are sent on the screenshot REQUEST so the
        server downscales+encodes at the source (not the client)."""
        shown = _png(640, 400, (5, 5, 5))
        client = ScriptedClient([
            {"mode": "full", "frame_token": 1, "width": 640, "height": 400,
             "native_width": 1280, "native_height": 800, "image": _b64(shown)},
        ])
        sess = Session(session_id="s1", status="ready")
        client.screenshot(sess, max_width=640, format="webp", quality=60)
        body = client.calls[-1]["body"]
        self.assertEqual(body["max_width"], 640)
        self.assertEqual(body["format"], "webp")
        self.assertEqual(body["quality"], 60)

    def test_no_downscale_keeps_scale_one(self):
        """When the server reports native == shown (no downscale), scale=1.0."""
        shown = _png(800, 600, (5, 5, 5))
        client = ScriptedClient([
            {"mode": "full", "frame_token": 1, "width": 800, "height": 600,
             "native_width": 800, "native_height": 600, "image": _b64(shown)},
        ])
        sess = Session(session_id="s1", status="ready")
        shot = client.screenshot(sess, max_width=1280)
        self.assertAlmostEqual(shot.scale, 1.0)
        self.assertAlmostEqual(sess.screenshot_scale, 1.0)

    def test_coordinate_scaling_after_downscale(self):
        """The core of the coordinate fix: after a downscaled screenshot (scale
        2.0), a click at SHOWN coords (x, y) must send NATIVE coords (2x, 2y) to
        /click — and scroll deltas (dx, dy) must NOT be scaled."""
        shown = _png(640, 400, (5, 5, 5))
        client = ScriptedClient([
            {"mode": "full", "frame_token": 1, "width": 640, "height": 400,
             "native_width": 1280, "native_height": 800, "image": _b64(shown)},
            {"ok": True},   # click
            {"ok": True},   # move
            {"ok": True},   # scroll
        ])
        sess = Session(session_id="s1", status="ready")
        client.screenshot(sess, max_width=640)
        self.assertAlmostEqual(sess.screenshot_scale, 2.0)

        # This is exactly the call the MCP tool makes (scale=sess.screenshot_scale).
        client.click("s1", 320, 200, scale=sess.screenshot_scale)
        click_body = client.calls[-1]["body"]
        self.assertEqual(client.calls[-1]["path"], "/sessions/s1/click")
        self.assertEqual(click_body["x"], 640)   # 320 * 2
        self.assertEqual(click_body["y"], 400)   # 200 * 2

        client.move("s1", 100, 50, scale=sess.screenshot_scale)
        move_body = client.calls[-1]["body"]
        self.assertEqual(move_body["x"], 200)
        self.assertEqual(move_body["y"], 100)

        # scroll: anchor scaled, dx/dy left alone (scroll clicks, not coords).
        client.scroll("s1", 10, 20, dx=3, dy=-4, scale=sess.screenshot_scale)
        scroll_body = client.calls[-1]["body"]
        self.assertEqual(scroll_body["x"], 20)
        self.assertEqual(scroll_body["y"], 40)
        self.assertEqual(scroll_body["dx"], 3)    # unscaled
        self.assertEqual(scroll_body["dy"], -4)   # unscaled

    def test_scale_one_is_a_noop(self):
        """scale=1.0 (the default) passes coordinates straight through."""
        client = ScriptedClient([{"ok": True}])
        client.click("s1", 321, 201)  # default scale
        body = client.calls[-1]["body"]
        self.assertEqual((body["x"], body["y"]), (321, 201))


# --------------------------------------------------------------------------
# Protocol #5: batch/macro action endpoint (client `actions()`)
# --------------------------------------------------------------------------

class BatchActionsTest(unittest.TestCase):
    def test_batch_scales_each_anchor_and_does_not_mutate_input(self):
        """With scale 2.0, every click/move/scroll ANCHOR in the batch is doubled
        on the wire, while type/key fields and scroll dx/dy are untouched — and
        the caller's input list is NOT mutated in place."""
        # A downscaled full frame sets the session scale to 2.0 (1280 / 640).
        shown = _png(640, 400, (5, 5, 5))
        client = ScriptedClient([
            {"mode": "full", "frame_token": 1, "width": 640, "height": 400,
             "native_width": 1280, "native_height": 800, "image": _b64(shown)},
            {"results": [{"ok": True}, {"ok": True}, {"ok": True}, {"ok": True}]},
        ])
        sess = Session(session_id="s1", status="ready")
        client.screenshot(sess, max_width=640)
        self.assertAlmostEqual(sess.screenshot_scale, 2.0)

        original = [
            {"action": "click", "x": 320, "y": 200, "button": "right"},
            {"action": "move", "x": 100, "y": 50},
            {"action": "type", "text": "mailon.ai"},
            {"action": "scroll", "x": 10, "y": 20, "dx": 3, "dy": -4},
        ]
        # Snapshot a deep-ish copy to prove non-mutation afterwards.
        import copy
        before = copy.deepcopy(original)

        # scale defaults to None -> uses sess.screenshot_scale (2.0).
        out = client.actions(sess, original)

        sent = client.calls[-1]["body"]["actions"]
        self.assertEqual(client.calls[-1]["path"], "/sessions/s1/actions")
        # click anchor doubled; button preserved.
        self.assertEqual((sent[0]["x"], sent[0]["y"]), (640, 400))
        self.assertEqual(sent[0]["button"], "right")
        # move anchor doubled.
        self.assertEqual((sent[1]["x"], sent[1]["y"]), (200, 100))
        # type text untouched.
        self.assertEqual(sent[2]["text"], "mailon.ai")
        self.assertNotIn("x", sent[2])
        # scroll anchor doubled; dx/dy NOT scaled (scroll clicks).
        self.assertEqual((sent[3]["x"], sent[3]["y"]), (20, 40))
        self.assertEqual((sent[3]["dx"], sent[3]["dy"]), (3, -4))

        # The caller's list was not mutated (still the original shown-space coords).
        self.assertEqual(original, before)
        # results passed through verbatim; no trailing screenshot requested.
        self.assertEqual(len(out["results"]), 4)
        self.assertIsNone(out["screenshot"])

    def test_explicit_scale_overrides_session_scale(self):
        """An explicit scale arg overrides sess.screenshot_scale; scale=1.0 is a
        no-op even when the session recorded a downscale."""
        shown = _png(640, 400, (5, 5, 5))
        client = ScriptedClient([
            {"mode": "full", "frame_token": 1, "width": 640, "height": 400,
             "native_width": 1280, "native_height": 800, "image": _b64(shown)},
            {"results": [{"ok": True}]},
        ])
        sess = Session(session_id="s1", status="ready")
        client.screenshot(sess, max_width=640)
        self.assertAlmostEqual(sess.screenshot_scale, 2.0)

        client.actions(sess, [{"action": "click", "x": 11, "y": 22}], scale=1.0)
        sent = client.calls[-1]["body"]["actions"]
        self.assertEqual((sent[0]["x"], sent[0]["y"]), (11, 22))  # unscaled

    def test_batch_trailing_full_screenshot_reconstructs(self):
        """A batch whose response carries a `full` trailing screenshot returns a
        reconstructed Screenshot (right mode, decoded png, derived scale), and
        passes the per-action results through."""
        # 1280-native frame shown at width 640 -> scale 2.0; real PNG via _png.
        shown = _png(640, 400, (12, 34, 56))
        client = ScriptedClient([
            {"results": [{"ok": True}, {"ok": True}],
             "screenshot": {
                 "mode": "full", "frame_token": 7,
                 "width": 640, "height": 400,
                 "native_width": 1280, "native_height": 800,
                 "image": _b64(shown)}},
        ])
        sess = Session(session_id="s1", status="ready")

        out = client.actions(
            sess,
            [{"action": "click", "x": 5, "y": 5}, {"action": "key", "keys": "Return"}],
            screenshot={"format": "png", "max_width": 640},
        )

        # Per-action results passed through.
        self.assertEqual(out["results"], [{"ok": True}, {"ok": True}])
        # Trailing screenshot reconstructed as a real Screenshot.
        shot = out["screenshot"]
        self.assertIsInstance(shot, Screenshot)
        self.assertEqual(shot.mode, "full")
        self.assertEqual(shot.width, 640)
        self.assertAlmostEqual(shot.scale, 2.0)              # 1280 / 640
        self.assertAlmostEqual(sess.screenshot_scale, 2.0)   # recorded on session
        self.assertEqual(shot.frame_token, 7)
        # The png reconstructs to the shown frame (normalized to PNG).
        out_img = PILImage.open(io.BytesIO(shot.png)).convert("RGB")
        self.assertEqual(out_img.size, (640, 400))
        self.assertEqual(out_img.getpixel((0, 0)), (12, 34, 56))
        # The session now holds this as its base (frame_token + base png set).
        self.assertEqual(sess._base_token, 7)

    def test_batch_trailing_diff_screenshot_composites_onto_base(self):
        """Establish a base full frame first, then a batch whose trailing
        screenshot is a `diff`: the diff region must composite onto the session
        base (the same reconstruction screenshot() does)."""
        base_png = _png(128, 96, (0, 0, 0))
        patch_png = _png(20, 10, (255, 0, 0))
        client = ScriptedClient([
            # Establish the base via a normal screenshot.
            {"mode": "full", "frame_token": 1, "width": 128, "height": 96,
             "native_width": 128, "native_height": 96, "image": _b64(base_png)},
            # The batch's trailing screenshot is a diff against that base.
            {"results": [{"ok": True}],
             "screenshot": {
                 "mode": "diff", "frame_token": 2, "base_token": 1,
                 "width": 128, "height": 96,
                 "native_width": 128, "native_height": 96,
                 "regions": [{"x": 40, "y": 30, "w": 20, "h": 10,
                              "image": _b64(patch_png)}]}},
        ])
        sess = Session(session_id="s1", status="ready")
        client.screenshot(sess, max_width=None)  # base established

        out = client.actions(
            sess,
            [{"action": "type", "text": "x"}],
            screenshot={"max_width": None},
        )
        shot = out["screenshot"]
        self.assertIsInstance(shot, Screenshot)
        self.assertEqual(shot.mode, "diff")
        composed = PILImage.open(io.BytesIO(shot.png)).convert("RGB")
        # Patch composited at (40,30); surroundings still the black base.
        self.assertEqual(composed.getpixel((45, 35)), (255, 0, 0))
        self.assertEqual(composed.getpixel((39, 30)), (0, 0, 0))
        self.assertEqual(composed.getpixel((0, 0)), (0, 0, 0))
        self.assertEqual(sess._base_token, 2)

    def test_batch_error_returns_partial_results_and_no_screenshot(self):
        """When the batch errored (a result has ok=false and the response carries
        no screenshot key), actions() returns the partial results and
        screenshot=None — even if a trailing screenshot was requested."""
        client = ScriptedClient([
            {"results": [{"ok": True}, {"ok": False, "error": "unknown action: 'frob'"}]},
        ])
        sess = Session(session_id="s1", status="ready")
        out = client.actions(
            sess,
            [{"action": "click", "x": 1, "y": 2}, {"action": "frob"}],
            screenshot={"format": "png"},
        )
        self.assertEqual(len(out["results"]), 2)
        self.assertFalse(out["results"][1]["ok"])
        self.assertIsNone(out["screenshot"])


# --------------------------------------------------------------------------
# Capture-once-stable ("settle"): the settle controls are forwarded to the wire
# when requested, and the default-OFF wire stays clean.
# --------------------------------------------------------------------------

class SettleTest(unittest.TestCase):
    def test_screenshot_forwards_settle_params(self):
        """client.screenshot(..., settle_ms=350, max_wait_ms=4000) puts
        settle_ms and max_wait_ms on the screenshot REQUEST so the server runs
        the settle loop and caps the wait at the source."""
        shown = _png(640, 400, (5, 5, 5))
        client = ScriptedClient([
            {"mode": "full", "frame_token": 1, "width": 640, "height": 400,
             "native_width": 1280, "native_height": 800, "image": _b64(shown)},
        ])
        sess = Session(session_id="s1", status="ready")
        client.screenshot(sess, settle_ms=350, max_wait_ms=4000)
        body = client.calls[-1]["body"]
        self.assertEqual(body["settle_ms"], 350)
        self.assertEqual(body["max_wait_ms"], 4000)

    def test_settle_true_sugar_is_forwarded(self):
        """settle=True (no settle_ms) forwards settle:true so the server applies
        its default settle window."""
        shown = _png(640, 400, (5, 5, 5))
        client = ScriptedClient([
            {"mode": "full", "frame_token": 1, "width": 640, "height": 400,
             "native_width": 1280, "native_height": 800, "image": _b64(shown)},
        ])
        sess = Session(session_id="s1", status="ready")
        client.screenshot(sess, settle=True)
        body = client.calls[-1]["body"]
        self.assertTrue(body["settle"])
        # No explicit settle_ms/max_wait_ms when only the sugar was used.
        self.assertNotIn("settle_ms", body)
        self.assertNotIn("max_wait_ms", body)

    def test_plain_screenshot_omits_settle_keys(self):
        """A plain screenshot (no settle) keeps the wire default-OFF: none of
        settle/settle_ms/max_wait_ms appear in the request body."""
        shown = _png(640, 400, (5, 5, 5))
        client = ScriptedClient([
            {"mode": "full", "frame_token": 1, "width": 640, "height": 400,
             "native_width": 1280, "native_height": 800, "image": _b64(shown)},
        ])
        sess = Session(session_id="s1", status="ready")
        client.screenshot(sess)
        body = client.calls[-1]["body"]
        self.assertNotIn("settle", body)
        self.assertNotIn("settle_ms", body)
        self.assertNotIn("max_wait_ms", body)

    def test_actions_forwards_settle_in_screenshot_subdict(self):
        """A trailing screenshot dict carrying settle_ms is forwarded inside the
        actions() POST body's `screenshot` sub-dict, and the caller's dict is
        NOT mutated."""
        shown = _png(640, 400, (12, 34, 56))
        client = ScriptedClient([
            {"results": [{"ok": True}],
             "screenshot": {
                 "mode": "full", "frame_token": 7,
                 "width": 640, "height": 400,
                 "native_width": 1280, "native_height": 800,
                 "image": _b64(shown)}},
        ])
        sess = Session(session_id="s1", status="ready")
        shot_req = {"settle_ms": 300}
        import copy
        before = copy.deepcopy(shot_req)

        out = client.actions(
            sess, [{"action": "key", "keys": "Return"}], screenshot=shot_req
        )

        sent = client.calls[-1]["body"]
        self.assertEqual(client.calls[-1]["path"], "/sessions/s1/actions")
        # settle_ms forwarded verbatim inside the screenshot sub-dict.
        self.assertEqual(sent["screenshot"]["settle_ms"], 300)
        # The caller's screenshot dict was not mutated.
        self.assertEqual(shot_req, before)
        # The trailing frame still reconstructs through the normal path.
        self.assertIsInstance(out["screenshot"], Screenshot)
        self.assertEqual(out["screenshot"].mode, "full")

    def test_settle_bumps_per_request_timeout_for_large_cap(self):
        """A large max_wait_ms bumps the per-request HTTP timeout above the
        client default (so the client waits for the server), while a plain
        screenshot keeps the default timeout (no override)."""
        # Drive the REAL _request -> _do_request so the timeout override is
        # exercised; capture the timeout that reaches the requests call.
        captured: dict = {}

        class TimeoutProbeClient(ECUClient):
            def __init__(self):
                super().__init__(base_url="https://ecu.test", api_key="k_test")

            def _do_request(self, method, path, timeout=None, **kwargs):
                captured["timeout"] = timeout
                # Return a canned full frame; don't hit the network.
                shown = _png(64, 64, (1, 2, 3))
                return {
                    "mode": "full", "frame_token": 1, "width": 64, "height": 64,
                    "native_width": 64, "native_height": 64, "image": _b64(shown),
                }

        client = TimeoutProbeClient()
        sess = Session(session_id="s1", status="ready")

        # 60 s cap -> timeout bumped to 60 + margin(10) = 70 (> default 30).
        client.screenshot(sess, settle_ms=500, max_wait_ms=60000)
        self.assertEqual(captured["timeout"], 70.0)

        # Plain screenshot -> no override (None keeps the client default).
        client.screenshot(sess)
        self.assertIsNone(captured["timeout"])

        # A small settle cap (default ~2500 ms) stays under the default 30 s
        # timeout, so no override is applied there either.
        client.screenshot(sess, settle=True)
        self.assertIsNone(captured["timeout"])


# --------------------------------------------------------------------------
# Fix #5: CLI sidecar cache
# --------------------------------------------------------------------------

class SidecarCacheTest(unittest.TestCase):
    def setUp(self):
        # Point the cache at a throwaway dir so we never touch the real one.
        self._tmp = tempfile.TemporaryDirectory()
        self._prev_xdg = os.environ.get("XDG_CACHE_HOME")
        os.environ["XDG_CACHE_HOME"] = self._tmp.name

    def tearDown(self):
        if self._prev_xdg is None:
            os.environ.pop("XDG_CACHE_HOME", None)
        else:
            os.environ["XDG_CACHE_HOME"] = self._prev_xdg
        self._tmp.cleanup()

    def test_sidecar_cache_round_trip(self):
        """Saving a Session and loading it back preserves token, scale, native
        dims, and the base PNG bytes exactly."""
        base_png = _png(1280, 800, (1, 2, 3))
        sess = Session(session_id="s_round", status="ready", width=1280, height=800)
        sess.screenshot_scale = 2.0
        sess._base_token = 42
        sess._base_png = base_png

        save_session_cache("s_round", sess)

        # The cache lives under $XDG_CACHE_HOME/ecu.
        self.assertEqual(
            ecu_client.session_cache_dir(), Path(self._tmp.name) / "ecu"
        )

        loaded = load_session_cache("s_round")
        self.assertEqual(loaded._base_token, 42)
        self.assertAlmostEqual(loaded.screenshot_scale, 2.0)
        self.assertEqual(loaded.width, 1280)
        self.assertEqual(loaded.height, 800)
        self.assertEqual(loaded._base_png, base_png)

    def test_missing_cache_is_cold_session(self):
        """A never-seen session loads cold (no token, no base) so the next
        screenshot is a clean full frame."""
        cold = load_session_cache("s_never")
        self.assertIsNone(cold._base_token)
        self.assertIsNone(cold._base_png)
        self.assertAlmostEqual(cold.screenshot_scale, 1.0)

    def test_corrupt_cache_falls_back_to_cold(self):
        """A corrupt JSON sidecar must not crash; it yields a cold Session."""
        cache_dir = Path(self._tmp.name) / "ecu"
        cache_dir.mkdir(parents=True, exist_ok=True)
        (cache_dir / "s_bad.json").write_text("{not valid json")
        cold = load_session_cache("s_bad")
        self.assertIsNone(cold._base_token)
        self.assertIsNone(cold._base_png)

    def test_token_without_png_is_unusable(self):
        """If the JSON has a token but the base PNG is missing, the loaded
        session must drop the token (a token with no frame is unusable)."""
        cache_dir = Path(self._tmp.name) / "ecu"
        cache_dir.mkdir(parents=True, exist_ok=True)
        (cache_dir / "s_nopng.json").write_text(
            json.dumps({"frame_token": 9, "scale": 1.0, "width": 100, "height": 100})
        )
        loaded = load_session_cache("s_nopng")
        self.assertIsNone(loaded._base_token)
        self.assertIsNone(loaded._base_png)

    def test_sidecar_second_screenshot_sends_since(self):
        """End-to-end Fix #5: a first screenshot saved to the sidecar, then a
        SECOND screenshot hydrated from that cache, sends `since` equal to the
        cached frame_token (so the server can return the cheap diff/nochange
        path). Captures the outbound body to prove it."""
        base_png = _png(1280, 800, (9, 9, 9))

        # --- first CLI invocation: cold cache -> full frame, then persist ---
        client1 = ScriptedClient([
            {"mode": "full", "frame_token": 101, "width": 1280, "height": 800,
             "image": _b64(base_png)},
        ])
        sess1 = load_session_cache("s_e2e")          # cold
        self.assertIsNone(sess1._base_token)
        client1.screenshot(sess1, max_width=1280)
        save_session_cache("s_e2e", sess1)
        # First screenshot did NOT send `since` (nothing cached yet).
        self.assertNotIn("since", client1.calls[0]["body"])
        self.assertEqual(client1.calls[0]["body"]["mode"], "auto")

        # --- second CLI invocation: fresh process, hydrate from sidecar ---
        client2 = ScriptedClient([
            {"mode": "nochange", "frame_token": 101},
        ])
        sess2 = load_session_cache("s_e2e")          # warm
        self.assertEqual(sess2._base_token, 101)
        self.assertEqual(sess2._base_png, base_png)
        shot2 = client2.screenshot(sess2, max_width=1280)

        # The cheap path was taken and the cached frame returned.
        self.assertEqual(shot2.mode, "nochange")
        self.assertEqual(shot2.png, base_png)
        # The outbound body carried `since` == cached frame_token.
        body = client2.calls[0]["body"]
        self.assertEqual(body["since"], 101)


# --------------------------------------------------------------------------
# Fix #4 (MCP side) + Fix #3 (MCP side): drive the actual MCP module functions.
# --------------------------------------------------------------------------

class _FakeClient:
    """Stand-in for ECUClient used to drive the MCP tool functions directly.
    Records action calls and returns canned Screenshots from screenshot()."""

    def __init__(self):
        self.shots = []            # queued Screenshot objects to return
        self.action_calls = []     # (name, args, kwargs)

    def screenshot(self, sess, max_width=1280, force_full=False,
                   format="jpeg", quality=75):
        shot = self.shots.pop(0)
        # Mirror the real client: stamp the session scale so _get() reflects it.
        sess.screenshot_scale = shot.scale
        self._last = (max_width, force_full)
        return shot

    def click(self, session_id, x, y, button="left", scale=1.0):
        self.action_calls.append(("click", session_id, x, y, button, scale))

    def move(self, session_id, x, y, scale=1.0):
        self.action_calls.append(("move", session_id, x, y, scale))

    def scroll(self, session_id, x, y, dx=0, dy=0, scale=1.0):
        self.action_calls.append(("scroll", session_id, x, y, dx, dy, scale))


class McpServerTest(unittest.TestCase):
    def setUp(self):
        import mcp_server
        self.mcp_server = mcp_server
        self.fake = _FakeClient()
        # Reset module state and inject our fake client.
        mcp_server._sessions.clear()
        mcp_server._client = self.fake
        # In this FastMCP version @mcp.tool() registers the tool and returns the
        # original function, so the module attributes are directly callable.
        self.screenshot = mcp_server.screenshot
        self.click = mcp_server.click
        self.move = mcp_server.move
        self.scroll = mcp_server.scroll

    def tearDown(self):
        self.mcp_server._sessions.clear()
        self.mcp_server._client = None

    def test_mcp_screenshot_full_returns_image(self):
        from mcp.server.fastmcp import Image as MCPImage
        png = _png(64, 64, (1, 1, 1))
        self.fake.shots = [
            Screenshot(png=png, width=64, height=64, mode="full",
                       scale=1.0, frame_token=5),
        ]
        result = self.screenshot("s1", max_width=1280)
        self.assertIsInstance(result, MCPImage)

    def test_mcp_screenshot_nochange_returns_text_sentinel(self):
        """Fix #4 (MCP): on the nochange path the tool returns a short TEXT
        sentinel (with the frame token) instead of an Image."""
        png = _png(64, 64, (1, 1, 1))
        self.fake.shots = [
            Screenshot(png=png, width=64, height=64, mode="nochange",
                       scale=1.0, frame_token=88),
        ]
        result = self.screenshot("s1", max_width=1280)
        self.assertIsInstance(result, str)
        self.assertIn("no change", result.lower())
        self.assertIn("88", result)   # frame token surfaced

    def test_mcp_force_full_is_forwarded(self):
        png = _png(64, 64, (1, 1, 1))
        self.fake.shots = [
            Screenshot(png=png, width=64, height=64, mode="full",
                       scale=1.0, frame_token=5),
        ]
        self.screenshot("s1", max_width=1280, force_full=True)
        self.assertEqual(self.fake._last, (1280, True))

    def test_mcp_click_uses_session_scale(self):
        """Fix #3 (MCP): a downscaled screenshot sets the session scale, and a
        subsequent click passes that scale to the client (so native coords are
        sent). Drives the real mcp_server.screenshot + click functions."""
        native = _png(1280, 800, (2, 2, 2))
        # A real Screenshot with scale 2.0 (as the real client would produce).
        self.fake.shots = [
            Screenshot(png=native, width=640, height=400, mode="full",
                       scale=2.0, frame_token=1),
        ]
        self.screenshot("s1", max_width=640)
        # The session cached in mcp_server now carries scale 2.0.
        self.assertAlmostEqual(self.mcp_server._get("s1").screenshot_scale, 2.0)

        self.click("s1", 320, 200, button="left")
        name, sid, x, y, button, scale = self.fake.action_calls[-1]
        self.assertEqual(name, "click")
        self.assertAlmostEqual(scale, 2.0)
        # The MCP tool passes shown coords + scale; the (mocked) client would
        # multiply. Assert the scale wiring is correct.
        self.assertEqual((x, y), (320, 200))

        self.scroll("s1", 10, 20, dx=1, dy=2)
        name, sid, x, y, dx, dy, scale = self.fake.action_calls[-1]
        self.assertEqual(name, "scroll")
        self.assertAlmostEqual(scale, 2.0)
        self.assertEqual((dx, dy), (1, 2))


# --------------------------------------------------------------------------
# Protocol #4 (client side): per-session request serialization
# --------------------------------------------------------------------------

class SerializationClient(ECUClient):
    """ECUClient that exercises the REAL per-session lock.

    It overrides ``_do_request`` (the inner HTTP call), NOT ``_request``, so the
    lock-acquiring ``_request`` -> ``_serialize`` wrapper is genuinely run. Each
    inner call records concurrency per session: it bumps an active-counter for
    the session, sleeps a beat so overlapping calls actually coexist in time,
    tracks the max concurrency ever seen for that session, then decrements. If
    the lock works, same-session max concurrency stays at 1.
    """

    def __init__(self, hold: float = 0.05):
        super().__init__(base_url="https://ecu.test", api_key="k_test")
        self._hold = hold
        self._counter_guard = threading.Lock()
        self.active: dict[str, int] = {}       # session_id -> currently inside
        self.max_active: dict[str, int] = {}   # session_id -> peak concurrency
        self.global_active = 0
        self.global_max = 0

    def _do_request(self, method, path, **kwargs):  # type: ignore[override]
        sid = self._session_id_from_path(path) or "<none>"
        with self._counter_guard:
            self.active[sid] = self.active.get(sid, 0) + 1
            self.max_active[sid] = max(self.max_active.get(sid, 0), self.active[sid])
            self.global_active += 1
            self.global_max = max(self.global_max, self.global_active)
        try:
            time.sleep(self._hold)  # hold the (per-session) lock long enough to overlap
            return {"ok": True}
        finally:
            with self._counter_guard:
                self.active[sid] -= 1
                self.global_active -= 1


def _run_concurrently(targets):
    """Start one thread per callable, run them all, and join."""
    threads = [threading.Thread(target=t) for t in targets]
    for t in threads:
        t.start()
    for t in threads:
        t.join()


class PerSessionSerializationTest(unittest.TestCase):
    def test_same_session_requests_are_serialized(self):
        """N concurrent calls to the SAME session never overlap: the per-session
        lock keeps max observed concurrency at exactly 1 (no two requests to one
        session are ever in flight together)."""
        client = SerializationClient(hold=0.05)
        n = 6
        _run_concurrently([
            (lambda: client.click("s1", 1, 2)) for _ in range(n)
        ])
        # Never more than one request in flight at a time for s1.
        self.assertEqual(client.max_active.get("s1"), 1,
                         "two requests to the same session overlapped — not serialized")

    def test_different_sessions_run_in_parallel(self):
        """Different sessions are NOT serialized against each other: requests to
        s1 and s2 overlap in time (global peak concurrency > 1), proving the lock
        is per-session, not global."""
        client = SerializationClient(hold=0.1)
        # Two threads, different sessions, fired together — they must coexist.
        _run_concurrently([
            (lambda: client.click("s1", 1, 1)),
            (lambda: client.move("s2", 2, 2)),
        ])
        # Each session saw at most one in-flight (trivially true: one call each)...
        self.assertEqual(client.max_active.get("s1"), 1)
        self.assertEqual(client.max_active.get("s2"), 1)
        # ...but globally the two ran concurrently — different sessions parallelize.
        self.assertEqual(client.global_max, 2,
                         "different sessions were serialized — lock is too coarse")

    def test_mixed_load_serializes_per_session_only(self):
        """A mixed burst: many calls across two sessions. Each session is
        internally serialized (max 1 each) while the two sessions overlap
        globally (peak 2). This is the exact property the fix guarantees."""
        client = SerializationClient(hold=0.03)
        targets = []
        for _ in range(4):
            targets.append(lambda: client.click("sA", 0, 0))
            targets.append(lambda: client.screenshot_action_probe("sB"))
        _run_concurrently(targets)
        self.assertEqual(client.max_active.get("sA"), 1)
        self.assertEqual(client.max_active.get("sB"), 1)
        self.assertEqual(client.global_max, 2)

    def test_reentrant_same_session_does_not_deadlock(self):
        """The per-session lock is reentrant (RLock): the screenshot nochange/
        diff recovery path re-enters _request for the SAME session on the SAME
        thread. A non-reentrant lock would self-deadlock; this proves it does
        not (the nested same-session call returns)."""
        client = SerializationClient(hold=0.0)
        done = {}

        # Simulate re-entry: from inside one same-session request, issue another
        # same-session request on the same thread (exactly what the recursive
        # screenshot() recovery does). With an RLock this returns; a plain Lock
        # would hang here forever.
        outer_do = client._do_request

        def reentrant_do(method, path, **kwargs):
            sid = client._session_id_from_path(path)
            if sid == "s1" and "nested" not in done:
                done["nested"] = True
                # Nested same-session call goes back through the locking _request.
                client._request("POST", "/sessions/s1/move", json={"x": 0, "y": 0})
            return outer_do(method, path, **kwargs)

        client._do_request = reentrant_do  # type: ignore[method-assign]

        finished = threading.Event()

        def go():
            client.click("s1", 5, 5)
            finished.set()

        t = threading.Thread(target=go)
        t.start()
        t.join(timeout=5)
        self.assertTrue(finished.is_set(),
                        "reentrant same-session request deadlocked (lock not RLock)")
        self.assertTrue(done.get("nested"))


# Small convenience used by test_mixed_load_serializes_per_session_only so both
# branches of the mixed burst go through the locking _request path with a known
# session id. (A thin alias over the action POST.)
def _screenshot_action_probe(self, session_id):
    return self._request("POST", f"/sessions/{session_id}/click", json={"x": 0, "y": 0})


SerializationClient.screenshot_action_probe = _screenshot_action_probe  # type: ignore[attr-defined]


if __name__ == "__main__":
    unittest.main(verbosity=2)
