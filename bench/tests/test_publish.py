import json
from pathlib import Path

from tailwise_bench import publish


def summary(variant: str) -> dict:
    review = {"review": {"at_rate": {"0.1": {"precision": 0.4, "ci95": [0.2, 0.6], "reviewed": 40,
                                             "vs_baseline": {"gain": 0.1, "ci95": [-0.1, 0.3], "not_better": 0.2}}}},
              "roc_auc": 0.7}
    group = {"runs_scored": 100, "failed": 40, "tokens": 300_000, "cost_per_run_usd": 0.0002, "default": review}
    meta = {"run_id": "20261105T121615Z_abc", "variant": variant, "jev_models": ["jev-1"],
            "source": {"base_sha": "abc"}, "config": {"review_budget": 0.05,
                                                      "cost": {"usd_per_million_input_tokens": 0.042}}}
    return {"meta": meta, "datasets": {"tau": group}, "detectable": {"tau": group}}


def test_publish(tmp_path: Path) -> None:
    run = tmp_path / "run"
    run.mkdir()
    (run / "summary.json").write_text(json.dumps(summary("tau-actions+rules")))
    data = publish.write([run], tmp_path / "out" / "benchmark.json")
    assert data["date"] == "2026-11-05"
    assert data["scenario"]["reviews"] == 500 and data["scenario"]["random_found"] == 50
    assert [(r["slice"], r["setup"], r["rules"]) for r in data["results"]] == [
        ("all", "tau-actions", True), ("detectable", "tau-actions", True)]
    r = data["results"][0]
    assert r["found"] == 200 and r["found_ci95"] == [100, 300] and r["multiple"] == 4
    assert r["reviews_per_failure"] == 2.5 and r["random_reviews_for_same"] == 2000
    assert abs(r["jev_usd"] - 2) < 1e-9 and abs(r["jev_usd_per_extra_failure"] - 2 / 150) < 1e-9
    assert r["vs_baseline"]["against"] == "jev_alone" and not r["rough"]
