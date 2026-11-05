"""The hard spending cap.

Before a call, its estimated cost is reserved; a call whose reservation would
take spent + reserved past the cap is never made. After the call, the
reservation is replaced by what Jev billed. Estimates can be low, so the
guard also keeps a safety margin of the largest estimate-to-actual ratio
seen so far.
"""

from __future__ import annotations

import threading
from dataclasses import dataclass


class BudgetExceeded(Exception):
    pass


@dataclass
class Spend:
    tokens: int = 0
    calls: int = 0
    estimated_tokens: int = 0  # of the calls billed, what was estimated


class BudgetGuard:
    def __init__(self, cap_usd: float, usd_per_token: float) -> None:
        self.cap_usd = cap_usd
        self.usd_per_token = usd_per_token
        self.spend = Spend()
        self._reserved = 0
        self._ratio = 1.0
        self._lock = threading.Lock()

    @property
    def spent_usd(self) -> float:
        return self.spend.tokens * self.usd_per_token

    def reserve(self, estimate: int) -> int:
        """Reserve a call; raises BudgetExceeded instead of going over."""
        with self._lock:
            held = int(estimate * self._ratio) + 1
            if (self.spend.tokens + self._reserved + held) * self.usd_per_token > self.cap_usd:
                raise BudgetExceeded(
                    f"the next call would take spending past the ${self.cap_usd:.2f} cap "
                    f"(spent ${self.spent_usd:.4f}, {self._reserved} tokens in flight)")
            self._reserved += held
            return held

    def settle(self, held: int, estimate: int, actual: int | None) -> None:
        """Replace a reservation by the billed tokens, None if the call failed
        before Jev billed anything."""
        with self._lock:
            self._reserved -= held
            if actual is None:
                return
            self.spend.tokens += actual
            self.spend.calls += 1
            self.spend.estimated_tokens += estimate
            if estimate > 0:
                self._ratio = min(4.0, max(self._ratio, actual / estimate))
