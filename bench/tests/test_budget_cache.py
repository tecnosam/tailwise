import pytest

from tailwise_bench.budget import BudgetExceeded, BudgetGuard
from tailwise_bench.cache import ScoreCache


def test_budget_refuses_a_call_that_would_go_over():
    g = BudgetGuard(cap_usd=1.0, usd_per_token=0.01)  # 100 tokens
    held = g.reserve(60)
    with pytest.raises(BudgetExceeded):
        g.reserve(60)  # 60 in flight + 61 more is over
    g.settle(held, 60, 50)
    assert g.spent_usd == pytest.approx(0.5)
    g.settle(g.reserve(40), 40, 40)  # 90 spent
    assert g.reserve(5) > 0  # 90 + 6 fits
    with pytest.raises(BudgetExceeded):
        g.reserve(5)  # 90 + 6 in flight + 6 does not


def test_budget_learns_when_estimates_run_low():
    g = BudgetGuard(cap_usd=1.0, usd_per_token=0.01)
    g.settle(g.reserve(10), 10, 20)  # billed twice the estimate
    with pytest.raises(BudgetExceeded):
        g.reserve(41)  # 80 + 41 * 2 > 100
    assert g.reserve(39) > 0


def test_failed_call_releases_its_reservation():
    g = BudgetGuard(cap_usd=1.0, usd_per_token=0.01)
    g.settle(g.reserve(90), 90, None)
    assert g.spent_usd == 0 and g.reserve(90) > 0


def test_cache_round_trip(tmp_path):
    c = ScoreCache(tmp_path / "s.sqlite")
    key = ("whowhen", "t", "s", "jev-latest", "abc")
    assert c.get(key) is None
    c.put(key, {"p_keep": 0.7})
    assert c.get(key) == {"p_keep": 0.7}
    assert c.get(("whowhen", "t", "s", "jev-latest", "other")) is None
    c.close()
    assert ScoreCache(tmp_path / "s.sqlite").get(key) == {"p_keep": 0.7}
