"""Runs the offline investigation (steps 1 to 6) and writes its folder.

Inputs: the τ-bench and SWE-agent runs the benchmark scores, the scores of an
earlier benchmark run (Jev's answers for every run, system prompt off), and
optionally a run with the system prompt on (for its token counts).
"""

from __future__ import annotations

import csv
import json
import math
import random
import statistics as st
from collections import Counter
from collections.abc import Callable, Iterable
from pathlib import Path
from typing import Any

from ..config import Config
from ..loaders import swe, tau
from ..otel import hex_id
from ..report import write_csv, write_json
from ..tokens import Estimator
from ..types import Run
from . import audit
from .capture import capture

FLAG_AT = 0.5
QUESTIONS = ["answer_wrong", "unsupported_answer", "missing_tool_calls", "answered_on_failure",
             "ignored_instructions", "contradicts_knowledge"]


def read_runs_csv(path: Path) -> dict[str, dict[str, str]]:
    with path.open() as f:
        return {r["run_id"]: r for r in csv.DictReader(f)}


def fnum(v: str | None) -> float | None:
    return float(v) if v not in (None, "") else None


def load_tau(cfg: Config) -> list[tuple[Run, dict[str, Any]]]:
    d = cfg.datasets.tau
    out = []
    for file in d["files"]:
        rows = json.loads(tau.download(d["commit"], file, cfg.resolve(cfg.cache_dir)).read_text())
        out += [(tau.parse(file, row), row) for row in rows]
    return out


def p_value(a_hit: int, a_n: int, b_hit: int, b_n: int) -> float | None:
    """Two-sided two-proportion z-test."""
    if not a_n or not b_n:
        return None
    p = (a_hit + b_hit) / (a_n + b_n)
    se = math.sqrt(p * (1 - p) * (1 / a_n + 1 / b_n))
    if se == 0:
        return None
    z = (a_hit / a_n - b_hit / b_n) / se
    return math.erfc(abs(z) / math.sqrt(2))


def share(rows: Iterable[dict[str, Any]], pred: Callable[[dict[str, Any]], bool]) -> tuple[int, int]:
    rows = list(rows)
    return sum(1 for r in rows if pred(r)), len(rows)


def pct(hit: int, n: int) -> str:
    return f"{hit / n:.0%} ({hit}/{n})" if n else "n/a"


def investigate(cfg: Config, baseline: Path, prompt_run: Path | None, out: Path,
                binary: Path, answer_wrong_manifest: Path) -> dict[str, Any]:
    est = Estimator(cfg.cost.tokenizer)
    base = read_runs_csv(baseline / "runs.csv")
    on_run = read_runs_csv(prompt_run / "runs.csv") if prompt_run else {}
    tau_runs = load_tau(cfg)
    runs = [r for r, _ in tau_runs]
    d = cfg.datasets.swe
    swe_runs, _ = swe.load(d["repo"], int(d["shards"]), int(d["shards_total"]), int(d["sample"]), cfg.seed,
                           cfg.resolve(cfg.cache_dir))
    work = out / "work"
    work.mkdir(parents=True, exist_ok=True)
    aw_manifest = str(answer_wrong_manifest.resolve())

    # Step 1: the exact requests, per setting. first/off is what the baseline sent.
    variants: dict[str, tuple[list[Run], dict[str, Any]]] = {
        "tau_first_off": (runs, {}),
        "tau_first_on": (runs, {"system_prompt": True}),
        "tau_grounded_off": (runs, {"evidence": "grounded", "manifest": aw_manifest}),
        "swe_first_off": (swe_runs, {}),
        "swe_grounded_off": (swe_runs, {"evidence": "grounded", "manifest": aw_manifest}),
    }
    captured: dict[str, dict[str, dict[str, Any]]] = {}
    for i, (name, (rs, scorer)) in enumerate(variants.items()):
        captured[name] = capture(rs, binary, work, scorer, port=18190 + 2 * i, model_port=18191 + 2 * i)

    def req_tokens(req: dict[str, Any]) -> int:
        return est.count(req["state"]) + est.count(json.dumps(req["questions"]))

    def per_run(name: str, rs: list[Run]) -> list[dict[str, Any]]:
        rows = []
        for r in rs:
            req = captured[name][r.trace_id]
            if "error" in req:
                rows.append({"trace_id": r.trace_id, "run_id": r.run_id, "error": req["error"]})
                continue
            vis = audit.visibility(req["state"], [c.output for c in r.tool_calls])
            with_out = [v for v, c in zip(vis, r.tool_calls, strict=True) if c.output.strip()]
            rows.append({
                "trace_id": r.trace_id, "run_id": r.run_id, "dataset": r.dataset, "failed": int(r.failed),
                "tool_calls": len(r.tool_calls), "outputs": len(with_out),
                "full": with_out.count("full"), "partial": with_out.count("partial"), "none": with_out.count("none"),
                "visible_share": (len(with_out) - with_out.count("none")) / len(with_out) if with_out else 1.0,
                "state_chars": len(req["state"]), "est_tokens": req_tokens(req),
                "trimmed": req["state"].count(" [trimmed]"),
                "system_prompt_cut": int(len(r.system_prompt) > 8000 and "SYSTEM PROMPT:" in str(req["state"])),
                "vis": vis,
            })
        return rows

    vis_rows = {name: per_run(name, rs) for name, (rs, _) in variants.items()}

    # Steps 2 to 5 on τ-bench.
    grounding_rows: list[dict[str, Any]] = []
    gold_rows: list[dict[str, Any]] = []
    run_rows: list[dict[str, Any]] = []
    vis_first = {r["trace_id"]: r["vis"] for r in vis_rows["tau_first_off"] if "vis" in r}
    vis_grounded = {r["trace_id"]: r["vis"] for r in vis_rows["tau_grounded_off"] if "vis" in r}
    for r, row in tau_runs:
        b = base.get(r.run_id, {})
        aw = fnum(b.get("answer_answer_wrong"))
        writes = audit.ground(row["traj"], r.system_prompt)
        gold = audit.compare_gold(writes, ((row.get("info") or {}).get("task") or {}).get("actions") or [], row["traj"])
        answers = {q: fnum(b.get(f"answer_{q}")) for q in QUESTIONS}
        flagged = aw is not None and aw >= FLAG_AT
        vf, vg = vis_first.get(r.trace_id, []), vis_grounded.get(r.trace_id, [])
        n_ungrounded = 0
        for w in writes:
            srcs = sorted({lf.tool_index for lf in w.leaves if lf.tool_index is not None})
            ung = w.ungrounded
            n_ungrounded += bool(ung)
            grounding_rows.append({
                "run_id": r.run_id, "trace_id": r.trace_id, "subset": r.subset, "failed": int(r.failed),
                "answer_wrong": aw, "flagged": int(flagged),
                "call_index": w.call_index, "tool": w.name,
                "tool_span_id": hex_id("tool", r.trace_id, str(w.call_index), size=8),
                "leaves_checked": sum(1 for lf in w.leaves if lf.kind in audit.CHECKED_KINDS),
                "ungrounded": int(bool(ung)),
                "ungrounded_leaves": "; ".join(f"{lf.path}={lf.value}" for lf in ung),
                "derived_values": "; ".join(f"{lf.path}={lf.value}" for lf in w.derived),
                "sources": "; ".join(f"{lf.path}<-{lf.source}" for lf in w.leaves if lf.source),
                "grounding_results": " ".join(str(s) for s in srcs),
                "calls_between": w.calls_between, "chars_between": w.chars_between,
                "grounding_visible_first": (int(all(vf[s] != "none" for s in srcs)) if srcs and vf else None),
                "grounding_visible_grounded": (int(all(vg[s] != "none" for s in srcs)) if srcs and vg else None),
                "acted_without_lookup": int(w.acted_without_lookup),
            })
        for g in gold.rows:
            gold_rows.append({"run_id": r.run_id, "trace_id": r.trace_id, "subset": r.subset, "failed": int(r.failed),
                              "answer_wrong": aw, "flagged": int(flagged), **g,
                              "tool_span_id": (hex_id("tool", r.trace_id, str(g["call_index"]), size=8)
                                               if g["call_index"] is not None else "")})
        vis_share = (sum(1 for v, c in zip(vf, r.tool_calls, strict=False) if c.output.strip() and v != "none")
                     / max(1, sum(1 for c in r.tool_calls if c.output.strip()))) if vf else None
        srcs_all = {lf.tool_index for w in writes for lf in w.leaves if lf.tool_index is not None}
        run_rows.append({
            "task": f"{r.meta['domain']}:{r.meta['task_id']}",
            "visible_share_first": vis_share,
            "grounding_visible_first": (int(all(vf[s] != "none" for s in srcs_all)) if srcs_all and vf else None),
            "gold_rows_wrong": sum(1 for g in gold.rows if g["category"] != "correct"),
            "run_id": r.run_id, "trace_id": r.trace_id, "subset": r.subset, "failed": int(r.failed),
            "answer_wrong": aw, "flagged": int(flagged), "flagged_six_any_of": int(b.get("kept") == "1"),
            "cause": b.get("cause", ""), "writes": len(writes), "ungrounded_writes": n_ungrounded,
            "acted_without_lookup": int(any(g["acted_without_lookup"] for g in grounding_rows
                                            if g["trace_id"] == r.trace_id)),
            "wrong_args": int(gold.has("wrong_args")),
            "grounded_wrong": int(any(g["grounded_wrong"] for g in gold.rows if g["category"] == "wrong_args")),
            "variant_mixup": int(any(g["variant_mixup"] for g in gold.rows)),
            "mixup_detectable": next((int(g["mixup_detectable_from_trace"]) for g in gold.rows
                                      if g["variant_mixup"] and g.get("mixup_detectable_from_trace") is not None), None),
            "extra_write": int(gold.has("extra")), "missed_write": int(gold.has("missed")),
            "tool_calls": len(r.tool_calls), "tokens_off": fnum(b.get("tokens")),
            "tokens_on": fnum(on_run.get(r.run_id, {}).get("tokens")),
            **{f"fires_{q}": (int(v >= FLAG_AT) if v is not None else None) for q, v in answers.items()},
        })

    # Tasks whose gold actions every successful run carried out exactly: on the
    # others the logged gold disagrees with how runs were scored.
    bad_tasks = {r["task"] for r in run_rows if not r["failed"] and r["gold_rows_wrong"]}
    consistent = {row_["trace_id"]: int(row_["task"] not in bad_tasks) for row_ in run_rows}
    for row_ in run_rows:
        row_["gold_consistent_task"] = consistent[row_["trace_id"]]
    for g in gold_rows:
        g["gold_consistent_task"] = consistent[g["trace_id"]]

    write_csv(out / "grounding.csv", grounding_rows)
    write_csv(out / "wrong_args.csv", gold_rows)
    write_csv(out / "tau_runs.csv", run_rows)
    for name, rows in vis_rows.items():
        write_csv(work / f"visibility_{name}.csv", [{k: v for k, v in r.items() if k != "vis"} for r in rows])

    summary = {
        "step1_visibility": {name: visibility_summary(rows) for name, rows in vis_rows.items()},
        "step2_grounding": grounding_tables(run_rows, grounding_rows),
        "step3_wrong_args": {
            "all_tasks": gold_tables(run_rows),
            "gold_consistent_tasks": gold_tables([r for r in run_rows if r["gold_consistent_task"]]),
            "tasks": len({r["task"] for r in run_rows}),
            "gold_consistent_task_count": len({r["task"] for r in run_rows if r["gold_consistent_task"]}),
        },
        "step4_audit": audit_tables(run_rows),
        "step5_distance": distance_tables(grounding_rows, run_rows),
        "step2_counts": {"runs": len(run_rows), "write_calls": len(grounding_rows),
                         "write_calls_ungrounded": sum(g["ungrounded"] for g in grounding_rows)},
        "h2_tokens": token_summary(run_rows, vis_rows),
    }
    summary["step6_samples"] = samples(run_rows, captured["tau_first_off"], base, tau_runs, grounding_rows, gold_rows,
                                       out / "samples", cfg.seed)
    write_json(out / "summary.json", summary)
    return summary


def visibility_summary(rows: list[dict[str, Any]]) -> dict[str, Any]:
    ok = [r for r in rows if "vis" in r]
    outs = sum(r["outputs"] for r in ok)
    return {
        "runs": len(rows), "errors": len(rows) - len(ok),
        "tool_outputs": outs,
        "outputs_full": sum(r["full"] for r in ok), "outputs_partial": sum(r["partial"] for r in ok),
        "outputs_none": sum(r["none"] for r in ok),
        "share_reaching_model": (outs - sum(r["none"] for r in ok)) / outs if outs else None,
        "per_run_visible_share_median": st.median(r["visible_share"] for r in ok) if ok else None,
        "runs_seeing_every_output": sum(1 for r in ok if r["none"] == 0),
        "est_tokens_median": st.median(r["est_tokens"] for r in ok) if ok else None,
        "est_tokens_max": max((r["est_tokens"] for r in ok), default=None),
        "state_chars_median": st.median(r["state_chars"] for r in ok) if ok else None,
        "results_trimmed": sum(r["trimmed"] for r in ok),
        "system_prompt_cut": sum(r["system_prompt_cut"] for r in ok),
    }


def token_summary(run_rows: list[dict[str, Any]], vis_rows: dict[str, list[dict[str, Any]]]) -> dict[str, Any]:
    off = [r["tokens_off"] for r in run_rows if r["tokens_off"]]
    on = [r["tokens_on"] for r in run_rows if r["tokens_on"]]
    est_off = {r["trace_id"]: r["est_tokens"] for r in vis_rows["tau_first_off"] if "est_tokens" in r}
    ratio = [r["tokens_off"] / est_off[r["trace_id"]] for r in run_rows
             if r["tokens_off"] and est_off.get(r["trace_id"])]
    calib = st.median(ratio) if ratio else None
    g = [r["est_tokens"] for r in vis_rows["tau_grounded_off"] if "est_tokens" in r]
    sg = [r["est_tokens"] for r in vis_rows["swe_grounded_off"] if "est_tokens" in r]
    sf = [r["est_tokens"] for r in vis_rows["swe_first_off"] if "est_tokens" in r]
    return {
        "jev_tokens_system_prompt_off": {"median": st.median(off), "max": max(off), "runs": len(off)} if off else None,
        "jev_tokens_system_prompt_on": {"median": st.median(on), "max": max(on), "runs": len(on)} if on else None,
        "jev_context_limit_tokens": 64000,
        "runs_over_32k_off": sum(1 for t in off if t > 32000), "runs_over_32k_on": sum(1 for t in on if t > 32000),
        "estimate_to_jev_ratio_median": calib,
        "tau_grounded_est_tokens_median": st.median(g) if g else None,
        "tau_grounded_jev_tokens_projected_median": st.median(g) * calib if g and calib else None,
        "tau_grounded_projected_cost_usd": sum(g) * (calib or 1) * 0.042 / 1e6 if g else None,
        "swe_first_est_tokens_median": st.median(sf) if sf else None,
        "swe_grounded_est_tokens_median": st.median(sg) if sg else None,
        "swe_grounded_projected_cost_usd": sum(sg) * (calib or 1) * 0.042 / 1e6 if sg else None,
    }


def grounding_tables(run_rows: list[dict[str, Any]], grounding_rows: list[dict[str, Any]]) -> dict[str, Any]:
    out: dict[str, Any] = {"by_run": {}, "by_write": {}}
    for outcome in ("failed", "succeeded"):
        for ung in (True, False):
            rs = [r for r in run_rows if r["writes"] and bool(r["failed"]) == (outcome == "failed")
                  and (r["ungrounded_writes"] > 0) == ung]
            hit, n = share(rs, lambda r: r["flagged"] == 1)
            out["by_run"][f"{outcome}, {'≥1 ungrounded write' if ung else 'all writes grounded'}"] = \
                {"runs": n, "flagged": hit, "flag_rate": hit / n if n else None}
    rs = [r for r in run_rows if not r["writes"]]
    hit, n = share(rs, lambda r: r["flagged"] == 1)
    out["by_run"]["no write calls"] = {"runs": n, "flagged": hit, "flag_rate": hit / n if n else None,
                                       "failed": sum(r["failed"] for r in rs)}
    kinds = Counter(lf.split("=")[0].split(".")[-1].split("[")[0] for g in grounding_rows
                    for lf in g["ungrounded_leaves"].split("; ") if lf)
    out["ungrounded_argument_keys"] = dict(kinds.most_common(15))
    for tool in sorted({g["tool"] for g in grounding_rows}):
        gs = [g for g in grounding_rows if g["tool"] == tool]
        out["by_write"][tool] = {"calls": len(gs), "ungrounded": sum(g["ungrounded"] for g in gs)}
    ung_runs = [r for r in run_rows if r["ungrounded_writes"]]
    out["ungrounded_runs_outcome"] = {"runs": len(ung_runs), "failed": sum(r["failed"] for r in ung_runs)}
    return out


def gold_tables(run_rows: list[dict[str, Any]]) -> dict[str, Any]:
    failed = [r for r in run_rows if r["failed"]]
    cats: list[tuple[str, Callable[[dict[str, Any]], bool]]] = [
        ("variant mix-up (grounded wrong item)", lambda r: r["variant_mixup"] == 1),
        ("other grounded-but-wrong arguments", lambda r: r["grounded_wrong"] == 1 and r["variant_mixup"] == 0),
        ("wrong arguments, not grounded", lambda r: r["wrong_args"] == 1 and r["grounded_wrong"] == 0),
        ("extra write (not in gold)", lambda r: r["wrong_args"] == 0 and r["extra_write"] == 1),
        ("missed write only", lambda r: r["wrong_args"] == 0 and r["extra_write"] == 0 and r["missed_write"] == 1),
        ("writes match gold (failed on something else)", lambda r: not (r["wrong_args"] or r["extra_write"]
                                                                        or r["missed_write"])),
    ]
    out: dict[str, Any] = {}
    for label, pred in cats:
        rs = [r for r in failed if pred(r)]
        hit, n = share(rs, lambda r: r["flagged"] == 1)
        out[label] = {"failed_runs": n, "flagged": hit, "flag_rate": hit / n if n else None}
    mix = [r for r in failed if r["variant_mixup"]]
    for label, val in (("variant mix-up, user stated the distinguishing option", 1),
                       ("variant mix-up, user never stated it", 0)):
        rs = [r for r in mix if r["mixup_detectable"] == val]
        hit, n = share(rs, lambda r: r["flagged"] == 1)
        out[label] = {"failed_runs": n, "flagged": hit, "flag_rate": hit / n if n else None}
    out["variant mix-up, could not tell"] = {"failed_runs": sum(1 for r in mix if r["mixup_detectable"] is None)}
    succ = [r for r in run_rows if not r["failed"]]
    out["_succeeded_with_write_discrepancy"] = sum(1 for r in succ if r["wrong_args"] or r["extra_write"]
                                                   or r["missed_write"])
    return out


def audit_tables(run_rows: list[dict[str, Any]]) -> dict[str, Any]:
    succ = [r for r in run_rows if not r["failed"]]
    problem: Callable[[dict[str, Any]], bool] = lambda r: r["ungrounded_writes"] > 0 or r["acted_without_lookup"] == 1
    out: dict[str, Any] = {}
    for label, rs in (("flagged successes", [r for r in succ if r["flagged"]]),
                      ("unflagged successes", [r for r in succ if not r["flagged"]])):
        p, n = share(rs, problem)
        u, _ = share(rs, lambda r: r["ungrounded_writes"] > 0)
        a, _ = share(rs, lambda r: r["acted_without_lookup"] == 1)
        out[label] = {"runs": n, "process_problem": p, "share": p / n if n else None,
                      "ungrounded_write": u, "acted_without_lookup": a}
    checks = {}
    for q in QUESTIONS:
        row: dict[str, Any] = {}
        for outcome in ("succeeded", "failed"):
            rs = [r for r in run_rows if r["writes"] and bool(r["failed"]) == (outcome == "failed")
                  and r[f"fires_{q}"] is not None]
            ug = [r for r in rs if r["ungrounded_writes"] > 0]
            gr = [r for r in rs if r["ungrounded_writes"] == 0]
            a, an = share(ug, lambda r: r[f"fires_{q}"] == 1)
            b, bn = share(gr, lambda r: r[f"fires_{q}"] == 1)
            row[outcome] = {"fires_on_ungrounded": a / an if an else None, "n_ungrounded": an,
                            "fires_on_grounded": b / bn if bn else None, "n_grounded": bn,
                            "difference_pts": (a / an - b / bn) * 100 if an and bn else None,
                            "p_value": p_value(a, an, b, bn)}
        s = row["succeeded"]
        diff, p = s["difference_pts"], s["p_value"]
        if s["n_ungrounded"] < 20:
            row["verdict"] = f"too few ungrounded successful runs to tell (n={s['n_ungrounded']})"
        else:
            row["verdict"] = ("process signal" if diff is not None and diff >= 10 and p is not None and p < 0.01
                              else "noise (no link to ungrounded actions)")
        checks[q] = row
    out["checks_vs_grounding"] = checks
    # Does a check fire because the model cannot see what the agent's actions
    # rest on? Successful runs only, so the outcome is held fixed.
    vis: dict[str, Any] = {}
    preds: list[tuple[str, Callable[[dict[str, Any]], bool]]]
    for q in QUESTIONS:
        row = {}
        preds = [("grounding visible", lambda r: r["grounding_visible_first"] == 1),
                            ("grounding not visible", lambda r: r["grounding_visible_first"] == 0),
                            ("≥50% of results visible", lambda r: (r["visible_share_first"] or 0) >= 0.5),
                            ("<25% of results visible", lambda r: (r["visible_share_first"] or 0) < 0.25)]
        for label, pred in preds:
            rs = [r for r in succ if r[f"fires_{q}"] is not None and pred(r)]
            hit, n = share(rs, lambda r: r[f"fires_{q}"] == 1)
            row[label] = {"runs": n, "fires": hit / n if n else None}
        a, b = row["grounding visible"], row["grounding not visible"]
        row["p_value_visible_vs_not"] = p_value(round((a["fires"] or 0) * a["runs"]), a["runs"],
                                                round((b["fires"] or 0) * b["runs"]), b["runs"])
        vis[q] = row
    out["checks_vs_visibility_successes"] = vis
    return out


def distance_tables(grounding_rows: list[dict[str, Any]], run_rows: list[dict[str, Any]]) -> dict[str, Any]:
    by_run = {r["trace_id"]: r for r in run_rows}
    failed_writes = [g for g in grounding_rows if g["failed"] and g["calls_between"] is not None]

    def miss(gs: list[dict[str, Any]]) -> dict[str, Any]:
        runs = {g["trace_id"] for g in gs}
        m = sum(1 for t in runs if not by_run[t]["flagged"])
        return {"failed_runs": len(runs), "missed": m, "miss_rate": m / len(runs) if runs else None}

    out: dict[str, Any] = {"by_grounding_visible_first": {}, "by_calls_between": {}, "by_chars_between": {},
                           "by_trace_length": {}}
    for v in (1, 0):
        out["by_grounding_visible_first"]["visible" if v else "not visible"] = \
            miss([g for g in failed_writes if g["grounding_visible_first"] == v])
    for lo, hi in ((0, 1), (2, 3), (4, 99)):
        out["by_calls_between"][f"{lo}-{hi if hi < 99 else '+'}"] = \
            miss([g for g in failed_writes if lo <= g["calls_between"] <= hi])
    for lo, hi in ((0, 2000), (2000, 8000), (8000, 10**9)):
        out["by_chars_between"][f"{lo}-{hi if hi < 10**9 else '+'} chars"] = \
            miss([g for g in failed_writes if lo <= g["chars_between"] < hi])
    failed = [r for r in run_rows if r["failed"]]
    for lo, hi in ((0, 4), (5, 8), (9, 99)):
        rs = [r for r in failed if lo <= r["tool_calls"] <= hi]
        m = sum(1 for r in rs if not r["flagged"])
        out["by_trace_length"][f"{lo}-{hi if hi < 99 else '+'} tool calls"] = \
            {"failed_runs": len(rs), "missed": m, "miss_rate": m / len(rs) if rs else None}
    return out


def samples(run_rows: list[dict[str, Any]], requests: dict[str, dict[str, Any]], base: dict[str, dict[str, str]],
            tau_runs: list[tuple[Run, dict[str, Any]]], grounding_rows: list[dict[str, Any]],
            gold_rows: list[dict[str, Any]], out: Path, seed: int) -> dict[str, list[str]]:
    out.mkdir(parents=True, exist_ok=True)
    by_id = {r.trace_id: (r, row) for r, row in tau_runs}
    classes: dict[str, Callable[[dict[str, Any]], bool]] = {
        "failed_rated_fine": lambda r: r["failed"] and not r["flagged"],
        "succeeded_flagged": lambda r: not r["failed"] and r["flagged"],
        "failed_flagged": lambda r: r["failed"] and r["flagged"],
    }
    rng = random.Random(seed)
    picked: dict[str, list[str]] = {}
    for name, pred in classes.items():
        pool = sorted((r for r in run_rows if r["answer_wrong"] is not None and pred(r)), key=lambda r: r["run_id"])
        chosen = rng.sample(pool, min(10, len(pool)))
        picked[name] = [r["run_id"] for r in chosen]
        for k, r in enumerate(chosen):
            run, _ = by_id[r["trace_id"]]
            b = base[r["run_id"]]
            spans = {"root": hex_id("root", run.trace_id, size=8),
                     "tools": [{"index": i, "name": c.name, "span_id": hex_id("tool", run.trace_id, str(i), size=8)}
                               for i, c in enumerate(run.tool_calls)]}
            write_json(out / f"{name}_{k:02d}.json", {
                "run_id": r["run_id"], "trace_id": run.trace_id, "class": name, "failed": bool(r["failed"]),
                "spans": spans,
                "jev_request": requests[run.trace_id],
                "jev_response": {
                    "p_keep": fnum(b.get("p_keep")), "decision_six_any_of": "keep" if b.get("kept") == "1" else "drop",
                    "answers": {q: fnum(b.get(f"answer_{q}")) for q in QUESTIONS},
                    "flags": b.get("flags", ""), "cause": b.get("cause", ""), "tokens": fnum(b.get("tokens")),
                },
                "grounding": [g for g in grounding_rows if g["trace_id"] == run.trace_id],
                "gold": [g for g in gold_rows if g["trace_id"] == run.trace_id],
                "tau_reward": run.meta.get("reward"),
            })
    return picked
