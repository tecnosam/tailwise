import pytest

from tailwise_bench import metrics


def test_threshold_metrics():
    m = metrics.threshold_metrics([1, 1, 0, 0, 1], [1, 0, 1, 0, 1], rates=(0.1,))
    assert (m["tp"], m["fp"], m["fn"], m["tn"]) == (2, 1, 1, 1)
    assert m["precision"] == pytest.approx(2 / 3)
    assert m["recall"] == pytest.approx(2 / 3)
    assert m["f1"] == pytest.approx(2 / 3)
    assert m["kept_share"] == pytest.approx(3 / 5)
    assert m["base_rate"] == pytest.approx(3 / 5)
    assert m["lift"] == pytest.approx((2 / 3) / (3 / 5))
    assert m["false_positive_rate"] == pytest.approx(1 / 2)
    # TPR 2/3, FPR 1/2 at a 10% failure rate: (0.0667) / (0.0667 + 0.45)
    assert m["precision_at_failure_rate"]["0.1"] == pytest.approx((2 / 30) / (2 / 30 + 0.45))


def test_threshold_metrics_nothing_kept():
    m = metrics.threshold_metrics([1, 0], [0, 0])
    assert m["precision"] is None and m["recall"] == 0 and m["f1"] is None and m["lift"] is None


def test_precision_at_rate():
    assert metrics.precision_at_rate(1.0, 0.0, 0.05) == 1.0
    assert metrics.precision_at_rate(0.5, 0.5, 0.2) == pytest.approx(0.2)  # no better than random
    assert metrics.precision_at_rate(None, 0.1, 0.1) is None


def test_ranking_metrics_perfect_and_single_class():
    m = metrics.ranking_metrics([0, 0, 1, 1], [0.1, 0.2, 0.8, 0.9])
    assert m["roc_auc"] == 1.0 and m["pr_auc"] == 1.0 and m["pr_auc_random"] == 0.5
    assert metrics.ranking_metrics([1, 1], [0.2, 0.3])["roc_auc"] is None


def test_budget_k_rounds_up_and_reviews_at_least_one():
    assert metrics.budget_k(0.01, 50) == 1
    assert metrics.budget_k(0.10, 95) == 10
    assert metrics.budget_k(1.0, 7) == 7
    assert metrics.budget_k(0.5, 0) == 0


def test_budget_curve_perfect_ranker_beats_random():
    y = [1] * 10 + [0] * 90
    s = [0.9] * 10 + [0.1] * 90
    curve = metrics.budget_curve(y, s, [0.10, 0.50], seeds=200, seed=0)
    p = curve[0]
    assert p["k"] == 10 and p["tailwise_recall"] == 1.0 and p["tailwise_precision"] == 1.0
    # Random picks k of n: expected recall k/n, expected precision the failure rate.
    assert p["random_recall_mean"] == pytest.approx(0.10, abs=0.03)
    assert p["random_precision_mean"] == pytest.approx(0.10, abs=0.03)
    lo, hi = p["random_recall_ci95"]
    assert lo <= p["random_recall_mean"] <= hi
    assert p["lift"] == pytest.approx(1 / p["random_recall_mean"])
    assert p["best_possible_recall"] == 1.0
    assert curve[1]["random_recall_mean"] == pytest.approx(0.50, abs=0.05)


def test_budget_curve_best_possible_is_capped_by_the_budget():
    y = [1] * 80 + [0] * 20
    p = metrics.budget_curve(y, list(range(100)), [0.10], seeds=5, seed=0)[0]
    assert p["best_possible_recall"] == pytest.approx(10 / 80)


def test_budget_curve_is_deterministic():
    y = [1, 0, 1, 0, 0, 1, 0, 0]
    s = [0.5] * 8  # all tied: order comes from the seeded tiebreak
    assert metrics.budget_curve(y, s, [0.3], 10, seed=3) == metrics.budget_curve(y, s, [0.3], 10, seed=3)


def test_rank_order_breaks_ties_randomly_but_reproducibly():
    import numpy as np
    s = np.array([0.5, 0.9, 0.5, 0.5])
    a = metrics.rank_order(s, seed=1)
    assert a[0] == 1 and sorted(a[1:].tolist()) == [0, 2, 3]
    assert (a == metrics.rank_order(s, seed=1)).all()


def test_calibration_perfect_and_off():
    assert metrics.calibration([0, 1, 0, 1], [0.0, 1.0, 0.0, 1.0])["ece"] == pytest.approx(0.0)
    off = metrics.calibration([0, 0], [0.95, 0.95])
    assert off["ece"] == pytest.approx(0.95)
    assert off["bins"][-1]["count"] == 2 and off["bins"][-1]["fraction_failed"] == 0.0
    assert len(off["bins"]) == 10


def test_calibration_score_of_one_lands_in_the_top_bin():
    assert metrics.calibration([1], [1.0])["bins"][-1]["count"] == 1


def test_flag_stats():
    out = metrics.flag_stats([1, 1, 0, 0], [["a", "b"], ["a"], ["a"], []])
    assert out["a"]["flagged"] == 3 and out["a"]["precision"] == pytest.approx(2 / 3)
    assert out["a"]["recall"] == 1.0 and out["a"]["false_positive_rate"] == 0.5
    assert out["b"] == {"flagged": 1, "flagged_failed": 1, "precision": 1.0, "recall": 0.5, "false_positive_rate": 0.0}
    assert list(out) == ["a", "b"]  # most flagged first


def test_review_precision_perfect_ranker_and_random():
    y = [1] * 50 + [0] * 950
    perfect = metrics.review_precision(y, [float(v) for v in y], 0.05, (0.10, 0.02), seeds=20, seed=0)
    assert perfect["real"]["precision"] == 1.0  # 50 reviewed, all 50 failures
    assert perfect["at_rate"]["0.1"]["precision"] == pytest.approx(1.0)  # more failures than reviewed
    # At 2% failing, the top 5% holds every failure and successes besides.
    assert perfect["at_rate"]["0.02"]["precision"] == pytest.approx(0.4, abs=0.02)
    assert perfect["at_rate"]["0.02"]["best_possible"] == pytest.approx(0.4)

    flat = metrics.review_precision(y, [0.5] * len(y), 0.05, (0.10,), seeds=200, seed=0)
    assert flat["at_rate"]["0.1"]["precision"] == pytest.approx(0.10, abs=0.02)  # ties: random review


def test_review_precision_single_class_has_no_rates():
    r = metrics.review_precision([0, 0, 0], [0.1, 0.2, 0.3], 0.05, (0.1,), seeds=5, seed=0)
    assert r["real"]["precision"] == 0 and r["at_rate"] == {}


def test_review_precision_resamples_tasks_and_compares_a_baseline():
    # Two tasks, each run 10 times: the better ranker sees failures first.
    y = [1] * 5 + [0] * 5 + [1] * 5 + [0] * 5
    good = [1.0 if v else 0.0 for v in y]
    groups = ["a"] * 10 + ["b"] * 10
    out = metrics.review_precision(y, good, 0.1, [0.5], seeds=20, seed=0, groups=groups,
                                   baseline=[0.5] * len(y))
    at = out["at_rate"]["0.5"]
    assert at["precision"] == 1.0
    assert at["vs_baseline"]["gain"] > 0 and at["vs_baseline"]["ci95"][0] >= 0
    assert 0 <= at["vs_baseline"]["not_better"] < 1
