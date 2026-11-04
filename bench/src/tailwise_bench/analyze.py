"""Scores and labels joined into one row per run, and the summary metrics.

Two decision rules are compared, both from the same call:

- default: Tailwise as configured (built-in questions: keep when any check
  reaches the threshold); the score is p_keep.
- answer_wrong: the decision question alone, read from the recorded answers.

A run Tailwise could not score (Jev rejected it, e.g. as too long) counts as
production treats it: dropped (there is no random floor here) and last in
any review order.
"""

from __future__ import annotations

from collections import Counter
from typing import Any

from . import metrics
from .config import Config
from .types import Run

DECISION_QUESTION = "answer_wrong"


def rows(runs: list[Run], results: dict[str, dict[str, Any]], errors: dict[str, str], cfg: Config,
         questions: list[str]) -> list[dict[str, Any]]:
    out = []
    for run in runs:
        r = results.get(run.trace_id)
        scored = r is not None and r.get("p_keep") is not None
        answers = (r or {}).get("answers") or {}
        aw = answers.get(DECISION_QUESTION)
        tokens = int((r or {}).get("tokens", 0))
        row: dict[str, Any] = {
            "dataset": run.dataset, "subset": run.subset, "run_id": run.run_id, "trace_id": run.trace_id,
            "failed": int(run.failed), "tool_calls": len(run.tool_calls),
            "p_keep": float(r["p_keep"]) if scored and r else None,
            "kept": int(r["decision"] == "keep") if scored and r else None,
            "answer_wrong": aw,
            "kept_answer_wrong_only": int(aw >= cfg.tailwise.keep_threshold) if aw is not None else None,
            "flags": "; ".join((r or {}).get("flags") or []),
            "severity": (r or {}).get("severity", 0.0) if scored else None,
            "cause": (r or {}).get("cause", ""),
            "tokens": tokens, "tokens_source": (r or {}).get("tokens_source", ""), "cost": cfg.cost.usd(tokens),
            "error": errors.get(run.trace_id, ""),
        }
        for q in questions:
            row[f"answer_{q}"] = answers.get(q)
        for k, v in run.meta.items():
            if isinstance(v, str | int | float):
                row[f"meta_{k}"] = v
        out.append(row)
    return out


def _rule(rows_: list[dict[str, Any]], score: str, kept: str, cfg: Config) -> dict[str, Any]:
    y = [r["failed"] for r in rows_]
    s = [r[score] if r[score] is not None else 0.0 for r in rows_]
    pred = [r[kept] or 0 for r in rows_]
    return {
        "at_threshold": {"threshold": cfg.tailwise.keep_threshold,
                         **metrics.threshold_metrics(y, pred, cfg.reference_failure_rates)},
        **metrics.ranking_metrics(y, s),
        "budget_curve": metrics.budget_curve(y, s, cfg.budgets, cfg.random_seeds, cfg.seed),
        "calibration": metrics.calibration(y, s),
    }


def group_summary(rows_: list[dict[str, Any]], cfg: Config) -> dict[str, Any]:
    scored = [r for r in rows_ if r["p_keep"] is not None]
    y = [r["failed"] for r in scored]
    default = _rule(rows_, "p_keep", "kept", cfg)
    tokens = sum(r["tokens"] for r in rows_)
    caught = default["at_threshold"]["tp"]
    return {
        "runs": len(rows_), "runs_scored": len(scored), "runs_failed_to_score": len(rows_) - len(scored),
        "failed": sum(r["failed"] for r in rows_),
        "failure_rate": sum(r["failed"] for r in rows_) / len(rows_) if rows_ else None,
        "default": default,
        "answer_wrong_only": _rule(rows_, "answer_wrong", "kept_answer_wrong_only", cfg),
        "flags": metrics.flag_stats(y, [[f for f in r["flags"].split("; ") if f] for r in scored]),
        "causes": {
            "failed_runs": dict(Counter(r["cause"] or "(none)" for r in scored if r["failed"]).most_common()),
            "successful_runs": dict(Counter(r["cause"] or "(none)" for r in scored if not r["failed"]).most_common()),
        },
        "tokens": tokens, "cost_usd": cfg.cost.usd(tokens),
        "cost_per_run_usd": cfg.cost.usd(tokens) / len(rows_) if rows_ else None,
        "cost_per_failure_caught_usd": cfg.cost.usd(tokens) / caught if caught else None,
    }


def summary(rows_: list[dict[str, Any]], cfg: Config) -> dict[str, Any]:
    """Per dataset, per dataset subset, and every run pooled. A dataset whose
    runs are labelled with meta_trace_detectable (τ-bench) also gets its
    trace-detectable runs alone, under "detectable": the runs whose label a
    trace-only reviewer could judge."""
    out: dict[str, Any] = {"datasets": {}, "subsets": {}, "detectable": {}}
    for ds in sorted({r["dataset"] for r in rows_}):
        out["datasets"][ds] = group_summary([r for r in rows_ if r["dataset"] == ds], cfg)
        detectable = [r for r in rows_ if r["dataset"] == ds and r.get("meta_trace_detectable") == 1]
        if detectable:
            out["detectable"][ds] = group_summary(detectable, cfg)
        for sub in sorted({r["subset"] for r in rows_ if r["dataset"] == ds}):
            out["subsets"][f"{ds}/{sub}"] = group_summary(
                [r for r in rows_ if r["dataset"] == ds and r["subset"] == sub], cfg)
    if len(out["datasets"]) > 1:
        out["all"] = group_summary(rows_, cfg)
    return out


def roc_pr_curves(rows_: list[dict[str, Any]]) -> dict[str, Any]:
    out = {}
    for ds in sorted({r["dataset"] for r in rows_}):
        sc = [r for r in rows_ if r["dataset"] == ds and r["p_keep"] is not None]
        c = metrics.curves([r["failed"] for r in sc], [r["p_keep"] for r in sc]) if sc else None
        if c:
            out[ds] = c
    return out
