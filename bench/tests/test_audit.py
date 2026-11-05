"""The investigation's plain-code checks, on invented τ-bench-shaped runs."""

import json

from tailwise_bench.investigate import audit


def tc(i, name, args):
    return {"id": f"c{i}", "type": "function", "function": {"name": name, "arguments": json.dumps(args)}}


TRAJ = [
    {"role": "system", "content": "# Policy"},
    {"role": "user", "content": "Exchange the keyboard in order #W100 for the size L one."},
    {"role": "assistant", "content": None, "tool_calls": [tc(0, "get_order_details", {"order_id": "#W100"})]},
    {"role": "tool", "tool_call_id": "c0", "name": "get_order_details",
     "content": json.dumps({"order_id": "#W100", "items": [{"item_id": "1111111"}], "payment": "credit_card_42"})},
    {"role": "assistant", "content": None, "tool_calls": [tc(1, "get_product_details", {"product_id": "77"})]},
    {"role": "tool", "tool_call_id": "c1", "name": "get_product_details",
     "content": json.dumps({"variants": {"2222222": {"size": "M"}, "3333333": {"size": "L"}}})},
    {"role": "assistant", "content": None, "tool_calls": [tc(2, "exchange_delivered_order_items", {
        "order_id": "#W100", "item_ids": ["1111111"], "new_item_ids": ["2222222"], "payment_method_id": "credit_card_42"})]},
    {"role": "tool", "tool_call_id": "c2", "name": "exchange_delivered_order_items", "content": "{\"status\": \"ok\"}"},
    {"role": "assistant", "content": None, "tool_calls": [tc(3, "cancel_pending_order", {"order_id": "#W999", "reason": "no longer needed"})]},
    {"role": "tool", "tool_call_id": "c3", "name": "cancel_pending_order", "content": "Error: not found"},
]


def test_only_writes_are_checked():
    assert audit.is_write("exchange_delivered_order_items") and audit.is_write("book_reservation")
    assert not audit.is_write("get_user_details") and not audit.is_write("transfer_to_human_agents")


def test_kinds():
    assert audit.kind_of("order_id", "#W100") == "id"
    assert audit.kind_of("reason", "no longer needed") == "skip"
    assert audit.kind_of("cabin", "economy") == "enum"
    assert audit.kind_of("flights[0].date", "2024-05-20") == "date"
    assert audit.kind_of("passengers[0].first_name", "Yusuf") == "text"
    assert audit.kind_of("total_baggages", 2) == "count"
    assert audit.kind_of("payment_methods[0].amount", 348.5) == "amount"


def test_grounding_traces_each_argument_and_flags_the_made_up_one():
    writes = audit.ground(TRAJ, "# Policy")
    exchange, cancel = writes
    src = {lf.path: lf.source for lf in exchange.leaves}
    assert src["order_id"] == "user"
    assert src["item_ids[0]"] == "tool:0" and src["payment_method_id"] == "tool:0"
    assert src["new_item_ids[0]"] == "tool:1"
    assert exchange.ungrounded == []
    assert exchange.calls_between == 1  # the order lookup (call 0) was two calls back
    assert [lf.path for lf in cancel.ungrounded] == ["order_id"]  # #W999 was never seen


def test_ids_match_whole_tokens_only():
    assert audit.found_in("id", "#W100", "order #w100 is pending", natural=False)
    assert not audit.found_in("id", "#W10", "order #w100 is pending", natural=False)
    assert audit.found_in("date", "2024-05-20", "i want to fly on may 20", natural=True)
    assert not audit.found_in("date", "2024-05-20", "i want to fly on may 20", natural=False)
    assert audit.found_in("amount", 1200.0, "the total is 1,200.00", natural=False)


def test_gold_labels_a_grounded_variant_mixup():
    writes = audit.ground(TRAJ, "# Policy")
    gold = [{"name": "get_order_details", "kwargs": {"order_id": "#W100"}},
            {"name": "exchange_delivered_order_items", "kwargs": {
                "order_id": "#W100", "item_ids": ["1111111"], "new_item_ids": ["3333333"],
                "payment_method_id": "credit_card_42"}}]
    res = audit.compare_gold(writes, gold)
    by_tool = {r["tool"]: r for r in res.rows}
    ex = by_tool["exchange_delivered_order_items"]
    assert ex["category"] == "wrong_args" and ex["grounded_wrong"] and ex["variant_mixup"]
    assert "new_item_ids" in ex["diff"]
    assert by_tool["cancel_pending_order"]["category"] == "extra"


def test_gold_exact_match_and_missed():
    writes = audit.ground(TRAJ[:8], "# Policy")
    gold = [{"name": "exchange_delivered_order_items", "kwargs": {
        "order_id": "#W100", "item_ids": ["1111111"], "new_item_ids": ["2222222"], "payment_method_id": "credit_card_42"}},
        {"name": "cancel_pending_order", "kwargs": {"order_id": "#W100", "reason": "ordered by mistake"}}]
    cats = sorted(r["category"] for r in audit.compare_gold(writes, gold).rows)
    assert cats == ["correct", "missed"]


def test_visibility_reads_evidence_and_inline_results():
    outputs = ["yusuf_9620", json.dumps({"a": 1}), json.dumps({"b": 2})]
    first = ("USER QUESTION:\nq\n\nTOOL CALLS:\n1. find() -> ok\n2. get() -> ok\n3. get() -> ok\n\n"
             "EVIDENCE:\n[1] yusuf_9620\n[2] {\"a\": 1}\n\nFINAL ANSWER:\na")
    assert audit.visibility(first, outputs) == ["full", "full", "none"]
    grounded = ("USER QUESTION:\nq\n\nTOOL CALLS:\n1. find() -> ok: yusuf_9620\n2. get() -> ok\n"
                "3. get() -> ok:\n   {\"b\": 2}\n\nEVIDENCE:\n(none)\n\nFINAL ANSWER:\na")
    assert audit.visibility(grounded, outputs) == ["full", "none", "full"]
    trimmed = "USER QUESTION:\nq\n\nTOOL CALLS:\n1. find() -> ok: yusuf [trimmed]\n\nEVIDENCE:\n(none)\n\nFINAL ANSWER:\na"
    assert audit.visibility(trimmed, outputs[:1]) == ["partial"]


def test_mixup_detectable_only_when_the_user_named_the_difference():
    # Exact phrases only: a paraphrase ("not water resistant") is not matched.
    traj = [
        {"role": "user", "content": "I want the blue one, the not resistant kind."},
        {"role": "tool", "content": json.dumps({"variants": {
            "1": {"options": {"color": "blue", "water resistance": "IPX4"}},
            "2": {"options": {"color": "blue", "water resistance": "not resistant"}}}})},
    ]
    assert audit.mixup_detectable(traj, chosen=["1"], gold=["2"]) is True
    traj[0]["content"] = "I want the blue one."
    assert audit.mixup_detectable(traj, chosen=["1"], gold=["2"]) is False
    assert audit.mixup_detectable(traj, chosen=["1"], gold=["9"]) is None


def _mixup_run(task_id, reward, user, new_item, gold_item):
    """A run exchanging item 1 for new_item, where the gold wants gold_item."""
    traj = [
        {"role": "system", "content": "# Policy"},
        {"role": "user", "content": f"Exchange item 1 in order #W1. {user}"},
        {"role": "assistant", "content": None, "tool_calls": [tc(0, "get_product_details", {"product_id": "7"})]},
        {"role": "tool", "tool_call_id": "c0", "content": json.dumps({"variants": {
            "2": {"options": {"size": "M"}}, "3": {"options": {"size": "L"}}}})},
        {"role": "assistant", "content": None, "tool_calls": [tc(1, "exchange_delivered_order_items", {
            "order_id": "#W1", "item_ids": ["1"], "new_item_ids": [new_item]})]},
        {"role": "tool", "tool_call_id": "c1", "content": "{}"},
    ]
    gold = [{"name": "exchange_delivered_order_items",
             "kwargs": {"order_id": "#W1", "item_ids": ["1"], "new_item_ids": [gold_item]}}]
    return {"task_id": task_id, "reward": reward, "traj": traj, "info": {"task": {"actions": gold}}}


def test_trace_detectable_leaves_out_hidden_mixups_and_inconsistent_tasks():
    runs = [
        ("retail", _mixup_run(1, 1.0, "", "3", "3")),        # task 1: success, matches gold
        ("retail", _mixup_run(1, 0.0, "", "2", "3")),        # task 1: mix-up, the user never said L
        ("retail", _mixup_run(1, 0.0, "Size L.", "2", "3")),  # task 1: mix-up the user ruled out
        ("retail", _mixup_run(2, 1.0, "", "2", "3")),        # task 2: a success that disagrees with gold
        ("retail", _mixup_run(2, 0.0, "Size L.", "2", "3")),
    ]
    got = [(r["gold_consistent_task"], r["mixup_not_in_trace"], r["trace_detectable"])
           for r in audit.trace_detectable(runs)]
    assert got == [(1, 0, 1), (1, 1, 0), (1, 0, 1), (0, 0, 0), (0, 0, 0)]
