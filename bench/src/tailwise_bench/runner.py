"""Scores each run once, through Tailwise, using the cache first."""

from __future__ import annotations

import asyncio
import hashlib
import json
import sys
from dataclasses import dataclass, field
from typing import Any

from .budget import BudgetExceeded
from .cache import TRACE, ScoreCache
from .client import ScoringClient
from .config import Config
from .otel import _value, run_trace
from .tokens import Estimator
from .types import Run

Key = tuple[str, str, str, str, str]

TAILWISE_MAX_STATE_CHARS = 40000  # trace.MaxStateChars: Tailwise cuts longer states to fit
# Rough sizes of the action checks beyond the user's request, for estimates:
# an action check's lookups, action and question; completeness questions.
ACTION_CHECK_TOKENS = 900
COMPLETENESS_TOKENS_PER_ACTION = 70


def key(run: Run, payload: dict[str, Any], cfg: Config) -> Key:
    """The settings that change a score, and the exact request: a change to
    what the harness sends re-scores the run."""
    sent = hashlib.sha256(json.dumps(payload, sort_keys=True).encode()).hexdigest()[:16]
    return (run.dataset, run.trace_id, TRACE, cfg.tailwise.model, f"{cfg.score_hash()}:{sent}")


def tokens_for(run: Run, state: str, est: Estimator, cfg: Config) -> int:
    """Estimated input tokens of the call: the state as shown, plus, with the
    grounded evidence strategy, the tool results it adds (about 4 characters
    per token)."""
    tokens = est.call(shown(state, cfg)[:TAILWISE_MAX_STATE_CHARS])
    actions = cfg.actions
    if actions:
        # The action checks: one call per action call (its request, lookups and
        # neighbours), and one for completeness (request, actions, answer).
        question = est.count(run.question)
        tokens += sum(question + ACTION_CHECK_TOKENS for c in run.tool_calls if c.name in actions)
        turns = sum(est.count(text[:600] if role == "assistant" else text) for role, text in run.messages)
        tokens += (turns or question) + est.count(run.final_answer) + COMPLETENESS_TOKENS_PER_ACTION * len(actions)
    if cfg.tailwise.evidence == "grounded":
        outputs = (sum(len(_value(s, "output.value")) for s in run.otlp_spans
                       if _value(s, "openinference.span.kind") == "TOOL")
                   if run.otlp_spans else sum(len(c.output) for c in run.tool_calls))
        tokens += min(cfg.tailwise.evidence_budget, outputs) // 4
    return tokens


def shown(state: str, cfg: Config) -> str:
    """The state as Tailwise shows it: without the system prompt unless the
    scorer is set to show it."""
    if cfg.tailwise.system_prompt or not state.startswith("SYSTEM PROMPT:\n"):
        return state
    return state[state.index("USER QUESTION:\n"):]


@dataclass
class RunResult:
    results: dict[str, dict[str, Any]] = field(default_factory=dict)  # by trace_id
    errors: dict[str, str] = field(default_factory=dict)
    cached: int = 0
    scored: int = 0
    aborted: str = ""


def estimate(runs: list[Run], est: Estimator, cache: ScoreCache, cfg: Config) -> dict[str, dict[str, int]]:
    """Projected calls and input tokens per dataset and subset; cached runs cost nothing."""
    out: dict[str, dict[str, int]] = {}
    for run in runs:
        row = out.setdefault(f"{run.dataset}/{run.subset}", {"calls": 0, "tokens": 0, "cached": 0})
        payload, state = run_trace(run)
        if cache.get(key(run, payload, cfg)) is not None:
            row["cached"] += 1
            continue
        row["calls"] += 1
        row["tokens"] += tokens_for(run, state, est, cfg)
    return out


async def run(runs: list[Run], client: ScoringClient, cache: ScoreCache, est: Estimator, cfg: Config) -> RunResult:
    out = RunResult()
    queue: asyncio.Queue[Run] = asyncio.Queue()
    for r in runs:
        hit = cache.get(key(r, run_trace(r)[0], cfg))
        if hit is not None:
            out.results[r.trace_id] = {**hit, "cached": True}
            out.cached += 1
        else:
            queue.put_nowait(r)
    todo, done = queue.qsize(), 0

    async def worker() -> None:
        nonlocal done
        while not out.aborted:
            try:
                r = queue.get_nowait()
            except asyncio.QueueEmpty:
                return
            payload, state = run_trace(r)
            tokens = tokens_for(r, state, est, cfg)
            try:
                o = await client.score(payload, tokens)
            except BudgetExceeded as e:
                out.aborted = str(e)
                return
            if o.result is None:
                out.errors[r.trace_id] = o.error
            else:
                result = {**o.result, "estimated_tokens": tokens}
                cache.put(key(r, payload, cfg), result)
                out.results[r.trace_id] = {**result, "cached": False}
                out.scored += 1
            done += 1
            if done % 100 == 0 or done == todo:
                print(f"  scored {done}/{todo} (spent ${client.spent_usd:.4f})", file=sys.stderr)

    await asyncio.gather(*(worker() for _ in range(max(1, cfg.client.concurrency))))
    return out
