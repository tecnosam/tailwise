"""The PNG plots. One look throughout: Tailwise in the first series color,
answer_wrong alone in the second, random sampling in neutral gray with its
95% band, recessive axes."""

from __future__ import annotations

from collections.abc import Callable
from pathlib import Path
from typing import Any

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt  # noqa: E402

SURFACE = "#fcfcfb"
INK = "#0b0b0b"
INK_2 = "#52514e"
GRID = "#e4e3df"
SERIES = ["#2a78d6", "#eb6834", "#1baf7a"]  # fixed order: Tailwise default, answer_wrong only, third
BASELINE = "#8a8984"
NAMES = {"tau": "τ-bench", "swe": "SWE-agent", "trail": "TRAIL"}

plt.rcParams.update({
    "figure.facecolor": SURFACE, "axes.facecolor": SURFACE, "savefig.facecolor": SURFACE,
    "axes.edgecolor": GRID, "axes.labelcolor": INK_2, "axes.titlecolor": INK, "axes.titlesize": 11,
    "axes.titleweight": "bold", "axes.labelsize": 9, "xtick.color": INK_2, "ytick.color": INK_2,
    "xtick.labelsize": 8, "ytick.labelsize": 8, "axes.grid": True, "grid.color": GRID, "grid.linewidth": 0.6,
    "axes.axisbelow": True, "axes.spines.top": False, "axes.spines.right": False, "legend.frameon": False,
    "legend.fontsize": 8, "font.family": "sans-serif", "lines.linewidth": 2, "lines.markersize": 5,
})


def name(ds: str) -> str:
    return NAMES.get(ds, ds)


def _save(fig: Any, path: Path) -> Path:
    fig.tight_layout()
    fig.savefig(path, dpi=160)
    plt.close(fig)
    return path


def _panels(n: int, height: float = 3.7) -> tuple[Any, list[Any]]:
    fig, axes = plt.subplots(1, n, figsize=(4.4 * n, height), squeeze=False)
    return fig, list(axes[0])


def budget(datasets: dict[str, Any], out: Path) -> Path | None:
    if not datasets:
        return None
    fig, axes = _panels(len(datasets))
    for ax, (ds, s) in zip(axes, datasets.items(), strict=True):
        tw, aw = s["default"]["budget_curve"], s["answer_wrong_only"]["budget_curve"]
        xs = [p["budget"] * 100 for p in tw]
        ax.fill_between(xs, [p["random_recall_ci95"][0] for p in tw], [p["random_recall_ci95"][1] for p in tw],
                        color=BASELINE, alpha=0.18, linewidth=0, label="Random, 95% interval")
        ax.plot(xs, [p["random_recall_mean"] for p in tw], color=BASELINE, linestyle="--", label="Random sampling")
        ax.plot(xs, [p["best_possible_recall"] for p in tw], color=INK_2, linestyle=":", linewidth=1.2, label="Best possible")
        ax.plot(xs, [p["tailwise_recall"] for p in aw], color=SERIES[1], marker="o", label="answer_wrong only")
        ax.plot(xs, [p["tailwise_recall"] for p in tw], color=SERIES[0], marker="o", label="Tailwise (default)")
        ax.set(title=f"{name(ds)}: failed runs found", xlabel="Runs reviewed (% of all, highest score first)",
               ylabel="Share of failed runs found", ylim=(0, 1.02), xlim=(0, max(xs) + 2))
    axes[0].legend(loc="upper left")
    return _save(fig, out / "budget_curve.png")


def kept_precision(datasets: dict[str, Any], out: Path) -> Path | None:
    """Of the runs kept at the threshold, the share that failed, against the
    share that fails overall (what random keeping gets)."""
    if not datasets:
        return None
    labels = [name(ds) for ds in datasets]
    xs = range(len(labels))
    fig, ax = plt.subplots(figsize=(1.9 * len(labels) + 3, 3.8))
    groups: list[tuple[str, str, Callable[[dict[str, Any]], float | None]]] = [("Random keeping (failure rate)", BASELINE, lambda s: s["default"]["at_threshold"]["base_rate"]),
              ("answer_wrong only", SERIES[1], lambda s: s["answer_wrong_only"]["at_threshold"]["precision"]),
              ("Tailwise (default)", SERIES[0], lambda s: s["default"]["at_threshold"]["precision"])]
    w = 0.26
    for i, (label, color, get) in enumerate(groups):
        vals = [get(s) or 0 for s in datasets.values()]
        bars = ax.bar([x + (i - 1) * w for x in xs], vals, width=w - 0.02, color=color, label=label)
        for b, v in zip(bars, vals, strict=True):
            ax.text(b.get_x() + b.get_width() / 2, v + 0.015, f"{v:.0%}", ha="center", fontsize=7.5, color=INK_2)
    ax.set_xticks(list(xs), labels)
    ax.set(title="Of the runs kept, the share that failed", ylabel="Precision", ylim=(0, 1.12))
    ax.grid(axis="x", visible=False)
    ax.legend(loc="upper left", ncol=3)
    return _save(fig, out / "kept_precision.png")


def roc_pr(curves: dict[str, Any], datasets: dict[str, Any], out: Path) -> list[Path]:
    paths = []
    for kind, x, y, xl, yl, auc in (("roc", "fpr", "tpr", "False positive rate", "True positive rate", "roc_auc"),
                                    ("pr", "recall", "precision", "Recall", "Precision", "pr_auc")):
        fig, ax = plt.subplots(figsize=(4.8, 4.1))
        for i, ds in enumerate(curves):
            v = datasets[ds]["default"][auc]
            ax.plot(curves[ds][x], curves[ds][y], color=SERIES[i % 3],
                    label=f"{name(ds)} ({auc.upper().replace('_', '-')} {v:.3f})")
            if kind == "pr":
                base = datasets[ds]["default"]["pr_auc_random"]
                ax.axhline(base, color=SERIES[i % 3], linestyle="--", linewidth=0.8, alpha=0.6)
        if kind == "roc":
            ax.plot([0, 1], [0, 1], color=BASELINE, linestyle="--", linewidth=1, label="Random")
        ax.set(title=f"Tailwise score: {'ROC' if kind == 'roc' else 'precision-recall (dashed: random)'}",
               xlabel=xl, ylabel=yl, xlim=(0, 1), ylim=(0, 1.02))
        ax.legend(loc="lower right" if kind == "roc" else "lower left")
        paths.append(_save(fig, out / f"{kind}_curve.png"))
    return paths


def reliability(datasets: dict[str, Any], out: Path) -> Path | None:
    if not datasets:
        return None
    fig, ax = plt.subplots(figsize=(4.8, 4.1))
    ax.plot([0, 1], [0, 1], color=BASELINE, linestyle="--", linewidth=1, label="Perfectly calibrated")
    for i, (ds, s) in enumerate(datasets.items()):
        cal = s["default"]["calibration"]
        pts = [(b["mean_predicted"], b["fraction_failed"]) for b in cal["bins"] if b["count"]]
        ax.plot([p[0] for p in pts], [p[1] for p in pts], color=SERIES[i % 3], marker="o",
                label=f"{name(ds)} (ECE {cal['ece']:.3f})")
    ax.set(title="Is p_keep a probability of failure?", xlabel="Tailwise score (p_keep)",
           ylabel="Share of runs that failed", xlim=(0, 1), ylim=(0, 1.02))
    ax.legend(loc="upper left")
    return _save(fig, out / "reliability.png")


def flags(datasets: dict[str, Any], out: Path) -> Path | None:
    """Per question: how often it fires on failed runs and on successful ones."""
    if not datasets:
        return None
    qs = sorted({q for s in datasets.values() for q in s["flags"]})
    if not qs:
        return None
    fig, axes = _panels(len(datasets), height=0.42 * len(qs) + 1.6)
    for ax, (ds, s) in zip(axes, datasets.items(), strict=True):
        ys = range(len(qs))
        fail = [(s["flags"].get(q) or {}).get("recall") or 0 for q in qs]
        ok = [(s["flags"].get(q) or {}).get("false_positive_rate") or 0 for q in qs]
        ax.barh([y + 0.2 for y in ys], fail, height=0.38, color=SERIES[0], label="Fires on failed runs")
        ax.barh([y - 0.2 for y in ys], ok, height=0.38, color=BASELINE, label="Fires on successful runs")
        ax.set_yticks(list(ys), qs)
        ax.set(title=name(ds), xlabel="Share of runs", xlim=(0, 1.02))
        ax.grid(axis="y", visible=False)
    axes[0].legend(loc="lower right")
    fig.suptitle("Which checks tell failed runs apart", fontsize=11, fontweight="bold", color=INK)
    return _save(fig, out / "flags.png")


def cost(datasets: dict[str, Any], out: Path) -> Path | None:
    if not datasets:
        return None
    names = [name(ds) for ds in datasets]
    vals = [s["cost_usd"] for s in datasets.values()]
    fig, ax = plt.subplots(figsize=(6.6, 0.45 * len(names) + 1.3))
    ax.barh(names, vals, color=SERIES[0], height=0.6)
    top = max(vals) or 1
    for i, s in enumerate(datasets.values()):
        ax.text(vals[i] + top * 0.01, i, f"${vals[i]:.4f}  ({s['tokens']:,} tokens, ${s['cost_per_run_usd'] * 1000:.3f} per 1,000 runs)",
                va="center", fontsize=7.5, color=INK_2)
    ax.set(title="Cost by dataset", xlabel="US dollars (Jev input tokens)", xlim=(0, top * 2.2))
    ax.grid(axis="y", visible=False)
    return _save(fig, out / "cost_by_dataset.png")
