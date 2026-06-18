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
    def test_screenshot_records_scale_factor(self):
        """A 1280-wide native frame shown at max_width=640 yields scale 2.0 on
        both the Screenshot and the Session."""
        native = _png(1280, 800, (5, 5, 5))
        client = ScriptedClient([
            {"mode": "full", "frame_token": 1, "width": 1280, "height": 800,
             "image": _b64(native)},
        ])
        sess = Session(session_id="s1", status="ready")
        shot = client.screenshot(sess, max_width=640)

        self.assertEqual(shot.width, 640)              # shown width
        self.assertAlmostEqual(shot.scale, 2.0)
        self.assertAlmostEqual(sess.screenshot_scale, 2.0)

    def test_no_downscale_keeps_scale_one(self):
        """When the native frame is already <= max_width, scale stays 1.0."""
        native = _png(800, 600, (5, 5, 5))
        client = ScriptedClient([
            {"mode": "full", "frame_token": 1, "width": 800, "height": 600,
             "image": _b64(native)},
        ])
        sess = Session(session_id="s1", status="ready")
        shot = client.screenshot(sess, max_width=1280)
        self.assertAlmostEqual(shot.scale, 1.0)
        self.assertAlmostEqual(sess.screenshot_scale, 1.0)

    def test_coordinate_scaling_after_downscale(self):
        """The core of Fix #3: after a downscaled screenshot (scale 2.0), a click
        at SHOWN coords (x, y) must send NATIVE coords (2x, 2y) to /click — and
        scroll deltas (dx, dy) must NOT be scaled."""
        native = _png(1280, 800, (5, 5, 5))
        client = ScriptedClient([
            {"mode": "full", "frame_token": 1, "width": 1280, "height": 800,
             "image": _b64(native)},
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

    def screenshot(self, sess, max_width=1280, force_full=False):
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


if __name__ == "__main__":
    unittest.main(verbosity=2)
