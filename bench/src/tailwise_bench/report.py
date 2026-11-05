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


# Fewer reviewed runs per draw than this, and a review cell is marked as noisy.
FEW_REVIEWED = 20


def _review_at(r: dict[str, Any], rate: float) -> dict[str, Any]:
    at: dict[str, Any] = (r.get("review") or {}).get("at_rate", {}).get(str(rate), {})
    return at


def _review_cell(r: dict[str, Any], rate: float | None) -> str:
    if rate is None:
        return _pct((r.get("review") or {}).get("real", {}).get("precision"))
    a = _review_at(r, rate)
    if not a:
        return "n/a"
    return _pct(a["precision"]) + ("†" if a["reviewed"] < FEW_REVIEWED else "")


def review_section(summary: dict[str, Any], meta: dict[str, Any]) -> list[str]:
    """The headline table: of the runs a team reviews, ranked by Tailwise,
    the share that failed."""
    cfg = meta["config"]
    budget, rates = cfg.get("review_budget", 0.05), sorted(cfg["reference_failure_rates"], reverse=True)
    lines = [f"## Review budget: the share of reviewed runs that failed", "",
             f"A team reviews the {budget:.0%} of runs Tailwise ranks highest (by severity). The table shows what share "
             "of those runs failed. Random review finds failures at the failure rate; the best any ranking can do is "
             f"every reviewed run a failure, or failure rate ÷ {budget:.0%} when failures are fewer than that.", "",
             "The datasets fail far more often than production agents, so each column after the first drops failed "
             f"runs at random until that share fail ({meta['config']['random_seeds']} draws, mean). Each draw also "
             "resamples the tasks, since a dataset runs each task several times, so the intervals below cover which "
             "tasks the dataset happened to contain.", "",
             "| Ranking | Own rate | " + " | ".join(f"{r:.0%} failing" for r in rates) + " |",
             "| --- | --- | " + " | ".join("---" for _ in rates) + " |",
             "| Random review | failure rate | " + " | ".join(_pct(r) for r in rates) + " |",
             "| Best possible | 100% | " + " | ".join(_pct(min(1.0, r / budget)) for r in rates) + " |"]
    groups = []
    for ds, s in summary["datasets"].items():
        whole = [(f"{name(ds)}", s)]
        if ds in summary.get("detectable", {}):
            whole.append((f"{name(ds)}, trace-detectable", summary["detectable"][ds]))
        for label, g in whole:
            groups.append((f"{label}, Tailwise" + (" (Jev + rules)" if "jev_only" in g else ""), g, "default"))
            if "jev_only" in g:
                groups.append((f"{label}, Jev alone", g, "jev_only"))
                groups.append((f"{label}, rules alone", g, "rules_only"))
            groups.append((f"{label}, ranked by p_keep (highest answer)", g, "by_p_keep"))
            groups.append((f"{label}, answer_wrong only", g, "answer_wrong_only"))
    for label, s, rule in groups:
        r = s[rule]
        # A comparison row that ranks exactly as Tailwise does (one deciding
        # question) adds nothing.
        if rule != "default" and r.get("review") == s["default"].get("review"):
            continue
        lines.append(f"| {label} ({_pct(s['failure_rate'])} failed) | {_review_cell(r, None)} | "
                     + " | ".join(_review_cell(r, rate) for rate in rates) + " |")
    lines += ["", f"† Fewer than {FEW_REVIEWED} runs reviewed per draw: a rough estimate.", ""]
    lines += gain_table([(lbl, s_) for lbl, s_, rule in groups if rule == "default"], rates)
    lines += rules_table(summary)
    return lines


# The deployment the cost table describes.
COST_TRACES = 10_000
COST_FAILURE_RATE = 0.1


def cost_section(summary: dict[str, Any], meta: dict[str, Any]) -> list[str]:
    """What upgrading from random review to Tailwise costs and buys: per
    10,000 traces at a 10% failure rate, with the headline review budget."""
    budget = meta["config"].get("review_budget", 0.05)
    reviews = round(COST_TRACES * budget)
    random_found = reviews * COST_FAILURE_RATE
    usd_per_m = meta["config"]["cost"]["usd_per_million_input_tokens"]
    rows = []
    groups = []
    for ds, s in summary["datasets"].items():
        groups.append((name(ds), s))
        if ds in summary.get("detectable", {}):
            groups.append((f"{name(ds)}, trace-detectable", summary["detectable"][ds]))
    for label, s in groups:
        at = _review_at(s["default"], COST_FAILURE_RATE)
        per_run = s.get("cost_per_run_usd")
        if not at or per_run is None:
            continue
        found = reviews * at["precision"]
        jev = per_run * COST_TRACES
        extra = found - random_found
        rough = "†" if at["reviewed"] < FEW_REVIEWED else ""
        rows.append(f"| {label} | ${jev:.2f} | {random_found:.0f} → {found:.0f}{rough} | "
                    f"{reviews / random_found:.0f} → {reviews / found:.1f}{rough} | "
                    f"{found / COST_FAILURE_RATE:,.0f} | "
                    + (f"${jev / extra:.3f}" if extra > 0 else "no gain") + " |")
    if not rows:
        return []
    return ["## Cost: random review against Tailwise", "",
            f"Per {COST_TRACES:,} traces with {COST_FAILURE_RATE:.0%} failing, a team reviewing {reviews} of them "
            f"({budget:.0%}). Random review costs nothing to choose the traces and finds failures at the failure rate; "
            f"Tailwise pays Jev to score every trace (${usd_per_m} per million input tokens; output is free). "
            "\"Random reviews for the same failures\" is how many traces random review would need to read to find as many "
            "failures as Tailwise's queue does.", "",
            "| Dataset | Jev cost | Failures found: random → Tailwise | Reviews per failure found | "
            "Random reviews for the same failures | Jev cost per extra failure found |",
            "| --- | --- | --- | --- | --- | --- |", *rows, "",
            "Reviewer time is the larger cost: multiply \"reviews per failure found\" by what a review costs your "
            "team. † Rough: few successful runs in the dataset.", ""]


def rules_table(summary: dict[str, Any]) -> list[str]:
    """What each heuristic rule fired on."""
    rows = []
    for ds, s in summary["datasets"].items():
        for rule, f in (s.get("rules") or {}).items():
            rows.append(f"| {name(ds)} | `{rule}` | {_pct(f['recall'])} | {_pct(f['false_positive_rate'])} | "
                        f"{_pct(f['precision'])} |")
    if not rows:
        return []
    return ["### The rules", "",
            "How often each rule fired on failed and on successful runs, and the share of runs it fired on that "
            "failed. A keep rule's runs are kept without asking Jev.", "",
            "| Dataset | Rule | Fired on failed | Fired on successful | Fired runs that failed |",
            "| --- | --- | --- | --- | --- |", *rows, ""]


def _points(v: float) -> str:
    return f"{round(v * 100) + 0:+d}"  # + 0 turns -0 into 0


def gain_table(groups: list[tuple[str, dict[str, Any]]], rates: list[float]) -> list[str]:
    """Tailwise's ranking against answer_wrong alone, on the same resampled
    runs: the points it adds, with the 95% interval."""
    rows = []
    for label, s in groups:
        cells = [(_review_at(s["default"], r).get("vs_baseline")) for r in rates]
        if not any(cells):
            continue
        rows.append(f"| {label} | " + " | ".join(
            "n/a" if not c else f"{_points(c['gain'])} ({_points(c['ci95'][0])} to {_points(c['ci95'][1])})"
            for c in cells) + " |")
    if not rows:
        return []
    against = "Jev alone" if any("jev_only" in s for _, s in groups) else "`answer_wrong` alone"
    return [f"### Against {against.replace('`', '')}", "",
            f"Points of reviewed runs that failed, Tailwise's ranking minus {against} on the same draws, "
            "with the 95% interval. An interval that includes 0 means the gain is not shown.", "",
            "| Ranking | " + " | ".join(f"{r:.0%} failing" for r in rates) + " |",
            "| --- | " + " | ".join("---" for _ in rates) + " |", *rows, ""]


def dataset_sentence(ds: str, s: dict[str, Any]) -> str:
    t = s["default"]["at_threshold"]
    rev = s["default"].get("review") or {}
    budget = rev.get("budget", 0.05)
    at10 = _review_at(s["default"], 0.1)
    headline = (f"Reviewing the {budget:.0%} of {name(ds)} runs Tailwise ranks highest, "
                f"{_pct(rev.get('real', {}).get('precision'))} failed (failure rate {_pct(s['failure_rate'])})"
                + (f"; were 10% of runs failing, {_pct(at10.get('precision'))} would." if at10 else ".") + " ")
    p = _at(s["default"]["budget_curve"], 0.10)
    lo, hi = p.get("random_precision_ci95") or [None, None]
    return headline + (
        f"On {name(ds)} ({s['runs']} runs, {_pct(s['failure_rate'])} failed), Tailwise kept {_pct(t['kept_share'])} "
        f"of runs; {_pct(t['precision'])} of those had failed, against {_pct(t['base_rate'])} for runs kept at random "
        f"({_num(t['lift'])}× lift), and it caught {_pct(t['recall'])} of all failures while keeping "
        f"{_pct(t['false_positive_rate'])} of successful runs. Reviewing only the 10% of runs it scores highest, "
        f"{_pct(p.get('tailwise_precision'))} are failures (random: {_pct(p.get('random_precision_mean'))}, "
        f"95% interval {_pct(lo)}–{_pct(hi)}). ROC-AUC is {_num(s['default']['roc_auc'], 3)}."
    )


def detectable_sentence(ds: str, s: dict[str, Any], whole: dict[str, Any]) -> str:
    at10 = _review_at(s["default"], 0.1)
    lo, hi = at10.get("ci95") or [None, None]
    return (f"On the {s['runs']} {name(ds)} runs whose label the trace can show ({_pct(s['failure_rate'])} failed), "
            f"{_pct(at10.get('precision'))} of reviewed runs failed at a 10% failure rate "
            f"(95% interval {_pct(lo)}–{_pct(hi)}), and "
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
    if "tokens_with_rules" in meta:
        parts.append(f"With the rules, keep rules decide some runs without Jev: "
                     f"{meta['tokens_with_rules']:,} tokens ({meta['tokens_with_rules'] / max(1, meta['total_tokens']):.0%}).")
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
    *[(f"Reviewed runs that failed, {r:.0%} failing", (lambda r: lambda x: _review_at(x, r).get("precision"))(r), "pct")
      for r in (0.2, 0.1, 0.05)],
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
    lines += review_section(summary, meta)
    lines += cost_section(summary, meta)
    lines += ["## Run", "",
              f"- Tailwise {meta['tailwise_version']} (commit {meta['git_sha']}), Jev model {', '.join(meta['jev_models']) or 'n/a'}",
              f"- Questions: {meta['variant']}; threshold {meta['config']['tailwise']['keep_threshold']}, no random floor; "
              "runs ranked by severity, the deciding questions' average answer",
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
