"""Scores and labels joined into one row per run, and the summary metrics.

Two decision rules are compared, both from the same call:

- default: Tailwise as configured (built-in questions: keep when any check
  reaches the threshold); runs are ranked for review by severity, the
  deciding questions' average answer.
- answer_wrong: the decision question alone, read from the recorded answers.

Severity is computed here from the recorded answers, as Tailwise computes it,
so a change to how Tailwise combines answers does not need the runs scored
again; for runs scored in this run (not from the cache), it is checked against
the severity Tailwise returned.

A run Tailwise could not score (Jev rejected it, e.g. as too long) counts as
production treats it: dropped (there is no random floor here) and last in
any review order.
"""

from __future__ import annotations

from collections import Counter
from collections.abc import Sequence
from typing import Any

from . import metrics
from .config import Config
from .types import Run

DECISION_QUESTION = "answer_wrong"


def severity(answers: dict[str, float | None], deciding: list[str], signals: Sequence[float] = ()) -> float:
    """Tailwise's severity: the average answer of the deciding questions that
    answered, a failed check left out, with each signal rule's 1 or 0
    (internal/pipeline/policy.go)."""
    got = [*signals, *(a for a in (answers.get(q) for q in deciding) if a is not None)]
    return sum(got) / len(got) if got else 0.0


def apply_rules(row: dict[str, Any], verdicts: list[dict[str, Any]], answers: dict[str, float | None],
                deciding: list[str]) -> None:
    """The row as Tailwise with these rule verdicts would decide it
    (internal/pipeline/engine.go): a keep rule keeps the trace at severity 1
    without asking Jev, so its tokens are not spent; signal rules join the
    severity average. The answers are kept, for the comparison rows."""
    keep = any(v["fired"] and v["keep"] for v in verdicts)
    signals = [1.0 if v["fired"] else 0.0 for v in verdicts if not v["keep"]]
    row["rules_fired"] = "; ".join(v["name"] for v in verdicts if v["fired"])
    row["rule_keep"] = int(keep)
    row["severity_rules"] = 1.0 if keep else (sum(signals) / len(signals) if signals else 0.0)
    if keep:
        row["severity"], row["kept"], row["tokens"], row["cost"] = 1.0, 1, 0, 0.0
    elif row["severity"] is not None:
        row["severity"] = severity(answers, deciding, signals)


def task_group(run: Run) -> str:
    """The task a run attempted: runs of one task are resampled together.
    τ-bench repeats each task in several trials and SWE-agent each issue
    across models; a TRAIL run is its own task."""
    m = run.meta
    if "task_id" in m:
        return f"{run.dataset}:{m.get('domain', '')}:{m['task_id']}"
    if "instance_id" in m:
        return f"{run.dataset}:{m['instance_id']}"
    return f"{run.dataset}:{run.run_id}"


def rows(runs: list[Run], results: dict[str, dict[str, Any]], errors: dict[str, str], cfg: Config,
         questions: list[str], verdicts: dict[str, list[dict[str, Any]]] | None = None) -> list[dict[str, Any]]:
    """One row per run. With rule verdicts (by trace_id), severity, kept and
    tokens are as Tailwise with the rules would have them; severity_jev and
    kept_jev are without them."""
    deciding = cfg.deciding_questions(DECISION_QUESTION)
    out = []
    for run in runs:
        r = results.get(run.trace_id)
        scored = r is not None and r.get("p_keep") is not None
        answers = (r or {}).get("answers") or {}
        aw = answers.get(DECISION_QUESTION)
        tokens = int((r or {}).get("tokens", 0))
        row: dict[str, Any] = {
            "dataset": run.dataset, "subset": run.subset, "run_id": run.run_id, "trace_id": run.trace_id,
            "task": task_group(run),
            "failed": int(run.failed), "tool_calls": len(run.tool_calls),
            "p_keep": float(r["p_keep"]) if scored and r else None,
            "kept": int(r["decision"] == "keep") if scored and r else None,
            "answer_wrong": aw,
            "kept_answer_wrong_only": int(aw >= cfg.tailwise.keep_threshold) if aw is not None else None,
            "flags": "; ".join((r or {}).get("flags") or []),
            "severity": severity(answers, deciding) if scored else None,
            "cause": (r or {}).get("cause", ""),
            "tokens": tokens, "tokens_source": (r or {}).get("tokens_source", ""), "cost": cfg.cost.usd(tokens),
            "error": errors.get(run.trace_id, ""),
        }
        for q in questions:
            row[f"answer_{q}"] = answers.get(q)
        for k, v in run.meta.items():
            if isinstance(v, str | int | float):
                row[f"meta_{k}"] = v
        if scored and r and not r.get("cached") and r.get("severity") is not None \
                and abs(float(r["severity"]) - row["severity"]) > 1e-6:
            raise ValueError(f"{run.run_id}: severity {row['severity']} differs from Tailwise's {r['severity']}: "
                             "analyze.severity no longer matches internal/pipeline/policy.go")
        row["severity_jev"], row["kept_jev"] = row["severity"], row["kept"]
        if verdicts is not None:
            apply_rules(row, verdicts.get(run.trace_id, []), answers, deciding)
        out.append(row)
    return out


def _rule(rows_: list[dict[str, Any]], score: str, kept: str, probability: str, cfg: Config,
          baseline: str | None = None) -> dict[str, Any]:
    """score ranks runs for review; probability is the one meant as a
    probability of failure, for calibration. With baseline, the headline also
    compares score with that ranking on the same resampled runs."""
    y = [r["failed"] for r in rows_]

    def scores(key: str) -> list[float]:
        return [r[key] if r[key] is not None else 0.0 for r in rows_]

    s = scores(score)
    p = [r[probability] if r[probability] is not None else 0.0 for r in rows_]
    pred = [r[kept] or 0 for r in rows_]
    return {
        "review": metrics.review_precision(y, s, cfg.review_budget, cfg.reference_failure_rates,
                                           cfg.random_seeds, cfg.seed, groups=[r["task"] for r in rows_],
                                           baseline=scores(baseline) if baseline else None),
        "at_threshold": {"threshold": cfg.tailwise.keep_threshold,
                         **metrics.threshold_metrics(y, pred, cfg.reference_failure_rates)},
        **metrics.ranking_metrics(y, s),
        "budget_curve": metrics.budget_curve(y, s, cfg.budgets, cfg.random_seeds, cfg.seed),
        "calibration": metrics.calibration(y, p),
    }


def group_summary(rows_: list[dict[str, Any]], cfg: Config) -> dict[str, Any]:
    scored = [r for r in rows_ if r["p_keep"] is not None]
    y = [r["failed"] for r in scored]
    ruled = any("rule_keep" in r for r in rows_)
    # With rules, compared with Jev alone; without, with answer_wrong alone
    # when the two rank differently.
    if ruled:
        baseline: str | None = "severity_jev"
    else:
        baseline = "answer_wrong" if any(r["severity"] != r["answer_wrong"] for r in scored) else None
    default = _rule(rows_, "severity", "kept", "p_keep", cfg, baseline=baseline)
    tokens = sum(r["tokens"] for r in rows_)
    caught = default["at_threshold"]["tp"]
    return {
        "runs": len(rows_), "runs_scored": len(scored), "runs_failed_to_score": len(rows_) - len(scored),
        "failed": sum(r["failed"] for r in rows_),
        "failure_rate": sum(r["failed"] for r in rows_) / len(rows_) if rows_ else None,
        "default": default,
        "answer_wrong_only": _rule(rows_, "answer_wrong", "kept_answer_wrong_only", "answer_wrong", cfg),
        # The same decisions ranked by the highest deciding answer, as before
        # severity was the average: what the ranking adds.
        "by_p_keep": _rule(rows_, "p_keep", "kept_jev", "p_keep", cfg),
        **({"jev_only": _rule(rows_, "severity_jev", "kept_jev", "p_keep", cfg),
            "rules_only": _rule(rows_, "severity_rules", "rule_keep", "severity_rules", cfg),
            "rules": metrics.flag_stats([r["failed"] for r in rows_],
                                        [[f for f in r["rules_fired"].split("; ") if f] for r in rows_])}
           if ruled else {}),
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
        sc = [r for r in rows_ if r["dataset"] == ds and r["severity"] is not None]
        c = metrics.curves([r["failed"] for r in sc], [r["severity"] for r in sc]) if sc else None
        if c:
            out[ds] = c
    return out
