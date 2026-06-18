"""Host-only unit tests for the settle decision logic (``settle_logic``).

These run OUTSIDE the container — ``settle_logic`` has no FastAPI / numpy /
``computer_use_demo`` imports, so a plain ``python -m unittest`` can exercise the
"unchanged-for-N-ms or elapsed-past-cap" state machine that ``app.py``'s
``_capture_settled_png`` loop relies on, without building the image. The IO loop
(capture/decode/compare/sleep) is the container's job and is demonstrated live by
the settle smoke; THIS proves the pure stop/continue arithmetic in isolation.

    python -m unittest test_settle_logic -v          # from this directory
"""

from __future__ import annotations

import sys
import unittest
from pathlib import Path

_HERE = Path(__file__).resolve().parent
if str(_HERE) not in sys.path:
    sys.path.insert(0, str(_HERE))

from settle_logic import SettleDecider  # noqa: E402


class SettleDeciderTest(unittest.TestCase):
    def test_settles_after_unchanged_window(self):
        """A run of unchanged grabs reaching the window returns settled exactly
        when the accumulated still-time first hits settle_ms."""
        d = SettleDecider(settle_ms=300, max_wait_ms=2500)
        # Two 100 ms still steps: 200 ms < 300 ms, not settled yet.
        self.assertFalse(d.step(changed=False, dt_ms=100))
        self.assertFalse(d.step(changed=False, dt_ms=100))
        # Third 100 ms still step: 300 ms >= 300 ms -> settled.
        self.assertTrue(d.step(changed=False, dt_ms=100))

    def test_change_resets_the_accumulator(self):
        """A change mid-run resets the unchanged accumulator, so the window must
        be re-accumulated from scratch (a moving screen never settles early)."""
        d = SettleDecider(settle_ms=300, max_wait_ms=2500)
        self.assertFalse(d.step(changed=False, dt_ms=200))  # 200 ms still
        self.assertFalse(d.step(changed=True, dt_ms=100))   # change -> reset to 0
        self.assertFalse(d.step(changed=False, dt_ms=200))  # 200 ms again, < 300
        self.assertTrue(d.step(changed=False, dt_ms=100))   # now 300 ms -> settled

    def test_capped_triggers_at_budget(self):
        """``capped`` is False below the cap and True at/over it, regardless of
        the (never-settling) accumulator — the no-hang guard for an animating
        screen."""
        d = SettleDecider(settle_ms=300, max_wait_ms=2500)
        # Simulate a constantly-changing screen: every step resets, never settles.
        for _ in range(40):
            d.step(changed=True, dt_ms=100)
        self.assertFalse(d.capped(2499.0))
        self.assertTrue(d.capped(2500.0))
        self.assertTrue(d.capped(3000.0))

    def test_cap_floored_at_settle_window(self):
        """A max_wait below the settle window is floored to the window, so a
        settled exit is still reachable (the cap can't pre-empt settle below it)."""
        d = SettleDecider(settle_ms=500, max_wait_ms=100)
        self.assertEqual(d.max_wait_ms, 500.0)
        # Not capped before the floored budget; the still-run can reach settle.
        self.assertFalse(d.capped(499.0))
        self.assertFalse(d.step(changed=False, dt_ms=400))
        self.assertTrue(d.step(changed=False, dt_ms=100))

    def test_zero_window_settles_immediately_on_first_still(self):
        """settle_ms==0 (an edge the resolver never actually produces, but the
        decider should be total) settles on the first non-change."""
        d = SettleDecider(settle_ms=0, max_wait_ms=1000)
        self.assertTrue(d.step(changed=False, dt_ms=0))


if __name__ == "__main__":
    unittest.main(verbosity=2)
