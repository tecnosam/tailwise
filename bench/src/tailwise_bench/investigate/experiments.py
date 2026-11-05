"""Tables for the opt-in experiments (narrow and completeness checks), from
their CSVs, against Tailwise's own answer_wrong on the same runs."""

from __future__ import annotations

import csv
from pathlib import Path
from typing import Any

from .. import metrics

FLAG_AT = 0.5


def _f(v: str) -> float | None:
    return float(v) if v not in ("", None) else None


def _rate(rows: list[dict[str, Any]], pred: Any) -> dict[str, Any]:
    n = len(rows)
    hit = sum(1 for r in rows if pred(r))
    return {"n": n, "flagged": hit, "rate": hit / n if n else None}


def narrow_tables(path: Path) -> dict[str, Any]:
    with path.open() as f:
        rows = [r for r in csv.DictReader(f) if not r["error"]]
    out: dict[str, Any] = {}
    for g in ("detectable_mixup", "grounded_wrong", "extra", "control"):
        rs = [r for r in rows if r["group"] == g]
        if not rs:
            continue
        aw_first = [r for r in rs if _f(r["answer_wrong_first"]) is not None]
        aw_grounded = [r for r in rs if _f(r["answer_wrong_grounded"]) is not None]
        out[g] = {
            "writes": len(rs), "runs": len({r["run_id"] for r in rs}),
            "argument_check": _rate(rs, lambda r: r["args_flag"] == "1"),
            "policy_check": _rate(rs, lambda r: r["policy_flag"] == "1"),
            "either_check": _rate(rs, lambda r: r["args_flag"] == "1" or r["policy_flag"] == "1"),
            # Tailwise's own decision on the run the write is in, for comparison.
            "answer_wrong_first": _rate(aw_first, lambda r: (_f(r["answer_wrong_first"]) or 0) >= FLAG_AT),
            "answer_wrong_grounded": _rate(aw_grounded, lambda r: (_f(r["answer_wrong_grounded"]) or 0) >= FLAG_AT),
        }
    # Each check's power to tell a wrong write from a correct one (control).
    for check, col in (("argument_check", "p_action_wrong_for_request"), ("policy_check", "p_action_violates_policy")):
        for g in ("detectable_mixup", "grounded_wrong", "extra"):
            rs = [r for r in rows if r["group"] in (g, "control") and _f(r[col]) is not None]
            if g in out and rs:
                out[g][f"{check}_roc_auc_vs_control"] = metrics.ranking_metrics(
                    [int(r["group"] == g) for r in rs], [_f(r[col]) or 0.0 for r in rs])["roc_auc"]
    return out


def completeness_tables(path: Path) -> dict[str, Any]:
    with path.open() as f:
        rows = [r for r in csv.DictReader(f) if not r["error"] and r["score"] != ""]
    out: dict[str, Any] = {}
    subsets = {
        "missing_action (all tasks)": [r for r in rows if r["group"] == "missing_action"],
        "missing_action (gold-consistent tasks)": [r for r in rows if r["group"] == "missing_action"
                                                   and r["gold_consistent_task"] == "1"],
        "control (successful runs)": [r for r in rows if r["group"] == "control"],
    }
    for name, rs in subsets.items():
        if not rs:
            continue
        out[name] = {
            "runs": len(rs),
            "completeness_check": _rate(rs, lambda r: r["flag"] == "1"),
            "flagged_for_the_missing_request": (_rate(rs, lambda r: r["right_reason"] == "1")
                                                if name.startswith("missing") else None),
            "answer_wrong_first": _rate([r for r in rs if _f(r["answer_wrong_first"]) is not None],
                                        lambda r: (_f(r["answer_wrong_first"]) or 0) >= FLAG_AT),
            "answer_wrong_grounded": _rate([r for r in rs if _f(r["answer_wrong_grounded"]) is not None],
                                           lambda r: (_f(r["answer_wrong_grounded"]) or 0) >= FLAG_AT),
            "either_completeness_or_answer_wrong_grounded": _rate(
                rs, lambda r: r["flag"] == "1" or (_f(r["answer_wrong_grounded"]) or 0) >= FLAG_AT),
        }
    pos = subsets["missing_action (all tasks)"]
    neg = subsets["control (successful runs)"]
    if pos and neg:
        y = [1] * len(pos) + [0] * len(neg)
        out["roc_auc_missing_vs_control"] = {
            "completeness_score": metrics.ranking_metrics(y, [_f(r["score"]) or 0.0 for r in pos + neg])["roc_auc"],
            "answer_wrong_first": metrics.ranking_metrics(
                y, [_f(r["answer_wrong_first"]) or 0.0 for r in pos + neg])["roc_auc"],
            "answer_wrong_grounded": metrics.ranking_metrics(
                y, [_f(r["answer_wrong_grounded"]) or 0.0 for r in pos + neg])["roc_auc"],
        }
    return out
