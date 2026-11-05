import json
from pathlib import Path

import pytest

from tailwise_bench import analyze
from tailwise_bench.config import Config


def test_severity_averages_the_answered_deciding_questions():
    deciding = ["answer_wrong", "action_wrong", "unhandled_request"]
    # action_wrong failed: left out, not counted as 0.
    assert analyze.severity({"answer_wrong": 0.6, "unhandled_request": 0.3}, deciding) == pytest.approx(0.45)
    assert analyze.severity({"answer_wrong": 0.6, "action_wrong": 0.0, "unhandled_request": 0.3}, deciding) == pytest.approx(0.3)


def test_deciding_questions_follow_the_manifest(tmp_path: Path):
    assert Config(path=tmp_path / "c.yaml").deciding_questions("answer_wrong") == ["answer_wrong"]
    manifest = tmp_path / "m.json"
    manifest.write_text(json.dumps({"questions": {}, "decision": {"any_of": ["missing_tool_calls"]},
                                    "actions": {"cancel": "cancel an order"}}))
    cfg = Config(path=tmp_path / "c.yaml")
    object.__setattr__(cfg, "tailwise", type(cfg.tailwise)(manifest=str(manifest)))
    assert cfg.deciding_questions("answer_wrong") == [
        "answer_wrong", "missing_tool_calls", "action_wrong", "unhandled_request"]


def test_apply_rules_mirrors_the_engine():
    deciding = ["answer_wrong"]
    answers = {"answer_wrong": 0.2}
    row = {"severity": 0.2, "kept": 0, "tokens": 500, "cost": 0.1}
    signal = [{"name": "loop", "keep": False, "fired": True}, {"name": "long", "keep": False, "fired": False}]
    analyze.apply_rules(row, signal, answers, deciding)
    assert row["severity"] == pytest.approx(0.4) and row["kept"] == 0 and row["tokens"] == 500
    assert row["rules_fired"] == "loop" and row["rule_keep"] == 0 and row["severity_rules"] == 0.5

    row = {"severity": 0.2, "kept": 0, "tokens": 500, "cost": 0.1}
    analyze.apply_rules(row, [*signal, {"name": "json", "keep": True, "fired": True}], answers, deciding)
    # A keep rule decides without Jev: kept, first in the review order, no tokens.
    assert row["severity"] == 1 and row["kept"] == 1 and row["tokens"] == 0 and row["cost"] == 0
    assert row["rules_fired"] == "loop; json" and row["severity_rules"] == 1
