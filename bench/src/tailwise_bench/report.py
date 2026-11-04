"""Writes the run folder: runs.csv, summary.json, plots and report.md."""

from __future__ import annotations

import csv
import json
import math
from collections.abc import Callable
from pathlib import Path
from typing import Any

from . import plots
from .plots import name


def write_csv(path: Path, rows: list[dict[str, Any]]) -> None:
    if not rows:
        return
    fields = list(rows[0])
    for r in rows[1:]:
        fields += [k for k in r if k not in fields]
    with path.open("w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=fields)
        w.writeheader()
        w.writerows(rows)


def _clean(v: Any) -> Any:
    """JSON without NaN."""
    if isinstance(v, float) and math.isnan(v):
        return None
    if isinstance(v, dict):
        return {k: _clean(x) for k, x in v.items()}
    if isinstance(v, list | tuple):
        return [_clean(x) for x in v]
    return v


def write_json(path: Path, data: Any) -> None:
    path.write_text(json.dumps(_clean(data), indent=2) + "\n")


def make_plots(summary: dict[str, Any], curves: dict[str, Any], out: Path) -> list[Path]:
    out.mkdir(parents=True, exist_ok=True)
    ds = summary["datasets"]
    made = [plots.budget(ds, out), plots.kept_precision(ds, out), *plots.roc_pr(curves, ds, out),
            plots.reliability(ds, out), plots.flags(ds, out), plots.cost(ds, out)]
    return [p for p in made if p is not None]


def _pct(v: float | None) -> str:
    return "n/a" if v is None else f"{v:.0%}"


def _num(v: float | None, digits: int = 2) -> str:
    return "n/a" if v is None else f"{v:.{digits}f}"


def _at(curve: list[dict[str, Any]], budget: float) -> dict[str, Any]:
    return next((p for p in curve if abs(p["budget"] - budget) < 1e-9), {})


def dataset_sentence(ds: str, s: dict[str, Any]) -> str:
    t = s["default"]["at_threshold"]
    p = _at(s["default"]["budget_curve"], 0.10)
    lo, hi = p.get("random_precision_ci95") or [None, None]
    return (
        f"On {name(ds)} ({s['runs']} runs, {_pct(s['failure_rate'])} failed), Tailwise kept {_pct(t['kept_share'])} "
        f"of runs; {_pct(t['precision'])} of those had failed, against {_pct(t['base_rate'])} for runs kept at random "
        f"({_num(t['lift'])}× lift), and it caught {_pct(t['recall'])} of all failures while keeping "
        f"{_pct(t['false_positive_rate'])} of successful runs. Reviewing only the 10% of runs it scores highest, "
        f"{_pct(p.get('tailwise_precision'))} are failures (random: {_pct(p.get('random_precision_mean'))}, "
        f"95% interval {_pct(lo)}–{_pct(hi)}). ROC-AUC is {_num(s['default']['roc_auc'], 3)}."
    )


def detectable_sentence(ds: str, s: dict[str, Any], whole: dict[str, Any]) -> str:
    return (f"On the {s['runs']} {name(ds)} runs whose label the trace can show ({_pct(s['failure_rate'])} failed), "
            f"ROC-AUC is {_num(s['default']['roc_auc'], 3)} (all runs: {_num(whole['default']['roc_auc'], 3)}).")


def summary_paragraph(summary: dict[str, Any], meta: dict[str, Any]) -> str:
    parts = []
    for ds, s in summary["datasets"].items():
        if s["runs"]:
            parts.append(dataset_sentence(ds, s))
        if ds in summary.get("detectable", {}):
            parts.append(detectable_sentence(ds, summary["detectable"][ds], s))
    parts.append(f"The scores took {meta['total_tokens']:,} input tokens (${meta['total_cost_usd']:.4f}), "
                 f"{meta['token_source']}; this run spent ${meta['spent_this_run_usd']:.4f}, the rest from the cache.")
    if meta.get("limit"):
        parts.append(f"This is a smoke run on at most {meta['limit']} runs per dataset: the numbers are not meaningful.")
    return " ".join(parts)


def _rule_row(label: str, s: dict[str, Any], rule: str) -> str:
    r = s[rule]
    t = r["at_threshold"]
    p = _at(r["budget_curve"], 0.10)
    return (f"| {label} | {s['runs']} | {s['runs_failed_to_score']} | {_pct(s['failure_rate'])} | {_pct(t['kept_share'])} | {_pct(t['precision'])} | "
            f"{_num(t['lift'])}× | {_pct(t['recall'])} | {_pct(t['false_positive_rate'])} | "
            f"{_pct(p.get('tailwise_precision'))} vs {_pct(p.get('random_precision_mean'))} | {_num(r['roc_auc'], 3)} | "
            f"{_num(r['calibration']['ece'], 3)} |")


HEADER = ["| Runs | Runs | Not scorable | Failed | Kept | Kept that failed | Lift | Failures caught | Successes kept "
          "| Top 10%: failed vs random | ROC-AUC | ECE |",
          "| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |"]


COMPARED: list[tuple[str, Callable[[dict[str, Any]], float | None], str]] = [
    ("Kept", lambda r: r["at_threshold"]["kept_share"], "pct"),
    ("Kept that failed (precision)", lambda r: r["at_threshold"]["precision"], "pct"),
    ("Lift", lambda r: r["at_threshold"]["lift"], "x"),
    ("Failures caught (recall)", lambda r: r["at_threshold"]["recall"], "pct"),
    ("Successes kept", lambda r: r["at_threshold"]["false_positive_rate"], "pct"),
    ("Top 10%: share that failed", lambda r: _at(r["budget_curve"], 0.10).get("tailwise_precision"), "pct"),
    ("Top 10%: failures found", lambda r: _at(r["budget_curve"], 0.10).get("tailwise_recall"), "pct"),
    ("ROC-AUC", lambda r: r["roc_auc"], "num"),
    ("PR-AUC", lambda r: r["pr_auc"], "num"),
    ("ECE (lower is better)", lambda r: r["calibration"]["ece"], "num"),
]


def _fmt(v: float | None, kind: str) -> str:
    if v is None:
        return "n/a"
    return {"pct": f"{v:.0%}", "x": f"{v:.2f}×", "num": f"{v:.3f}"}[kind]


def _delta(before: float | None, after: float | None, kind: str) -> str:
    if before is None or after is None:
        return ""
    d = after - before
    return f"{d * 100:+.0f} pts" if kind == "pct" else f"{d:+.3f}" if kind == "num" else f"{d:+.2f}×"


def comparison(summary: dict[str, Any], baseline: dict[str, Any]) -> list[str]:
    """Each dataset's default rule against the same dataset in a baseline run."""
    b_meta = baseline.get("meta", {})
    lines = [f"## Compared with {b_meta.get('run_id', 'the baseline')}", "",
             f"The baseline run is {b_meta.get('run_id', '?')} (Tailwise {b_meta.get('tailwise_version', '?')}). "
             "Both score the same runs; what changed between them is in the run notes below.", ""]
    groups = [(ds, name(ds), s, baseline.get("datasets", {}).get(ds)) for ds, s in summary["datasets"].items()]
    groups += [(ds, f"{name(ds)}, trace-detectable runs", s, baseline.get("detectable", {}).get(ds))
               for ds, s in summary.get("detectable", {}).items()]
    for ds, title, s, b in groups:
        if not b:
            continue
        lines += [f"### {title}", "", "| Metric | Before | After | Change |", "| --- | --- | --- | --- |"]
        for label, get, kind in COMPARED:
            before, after = get(b["default"]), get(s["default"])
            lines.append(f"| {label} | {_fmt(before, kind)} | {_fmt(after, kind)} | {_delta(before, after, kind)} |")
        lines += ["", "| Check | Fires on failed runs, before → after | Fires on successful runs, before → after |",
                  "| --- | --- | --- |"]
        for q in sorted(set(s["flags"]) | set(b["flags"])):
            fb, fa = b["flags"].get(q, {}), s["flags"].get(q, {})
            lines.append(f"| {q} | {_pct(fb.get('recall'))} → {_pct(fa.get('recall'))} | "
                         f"{_pct(fb.get('false_positive_rate'))} → {_pct(fa.get('false_positive_rate'))} |")
        lines.append("")
    subs = [k for k in summary["subsets"] if k in baseline.get("subsets", {})]
    if subs:
        lines += ["### ROC-AUC by subset", "", "| Subset | Before | After | Change |", "| --- | --- | --- | --- |"]
        for k in subs:
            before, after = baseline["subsets"][k]["default"]["roc_auc"], summary["subsets"][k]["default"]["roc_auc"]
            lines.append(f"| {k} | {_fmt(before, 'num')} | {_fmt(after, 'num')} | {_delta(before, after, 'num')} |")
        lines.append("")
    return lines


def write_report(out: Path, summary: dict[str, Any], meta: dict[str, Any], plot_paths: list[Path],
                 baseline: dict[str, Any] | None = None, notes: str = "") -> None:
    lines = [f"# Tailwise benchmark: {meta['run_id']}", "",
             "Tailwise's job is to pick, from many agent runs, the ones worth a human's review: the failures. "
             "Each run here is scored once, as production would score it, and labelled by whether it actually "
             "failed. Random keeping is the baseline: it finds failures at the dataset's failure rate.", "",
             summary_paragraph(summary, meta), ""]
    lines += ["## Run", "",
              f"- Tailwise {meta['tailwise_version']} (commit {meta['git_sha']}), Jev model {', '.join(meta['jev_models']) or 'n/a'}",
              f"- Questions: {meta['variant']}; threshold {meta['config']['tailwise']['keep_threshold']}, no random floor",
              f"- {meta['scored']} runs scored, {meta['cached']} from the cache, {meta['failed']} failed to score", ""]
    if meta.get("aborted"):
        lines += [f"**Stopped early:** {meta['aborted']}", ""]
    lines += ["## At Tailwise's threshold", "", HEADER[0].replace("| Runs |", "| Dataset |", 1), HEADER[1]]
    for ds, s in summary["datasets"].items():
        lines.append(_rule_row(f"{name(ds)}, Tailwise", s, "default"))
        lines.append(_rule_row(f"{name(ds)}, answer_wrong only", s, "answer_wrong_only"))
        if ds in summary.get("detectable", {}):
            lines.append(_rule_row(f"{name(ds)}, trace-detectable runs", summary["detectable"][ds], "default"))
    lines += ["", "\"Not scorable\" runs are ones Jev rejected (for example as too long); they count as dropped, as in "
              "production. \"Kept that failed\" is precision; random keeping gets the failure rate, so lift is precision ÷ failure "
              "rate. \"Failures caught\" is recall.", ""]
    if summary.get("detectable"):
        lines += ["\"Trace-detectable runs\" leave out runs whose label no trace-only reviewer could judge: runs on "
                  "tasks whose logged gold actions disagree with a successful run, and failed runs whose only mistake is "
                  "choosing a variant the user never described. Judge changes to Tailwise on this row; the full "
                  "dataset caps what any trace-based detector can reach.", ""]
    rates = meta["config"]["reference_failure_rates"]
    lines += ["### At production failure rates", "",
              "The datasets fail far more often than most production agents. With the same catch rate and "
              "false-positive rate, this is the share of kept runs that would be failures:", "",
              "| Dataset | " + " | ".join(f"{r:.0%} fail" for r in rates) + " |",
              "| --- | " + " | ".join("---" for _ in rates) + " |"]
    for ds, s in summary["datasets"].items():
        pa = s["default"]["at_threshold"]["precision_at_failure_rate"]
        lines.append(f"| {name(ds)} | " + " | ".join(_pct(pa.get(str(r))) for r in rates) + " |")
    lines += ["", "## By subset", "", HEADER[0].replace("| Runs |", "| Subset |", 1), HEADER[1]]
    for sub, s in summary["subsets"].items():
        lines.append(_rule_row(sub, s, "default"))
    lines.append("")
    if baseline:
        lines += comparison(summary, baseline)
    if notes:
        lines += ["## Run notes", "", notes, ""]
    if plot_paths:
        lines += ["## Plots", ""]
        for path in plot_paths:
            title = path.stem.replace("_", " ").capitalize()
            lines += [f"### {title}", "", f"![{title}](plots/{path.name})", ""]
    (out / "report.md").write_text("\n".join(lines))
