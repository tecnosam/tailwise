"""uv run bench run --datasets tau,swe[,trail] [--dry-run] [--limit N] [--config bench/config.yaml]"""

from __future__ import annotations

import argparse
import asyncio
import json
import subprocess
import sys
import time
from dataclasses import replace
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from . import analyze, report, rules, source
from .budget import BudgetGuard
from .cache import ScoreCache
from .client import ScoringClient
from .config import Config, load
from .loaders import swe, tau, trail
from .runner import RunResult, estimate, run
from .service import Tailwise, version
from .tokens import Estimator
from .types import Run

DEFAULT_CONFIG = Path(__file__).resolve().parents[2] / "config.yaml"
DATASETS = ("tau", "swe", "trail")


def git_sha(repo: Path) -> str:
    def git(*args: str) -> str:
        return subprocess.run(["git", *args], cwd=repo, capture_output=True, text=True).stdout.strip()
    sha = git("rev-parse", "--short", "HEAD") or "nogit"
    return sha + ("-dirty" if git("status", "--porcelain") else "")


def load_runs(cfg: Config, names: list[str], limit: int | None) -> tuple[list[Run], dict[str, Any]]:
    cache = cfg.resolve(cfg.cache_dir)
    runs: list[Run] = []
    stats: dict[str, Any] = {}
    for n in names:
        d = getattr(cfg.datasets, n)
        if n == "tau":
            r, stats[n] = tau.load(d["commit"], list(d["files"]), cache, limit)
        elif n == "swe":
            r, stats[n] = swe.load(d["repo"], int(d["shards"]), int(d["shards_total"]), int(d["sample"]), cfg.seed,
                                   cache, limit, int(d.get("first_shard", 0)))
        else:
            r, stats[n] = trail.load(d["repo"], list(d["splits"]), cache, limit)
        runs += r
    return runs, stats


def dry_run(cfg: Config, runs: list[Run], est: Estimator, cache: ScoreCache) -> int:
    rows = estimate(runs, est, cache, cfg)
    print(f"Dry run: no API calls. Tokens are tiktoken {cfg.cost.tokenizer} estimates of what Jev is sent "
          f"(state + {est.questions_tokens} tokens of questions per call).\n")
    print(f"{'dataset/subset':36} {'runs':>6} {'cached':>7} {'tokens':>12} {'avg/run':>8} {'cost':>9}")
    totals: dict[str, list[int]] = {}
    for key, r in sorted(rows.items()):
        avg = r["tokens"] // r["calls"] if r["calls"] else 0
        print(f"{key:36} {r['calls']:6} {r['cached']:7} {r['tokens']:12,} {avg:8,} ${cfg.cost.usd(r['tokens']):8.4f}")
        t = totals.setdefault(key.split("/")[0], [0, 0])
        t[0] += r["calls"]
        t[1] += r["tokens"]
    print()
    for ds, (calls, tokens) in totals.items():
        print(f"{ds:36} {calls:6} {'':7} {tokens:12,} {'':8} ${cfg.cost.usd(tokens):8.4f}")
    total = sum(t[1] for t in totals.values())
    print(f"{'total':36} {sum(t[0] for t in totals.values()):6} {'':7} {total:12,} {'':8} ${cfg.cost.usd(total):8.4f}")
    verdict = "within" if cfg.cost.usd(total) <= cfg.cost.budget_usd else "OVER"
    print(f"\nProjected ${cfg.cost.usd(total):.4f} is {verdict} the ${cfg.cost.budget_usd:.2f} budget cap "
          f"(Jev has billed about 25% more than these estimates).")
    return 0


def cmd_run(args: argparse.Namespace) -> int:
    cfg = load(Path(args.config))
    if args.manifest:
        cfg = replace(cfg, tailwise=replace(cfg.tailwise, manifest=str(Path(args.manifest).resolve())))
    if args.evidence:
        cfg = replace(cfg, tailwise=replace(cfg.tailwise, evidence=args.evidence))
    if args.rules:
        cfg = replace(cfg, tailwise=replace(cfg.tailwise, rules=str(Path(args.rules).resolve())))
    if args.budget is not None:
        cfg = replace(cfg, cost=replace(cfg.cost, budget_usd=args.budget))
    names = [d.strip() for d in args.datasets.split(",") if d.strip()]
    if unknown := set(names) - set(DATASETS):
        print(f"unknown datasets {sorted(unknown)}; choose from {', '.join(DATASETS)}", file=sys.stderr)
        return 2
    started = time.monotonic()
    runs, stats = load_runs(cfg, names, args.limit)
    est = Estimator(cfg.cost.tokenizer, cfg.manifest_text)
    cache = ScoreCache(cfg.resolve(cfg.cache_dir) / "scores.sqlite")
    try:
        if args.dry_run:
            return dry_run(cfg, runs, est, cache)

        sha = git_sha(cfg.base.parent)
        run_id = f"{datetime.now(UTC).strftime('%Y%m%dT%H%M%SZ')}_{sha}"
        out = cfg.resolve(cfg.results_dir) / run_id
        out.mkdir(parents=True)
        budget = BudgetGuard(cfg.cost.budget_usd, cfg.cost.usd(1))
        print(f"Scoring {len(runs)} runs ({cfg.variant} questions), cap ${cfg.cost.budget_usd:.2f}; results in {out}",
              file=sys.stderr)
        with Tailwise(cfg, out) as tw:
            async def go() -> RunResult:
                client = ScoringClient(cfg.tailwise.url, cfg.client, budget, cfg.seed)
                try:
                    return await run(runs, client, cache, est, cfg)
                finally:
                    await client.close()
            res = asyncio.run(go())
            tw_version = version(tw.binary) if cfg.tailwise.start else "external"

        sources = sorted({r.get("tokens_source", "") for r in res.results.values()} - {""})
        tokens = sum(int(r.get("tokens", 0)) for r in res.results.values())
        meta = {
            "source": source.record(cfg.base.parent, out),
            "baseline": Path(args.baseline).name if args.baseline else None,
            "run_id": run_id, "git_sha": sha, "tailwise_version": tw_version,
            "jev_models": sorted({str(r.get("model", "")) for r in res.results.values()} - {""}),
            "variant": cfg.variant, "datasets": names, "limit": args.limit,
            "scored": res.scored, "cached": res.cached, "failed": len(res.errors), "aborted": res.aborted,
            "total_tokens": tokens, "total_cost_usd": cfg.cost.usd(tokens),
            "spent_this_run_usd": budget.spent_usd, "tokens_this_run": budget.spend.tokens,
            "estimated_tokens_this_run": budget.spend.estimated_tokens,
            "token_source": ("as reported by Jev (usage.input_tokens)" if sources == ["api"] else
                             f"sources: {', '.join(sources) or 'none'} (estimate = tiktoken {cfg.cost.tokenizer})"),
            "duration_seconds": time.monotonic() - started,
            "dataset_stats": stats,
            "config": cfg.to_dict(),
        }
        if res.errors:
            report.write_json(out / "errors.json", [{"trace_id": k, "error": v} for k, v in res.errors.items()])
        questions = sorted({q for r in res.results.values() for q in (r.get("answers") or {})})
        verdicts = None
        if cfg.tailwise.rules:
            rule_files = rules.files(cfg.resolve(cfg.tailwise.rules), names)
            meta["rules"] = {ds: {"file": str(rule_files[ds]), "sha256": h} for ds, h in rules.digest(rule_files).items()}
            verdicts = rules.check(runs, rule_files, cfg.base.parent)
        rows = analyze.rows(runs, res.results, res.errors, cfg, questions, verdicts)
        if verdicts is not None:
            # A keep rule decides without Jev, so with the rules those runs cost nothing.
            meta["tokens_with_rules"] = sum(r["tokens"] for r in rows)
        summary = analyze.summary(rows, cfg)
        report.write_csv(out / "runs.csv", rows)
        report.write_json(out / "summary.json", {"meta": meta, **summary})
        plot_paths = report.make_plots(summary, analyze.roc_pr_curves(rows), out / "plots")
        baseline = json.loads((Path(args.baseline) / "summary.json").read_text()) if args.baseline else None
        notes = Path(args.notes).read_text() if args.notes else ""
        report.write_report(out, summary, meta, plot_paths, baseline, notes)
        print(f"\n{report.summary_paragraph(summary, meta)}\n\nWrote {out}")
        if res.aborted:
            print(f"Stopped before the budget cap: {res.aborted}. Rerun to resume from the cache.", file=sys.stderr)
            return 3
        return 0
    finally:
        cache.close()


def cmd_investigate(args: argparse.Namespace) -> int:
    from .investigate.run import investigate

    cfg = load(Path(args.config))
    if args.narrow_check or args.completeness_check:
        if not args.into:
            print("--narrow-check and --completeness-check need --into <an investigation folder>", file=sys.stderr)
            return 2
        grounded = Path(args.grounded_run) if args.grounded_run else None
        if args.narrow_check:
            from .investigate.narrow import GROUPS, narrow_check

            groups = [g.strip() for g in args.groups.split(",") if g.strip()] if args.groups else None
            if groups and (bad := set(groups) - set(GROUPS)):
                print(f"unknown groups {sorted(bad)}; choose from {', '.join(GROUPS)}", file=sys.stderr)
                return 2
            result = narrow_check(cfg, Path(args.into), Path(args.baseline), grounded, args.budget, groups,
                                  args.per_group, args.dry_run, "narrow_check_large" if groups else "narrow_check")
        else:
            from .investigate.completeness import completeness_check

            result = completeness_check(cfg, Path(args.into), Path(args.baseline), grounded, args.budget,
                                        args.control, args.dry_run)
        print(json.dumps(result, indent=2))
        return 3 if result.get("aborted") else 0
    run_id = f"{datetime.now(UTC).strftime('%Y%m%dT%H%M%SZ')}_investigation"
    out = cfg.resolve(cfg.results_dir) / run_id
    out.mkdir(parents=True)
    src = source.record(cfg.base.parent, out)
    started = time.monotonic()
    summary = investigate(cfg, Path(args.baseline), Path(args.prompt_run) if args.prompt_run else None, out,
                          cfg.resolve(cfg.tailwise.binary), Path(args.answer_wrong_manifest))
    meta = {"run_id": run_id, "source": src, "baseline": Path(args.baseline).name,
            "prompt_run": Path(args.prompt_run).name if args.prompt_run else None,
            "flag": "answer_wrong >= 0.5 (answer_wrong alone decides)", "seed": cfg.seed,
            "duration_seconds": time.monotonic() - started, "jev_calls": 0}
    report.write_json(out / "summary.json", {"meta": meta, **summary})
    print(f"Wrote {out}")
    return 0


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(prog="bench", description="Benchmark how well Tailwise picks failed agent runs.")
    sub = p.add_subparsers(dest="cmd", required=True)
    r = sub.add_parser("run", help="score the datasets and write a results folder")
    r.add_argument("--datasets", default="tau,swe", help=f"comma-separated: {', '.join(DATASETS)}")
    r.add_argument("--dry-run", action="store_true", help="estimate tokens and cost; no API calls")
    r.add_argument("--limit", type=int, default=None, help="at most N runs per dataset (a smoke test)")
    r.add_argument("--config", default=str(DEFAULT_CONFIG))
    r.add_argument("--manifest", help="score with this manifest instead of tailwise.manifest (a labelled variant)")
    r.add_argument("--budget", type=float, help="hard spending cap in dollars for this run (cost.budget_usd)")
    r.add_argument("--evidence", choices=["first", "grounded"], help="Tailwise's scorer.evidence for this run")
    r.add_argument("--rules", help="a folder of <dataset>.yaml heuristic rules, checked on the cached answers (no cost)")
    r.add_argument("--baseline", help="an earlier results folder: the report compares this run with it")
    r.add_argument("--notes", help="a Markdown file: what changed since the baseline, added to the report")
    inv = sub.add_parser("investigate", help="the offline τ-bench diagnosis: what Jev sees, grounding, gold actions")
    inv.add_argument("--baseline", default="results/20261031T171853Z_e4c33e4-dirty",
                     help="a results folder with Jev's answers for every run (system prompt off)")
    inv.add_argument("--prompt-run", default="results/20261031T190958Z_e4c33e4-dirty",
                     help="a results folder scored with the system prompt on, for its token counts")
    inv.add_argument("--answer-wrong-manifest", default="manifests/answer-wrong-only.json")
    inv.add_argument("--narrow-check", action="store_true",
                     help="step 7 (calls Jev): narrow checks of each write in the failed-but-rated-fine sample")
    inv.add_argument("--into", help="with --narrow-check: the investigation folder to read samples from and write to")
    inv.add_argument("--grounded-run", help="with --narrow-check: a results folder scored with evidence grounded")
    inv.add_argument("--budget", type=float, default=0.05, help="with an experiment: hard cap in dollars")
    inv.add_argument("--groups", help="with --narrow-check: detectable_mixup,grounded_wrong,extra,control "
                                      "(none: the failed-but-rated-fine sample)")
    inv.add_argument("--per-group", type=int, default=75, help="with --groups: writes sampled per group")
    inv.add_argument("--completeness-check", action="store_true",
                     help="experiment (calls Jev): did the agent handle every request the customer made?")
    inv.add_argument("--control", type=int, default=200, help="with --completeness-check: successful runs sampled")
    inv.add_argument("--dry-run", action="store_true", help="with an experiment: count and project cost, no calls")
    inv.add_argument("--config", default=str(DEFAULT_CONFIG))
    args = p.parse_args(argv)
    return cmd_investigate(args) if args.cmd == "investigate" else cmd_run(args)


if __name__ == "__main__":
    sys.exit(main())
