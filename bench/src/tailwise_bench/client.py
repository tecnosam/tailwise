"""Scores traces through Tailwise's POST /v1/otlp/traces:score.

Rate limited, with exponential backoff on 429 and 5xx. A fallback decision
(Tailwise could not get a score from Jev) is retried once. A 422 (nothing to
score) or other 4xx is final.
"""

from __future__ import annotations

import asyncio
import random
import time
from dataclasses import dataclass
from typing import Any

import httpx

from .budget import BudgetGuard
from .config import Client as ClientConfig

PATH = "/v1/otlp/traces:score"
RETRY_STATUS = {429, 500, 502, 503, 504}


@dataclass
class Outcome:
    result: dict[str, Any] | None  # Tailwise's response; None on failure
    error: str = ""
    attempts: int = 0


class RateLimiter:
    """At most rate starts per second, spaced evenly."""

    def __init__(self, rate: float) -> None:
        self._interval = 1.0 / rate if rate > 0 else 0.0
        self._next = 0.0
        self._lock = asyncio.Lock()

    async def wait(self) -> None:
        async with self._lock:
            now = time.monotonic()
            if self._next > now:
                await asyncio.sleep(self._next - now)
            self._next = max(now, self._next) + self._interval


class ScoringClient:
    def __init__(self, base_url: str, cfg: ClientConfig, budget: BudgetGuard, seed: int) -> None:
        self._http = httpx.AsyncClient(base_url=base_url, timeout=cfg.timeout_seconds)
        self._cfg = cfg
        self._budget = budget
        self._limiter = RateLimiter(cfg.requests_per_second)
        self._rng = random.Random(seed)

    @property
    def spent_usd(self) -> float:
        return self._budget.spent_usd

    async def close(self) -> None:
        await self._http.aclose()

    async def score(self, payload: dict[str, Any], estimate: int) -> Outcome:
        """One trace scored; raises BudgetExceeded before a call that would go
        over the cap."""
        last = ""
        fallbacks = 0
        for attempt in range(1, self._cfg.max_attempts + 1):
            held = self._budget.reserve(estimate)
            billed: int | None = None
            try:
                await self._limiter.wait()
                resp = await self._http.post(PATH, json=payload)
                if resp.status_code == 200:
                    body = resp.json()
                    if body.get("rule") != "fallback":
                        # Jev reports what it billed; if it ever does not, the
                        # estimate stands in, and the row says so.
                        reported = int(body.get("input_tokens") or 0)
                        body["tokens"] = reported or estimate
                        body["tokens_source"] = "api" if reported else "estimate"
                        billed = body["tokens"]
                        return Outcome(result=body, attempts=attempt)
                    # Tailwise already retried Jev; a second fallback is most
                    # likely a rejection (e.g. a trace too long for Jev), which
                    # retrying does not change.
                    last = f"fallback: {body.get('reason', '')}"
                    fallbacks += 1
                    if fallbacks >= 2:
                        return Outcome(result=None, error=last, attempts=attempt)
                elif resp.status_code in RETRY_STATUS:
                    last = f"HTTP {resp.status_code}: {resp.text[:200]}"
                else:
                    return Outcome(result=None, error=f"HTTP {resp.status_code}: {resp.text[:300]}", attempts=attempt)
            except httpx.TransportError as e:
                last = f"{type(e).__name__}: {e}"
            finally:
                self._budget.settle(held, estimate, billed)
            if attempt < self._cfg.max_attempts:
                delay = self._cfg.backoff_seconds * 2 ** (attempt - 1)
                await asyncio.sleep(delay * (0.5 + self._rng.random()))
        return Outcome(result=None, error=last, attempts=self._cfg.max_attempts)
