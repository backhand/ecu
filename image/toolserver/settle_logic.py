"""Pure decision logic for the capture-once-stable ("settle") loop.

Factored into its OWN module — with no FastAPI / numpy / ``computer_use_demo``
imports — so the settle state machine (the "unchanged-for-N-ms or elapsed-past-
cap" accumulator) can be unit-tested on the host, outside the container, where
``app.py`` cannot be imported. ``app.py``'s ``_capture_settled_png`` owns the IO
(capture through the gate, decode, tile-compare, ``asyncio.sleep``) and feeds the
per-step result here; THIS module owns only the timing arithmetic and the
stop/continue decision, so there is one source of truth for "are we settled yet,
or out of budget?" and it is testable in isolation.
"""

from __future__ import annotations


class SettleDecider:
    """Accumulator state machine for one settle loop.

    Construct with the effective settle window and the hard cap (both in ms).
    The caller drives it grab-by-grab: after each new capture it calls
    :meth:`step` with whether the screen CHANGED vs the previous grab and how
    long elapsed since that previous grab (``dt_ms``); ``step`` returns ``True``
    once the screen has been continuously unchanged for ``settle_ms`` (settled).
    Independently, before sleeping for the next grab the caller checks
    :meth:`capped` against the total elapsed time so an endlessly-changing screen
    returns at the cap and never hangs.

    The two exits are deliberately separate so the IO loop can check the cap
    BEFORE paying for another sleep+capture (bounding total wall-clock) and the
    settle accumulator only advances on observed stillness. ``max_wait`` is
    floored at ``settle_ms`` so a cap below the window can't make settle
    unreachable.
    """

    def __init__(self, settle_ms: float, max_wait_ms: float) -> None:
        self.settle_ms = float(settle_ms)
        # A cap below the settle window would make a settled exit impossible;
        # floor it so settle stays reachable (mirrors app.py's clamp).
        self.max_wait_ms = max(float(max_wait_ms), float(settle_ms))
        self.unchanged_ms = 0.0

    def capped(self, elapsed_ms: float) -> bool:
        """True when total elapsed has reached the hard cap (return latest frame)."""
        return elapsed_ms >= self.max_wait_ms

    def step(self, changed: bool, dt_ms: float) -> bool:
        """Fold one grab into the accumulator; return True once settled.

        ``changed`` is whether this grab differs from the previous one;
        ``dt_ms`` is the time since the previous grab. A change resets the
        unchanged accumulator (the screen is still moving); stillness adds
        ``dt_ms`` toward the window. Returns ``True`` the moment the accumulated
        unchanged time reaches ``settle_ms``.
        """
        if changed:
            self.unchanged_ms = 0.0
        else:
            self.unchanged_ms += float(dt_ms)
        return self.unchanged_ms >= self.settle_ms
