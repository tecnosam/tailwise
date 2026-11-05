"""The metrics, as pure functions over labels (1 = the run failed) and scores.

Every random choice takes an explicit seed. Ties in score are broken by a
seeded shuffle, so reviewing "by score" among equal scores is random, not by
position.
"""

from __future__ import annotations

import math
from collections.abc import Sequence
from typing import Any

import numpy as np
from sklearn.metrics import average_precision_score, precision_recall_curve, roc_auc_score, roc_curve

Array = np.ndarray


def _arrays(y: Sequence[int], s: Sequence[float]) -> tuple[Array, Array]:
    ya, sa = np.asarray(y, dtype=int), np.asarray(s, dtype=float)
    if ya.shape != sa.shape:
        raise ValueError("labels and scores differ in length")
    return ya, sa


def precision_at_rate(tpr: float | None, fpr: float | None, rate: float) -> float | None:
    """The precision the same decisions would have if `rate` of runs failed:
    TPR and FPR do not depend on the failure rate, precision does."""
    if tpr is None or fpr is None:
        return None
    kept = tpr * rate + fpr * (1 - rate)
    return tpr * rate / kept if kept else None


def threshold_metrics(y: Sequence[int], pred: Sequence[int], rates: Sequence[float] = ()) -> dict[str, Any]:
    """The keep decisions against the labels. `rates` adds the precision the
    decisions would have at those production failure rates."""
    ya, pa = np.asarray(y, dtype=int), np.asarray(pred, dtype=int)
    tp = int(((ya == 1) & (pa == 1)).sum())
    fp = int(((ya == 0) & (pa == 1)).sum())
    fn = int(((ya == 1) & (pa == 0)).sum())
    tn = int(((ya == 0) & (pa == 0)).sum())
    n = len(ya)
    precision = tp / (tp + fp) if tp + fp else None
    recall = tp / (tp + fn) if tp + fn else None
    fpr = fp / (fp + tn) if fp + tn else None
    f1 = (2 * precision * recall / (precision + recall)
          if precision is not None and recall is not None and precision + recall else None)
    base = (tp + fn) / n if n else None
    return {
        "kept_share": (tp + fp) / n if n else None,
        "precision": precision, "base_rate": base,
        "lift": precision / base if precision is not None and base else None,
        "recall": recall, "false_positive_rate": fpr, "f1": f1,
        "tp": tp, "fp": fp, "fn": fn, "tn": tn,
        "precision_at_failure_rate": {str(r): precision_at_rate(recall, fpr, r) for r in rates},
    }


def ranking_metrics(y: Sequence[int], s: Sequence[float]) -> dict[str, Any]:
    """ROC-AUC and PR-AUC (average precision); None when one class is missing."""
    ya, sa = _arrays(y, s)
    base = float(ya.mean()) if len(ya) else None
    if len(set(ya.tolist())) < 2:
        return {"roc_auc": None, "pr_auc": None, "pr_auc_random": base}
    return {"roc_auc": float(roc_auc_score(ya, sa)), "pr_auc": float(average_precision_score(ya, sa)),
            "pr_auc_random": base}


def curves(y: Sequence[int], s: Sequence[float]) -> dict[str, Any] | None:
    ya, sa = _arrays(y, s)
    if len(set(ya.tolist())) < 2:
        return None
    fpr, tpr, _ = roc_curve(ya, sa)
    precision, recall, _ = precision_recall_curve(ya, sa)
    return {"fpr": fpr.tolist(), "tpr": tpr.tolist(), "precision": precision.tolist(), "recall": recall.tolist()}


def rank_order(s: Array, seed: int) -> Array:
    """Indices from highest score to lowest, ties in a seeded random order."""
    tiebreak = np.random.default_rng(seed).permutation(len(s))
    return np.lexsort((tiebreak, -s))


def budget_k(budget: float, n: int) -> int:
    return min(n, max(1, math.ceil(budget * n))) if n else 0


def budget_curve(y: Sequence[int], s: Sequence[float], budgets: Sequence[float], seeds: int, seed: int) -> list[dict[str, Any]]:
    """Reviewing the top budget share of runs by score, against reviewing the
    same number picked at random (seeds runs: mean and 95% percentile
    interval): the share of failures found (recall) and the share of reviewed
    runs that failed (precision)."""
    ya, sa = _arrays(y, s)
    total = int(ya.sum())
    order = rank_order(sa, seed)
    rng = np.random.default_rng(seed + 1)
    perms = [rng.permutation(len(ya)) for _ in range(seeds)]
    out = []
    for b in budgets:
        k = budget_k(b, len(ya))
        found = int(ya[order[:k]].sum())
        rand = np.array([ya[p[:k]].sum() for p in perms], dtype=float)

        def ci(v: Array) -> list[float]:
            return [float(np.percentile(v, 2.5)), float(np.percentile(v, 97.5))]

        out.append({
            "budget": b, "k": k, "n": len(ya), "failures": total,
            "tailwise_recall": found / total if total else None,
            "random_recall_mean": float(rand.mean()) / total if total else None,
            "random_recall_ci95": ci(rand / total) if total else None,
            "tailwise_precision": found / k if k else None,
            "random_precision_mean": float(rand.mean()) / k if k else None,
            "random_precision_ci95": ci(rand / k) if k else None,
            "lift": found / float(rand.mean()) if rand.mean() > 0 else None,
            # The most a perfect ranker could find at this budget.
            "best_possible_recall": min(k, total) / total if total else None,
        })
    return out


def review_precision(y: Sequence[int], s: Sequence[float], budget: float, rates: Sequence[float],
                     seeds: int, seed: int, groups: Sequence[str] | None = None,
                     baseline: Sequence[float] | None = None) -> dict[str, Any]:
    """The headline: a team reviews the top `budget` share of runs by score;
    what share of those failed? At the dataset's own failure rate, and at each
    production failure rate in `rates`. The datasets fail far more often than
    production agents, so for each rate, failed runs (or, above the dataset's
    rate, successful ones) are dropped at random until `rate` of runs fail.

    Each of the `seeds` draws first resamples the tasks in `groups` with
    replacement (each run its own task without them): runs of one task are
    not independent, so the 95% interval covers which tasks the dataset
    happened to contain, not only which runs were dropped. With `baseline`
    scores, each draw also ranks the same runs by them, and "vs_baseline" is
    the gain in precision: its mean, 95% interval, and the share of draws in
    which the baseline did at least as well.

    Random review finds failures at the failure rate; a perfect ranker finds
    min(1, rate / budget)."""
    ya, sa = _arrays(y, s)
    ba = np.asarray(baseline, dtype=float) if baseline is not None else None
    if ba is not None and ba.shape != sa.shape:
        raise ValueError("baseline and scores differ in length")
    g = np.asarray(groups if groups is not None else range(len(ya)))
    if g.shape != ya.shape:
        raise ValueError("groups and labels differ in length")

    def top(idx: Array, scores: Array, tie_seed: int) -> float:
        k = budget_k(budget, len(idx))
        return float(ya[idx[rank_order(scores[idx], tie_seed)[:k]]].mean()) if k else float("nan")

    def ci(v: Array) -> list[float]:
        return [float(np.percentile(v, 2.5)), float(np.percentile(v, 97.5))]

    real = float(ya.mean()) if len(ya) else None
    out: dict[str, Any] = {
        "budget": budget,
        "real": {"failure_rate": real, "precision": top(np.arange(len(ya)), sa, seed) if len(ya) else None,
                 "best_possible": min(1.0, real / budget) if real is not None else None},
        "at_rate": {},
    }
    if not (ya == 1).any() or not (ya == 0).any():
        return out
    tasks, members = np.unique(g, return_inverse=True)
    by_task = [np.flatnonzero(members == t) for t in range(len(tasks))]
    rng = np.random.default_rng(seed + 2)
    for rate in rates:
        vals, gains = [], []
        for i in range(seeds):
            pool = np.concatenate([by_task[t] for t in rng.integers(len(tasks), size=len(tasks))])
            fail, ok = pool[ya[pool] == 1], pool[ya[pool] == 0]
            if not len(fail) or not len(ok):
                continue
            n_fail = round(rate / (1 - rate) * len(ok))
            if n_fail <= len(fail):
                idx = np.concatenate([rng.choice(fail, n_fail, replace=False), ok])
            else:
                n_ok = min(len(ok), round(len(fail) * (1 - rate) / rate))
                idx = np.concatenate([fail, rng.choice(ok, n_ok, replace=False)])
            vals.append(top(idx, sa, seed + 3 + i))
            if ba is not None:
                gains.append(vals[-1] - top(idx, ba, seed + 3 + i))
        v = np.asarray(vals)
        n_fail, n_ok = int((ya == 1).sum()), int((ya == 0).sum())
        row: dict[str, Any] = {
            "precision": float(v.mean()), "ci95": ci(v),
            "lift": float(v.mean()) / rate, "best_possible": min(1.0, rate / budget),
            # Reviewed runs per draw: few means a noisy estimate.
            "reviewed": budget_k(budget, min(len(ya), round(n_ok / (1 - rate)), round(n_fail / rate))),
        }
        if gains:
            d = np.asarray(gains)
            row["vs_baseline"] = {"gain": float(d.mean()), "ci95": ci(d), "not_better": float((d <= 0).mean())}
        out["at_rate"][str(rate)] = row
    return out


def calibration(y: Sequence[int], s: Sequence[float], bins: int = 10) -> dict[str, Any]:
    """Reliability-diagram data and expected calibration error, equal-width bins."""
    ya, sa = _arrays(y, s)
    edges = np.linspace(0, 1, bins + 1)
    index = np.clip(np.digitize(sa, edges[1:-1], right=False), 0, bins - 1)
    rows, ece = [], 0.0
    for b in range(bins):
        mask = index == b
        n = int(mask.sum())
        mean_pred = float(sa[mask].mean()) if n else None
        frac_pos = float(ya[mask].mean()) if n else None
        if mean_pred is not None and frac_pos is not None:
            ece += n / len(ya) * abs(mean_pred - frac_pos)
        rows.append({"lo": float(edges[b]), "hi": float(edges[b + 1]), "count": n,
                     "mean_predicted": mean_pred, "fraction_failed": frac_pos})
    return {"bins": rows, "ece": ece if len(ya) else None}


def flag_stats(y: Sequence[int], flags: Sequence[Sequence[str]]) -> dict[str, dict[str, Any]]:
    """For each question that flagged at least one run: how often it fires on
    failed and on successful runs, and how often a run it flags failed."""
    ya = list(y)
    failed, ok = sum(ya), len(ya) - sum(ya)
    out: dict[str, dict[str, Any]] = {}
    for label, fs in zip(ya, flags, strict=True):
        for f in set(fs):
            row = out.setdefault(f, {"flagged": 0, "flagged_failed": 0})
            row["flagged"] += 1
            row["flagged_failed"] += label
    for row in out.values():
        row["precision"] = row["flagged_failed"] / row["flagged"]
        row["recall"] = row["flagged_failed"] / failed if failed else None
        row["false_positive_rate"] = (row["flagged"] - row["flagged_failed"]) / ok if ok else None
    return dict(sorted(out.items(), key=lambda kv: -kv[1]["flagged"]))
