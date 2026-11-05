"""The converters, on tiny invented runs (never real dataset rows)."""

import json

from tailwise_bench.loaders import swe, tau, trail
from tailwise_bench.otel import run_trace

TAU_RUN = {
    "task_id": 7, "reward": 0.0, "trial": 1, "info": {},
    "traj": [
        {"role": "system", "content": "# Policy\nOnly refund delivered orders."},
        {"role": "user", "content": "Hi, I want a refund for order #1."},
        {"role": "assistant", "content": None, "tool_calls": [
            {"id": "c1", "type": "function", "function": {"name": "get_order", "arguments": "{\"order_id\": \"#1\"}"}}]},
        {"role": "tool", "tool_call_id": "c1", "name": "get_order", "content": "{\"status\": \"pending\"}"},
        {"role": "assistant", "content": "Let me refund it.", "tool_calls": [
            {"id": "c2", "type": "function", "function": {"name": "refund", "arguments": "{\"order_id\": \"#1\"}"}}]},
        {"role": "tool", "tool_call_id": "c2", "name": "refund", "content": "Error: order is not delivered"},
        {"role": "assistant", "content": "Your refund is done."},
        {"role": "user", "content": "Thanks! ###STOP###"},
    ],
}

SWE_ROW = {
    "instance_id": "acme__widgets-1", "model_name": "swe-agent-llama-70b", "target": True,
    "exit_status": "submitted", "generated_patch": "diff --git a/w.py b/w.py\n+fixed\n",
    "trajectory": [
        {"role": "system", "text": "You are an autonomous programmer."},
        {"role": "user", "text": "We're solving this issue:\nISSUE:\nWidgets crash on empty input\n\nINSTRUCTIONS:\nFix it."},
        {"role": "ai", "text": "Let's look.\n```\nopen w.py\n```"},
        {"role": "user", "text": "1: def w(x):\n2:     return x[0]"},
        {"role": "ai", "text": "Done.\n```\nsubmit\n```"},
    ],
}


def spans(req):
    return req["resourceSpans"][0]["scopeSpans"][0]["spans"]


def attrs(span):
    return {a["key"]: next(iter(a["value"].values())) for a in span["attributes"]}


def test_tau_run():
    run = tau.parse("gpt-4o-retail", TAU_RUN)
    assert run.failed and run.subset == "retail/gpt-4o" and run.run_id == "gpt-4o-retail:7:1"
    assert run.question == "Hi, I want a refund for order #1.\n\nThanks!"
    assert run.final_answer == "Your refund is done."
    assert [c.name for c in run.tool_calls] == ["get_order", "refund"]
    assert run.tool_calls[0].args == {"order_id": "#1"} and not run.tool_calls[0].error
    assert run.tool_calls[1].error and run.tool_calls[1].output.startswith("Error")
    assert "Policy" not in run.question


def test_tau_policy_is_sent_as_the_first_llm_calls_system_message():
    run = tau.parse("gpt-4o-retail", TAU_RUN)
    assert run.system_prompt.startswith("# Policy")
    req, state = run_trace(run)
    llm = [s for s in spans(req) if attrs(s)["openinference.span.kind"] == "LLM"]
    assert len(llm) == 2  # the first call, with the policy; the last, with the conversation
    a = attrs(llm[0])
    assert a["llm.input_messages.0.message.role"] == "system"
    assert a["llm.input_messages.0.message.content"] == run.system_prompt
    last = attrs(llm[1])
    assert last["llm.input_messages.0.message.role"] == "user"
    # The run ends with the user's thanks: it is history, and there is no reply.
    assert last[f"llm.input_messages.{len(run.messages) - 1}.message.content"] == "Thanks!"
    assert "llm.output_messages.0.message.content" not in last
    assert state.startswith("SYSTEM PROMPT:\n# Policy\nOnly refund delivered orders.\n\nUSER QUESTION:")


def test_tau_success_is_reward_one():
    assert not tau.parse("gpt-4o-airline", {**TAU_RUN, "reward": 1.0}).failed


def test_swe_run():
    run = swe.parse(SWE_ROW, 3)
    assert not run.failed and run.subset == "swe-agent-llama-70b"
    assert run.question == "Widgets crash on empty input"
    assert [(c.name, c.args["command"]) for c in run.tool_calls] == [("open", "open w.py"), ("submit", "submit")]
    assert run.tool_calls[0].output.startswith("1: def w(x)")
    assert run.final_answer.startswith("diff --git")


def test_swe_failed_is_target_false():
    assert swe.parse({**SWE_ROW, "target": False}, 0).failed


def test_run_trace_is_one_agent_root_and_a_tool_span_per_call():
    run = tau.parse("gpt-4o-retail", TAU_RUN)
    req, state = run_trace(run)
    root, *tools = spans(req)
    r = attrs(root)
    assert r["openinference.span.kind"] == "AGENT" and "parentSpanId" not in root
    assert r["input.value"] == run.question and r["output.value"] == "Your refund is done."
    tools = [t for t in tools if attrs(t)["openinference.span.kind"] == "TOOL"]
    assert len(tools) == 2 and all(t["parentSpanId"] == root["spanId"] for t in tools)
    t0, t1 = attrs(tools[0]), attrs(tools[1])
    assert t0["tool.name"] == "get_order" and json.loads(t0["input.value"]) == {"order_id": "#1"}
    assert "status" not in tools[0] and tools[1]["status"]["code"] == 2
    assert len(root["traceId"]) == 32 and len({s["spanId"] for s in spans(req)}) == 5  # root, two LLM, two tools
    assert "2. refund({\"order_id\": \"#1\"}) -> error" in state
    assert "FINAL ANSWER:\nYour refund is done." in state


def test_run_trace_ids_are_stable_and_distinct():
    a, b = tau.parse("gpt-4o-retail", TAU_RUN), tau.parse("gpt-4o-retail", {**TAU_RUN, "trial": 2})
    assert a.trace_id == tau.parse("gpt-4o-retail", TAU_RUN).trace_id
    assert a.trace_id != b.trace_id


TRAIL_TRACE = {
    "trace_id": "ab" * 16,
    "spans": [{
        "timestamp": "2025-01-01T00:00:00.000000Z", "trace_id": "ab" * 16, "span_id": "0000000000000001",
        "parent_span_id": None, "span_name": "main", "duration": "PT2S", "status_code": "Unset",
        "span_attributes": {},
        "child_spans": [{
            "timestamp": "2025-01-01T00:00:00.100000Z", "trace_id": "ab" * 16, "span_id": "0000000000000002",
            "parent_span_id": "0000000000000001", "span_name": "Agent.run", "duration": "PT1.5S", "status_code": "Ok",
            "span_attributes": {"openinference.span.kind": "AGENT", "input.value": "What is 2 + 2?", "output.value": "5"},
            "child_spans": [{
                "timestamp": "2025-01-01T00:00:00.200000Z", "trace_id": "ab" * 16, "span_id": "0000000000000003",
                "parent_span_id": "0000000000000002", "span_name": "calc", "duration": "PT0.1S", "status_code": "Error",
                "span_attributes": {"openinference.span.kind": "TOOL", "tool.name": "calc", "input.value": "{\"a\": 2}"},
                "child_spans": [],
            }],
        }],
    }],
}


def test_trail_failed_means_a_high_impact_error():
    low = {"errors": [{"category": "Formatting Errors", "location": "0000000000000003", "impact": "LOW"}]}
    high = {"errors": [*low["errors"], {"category": "Tool-related", "location": "0000000000000003", "impact": "high"}]}
    assert not trail.parse("gaia", TRAIL_TRACE, low).failed
    run = trail.parse("gaia", TRAIL_TRACE, high)
    assert run.failed and run.meta["errors"] == 2 and run.meta["high_impact_errors"] == 1


def test_trail_run_trace_makes_the_agent_span_a_root():
    run = trail.parse("gaia", TRAIL_TRACE, {"errors": []})
    req, state = run_trace(run)
    by_id = {s["spanId"]: s for s in spans(req)}
    assert "parentSpanId" not in by_id["0000000000000002"]  # the AGENT span, re-rooted
    assert by_id["0000000000000003"]["parentSpanId"] == "0000000000000002"
    start, end = int(by_id["0000000000000002"]["startTimeUnixNano"]), int(by_id["0000000000000002"]["endTimeUnixNano"])
    assert end - start == 1_500_000_000
    assert "USER QUESTION:\nWhat is 2 + 2?" in state and "FINAL ANSWER:\n5" in state


def test_tau_results_pair_by_position_when_ids_repeat():
    # claude-3.5-sonnet logs give every call the id "0".
    row = {"task_id": 1, "reward": 0.0, "traj": [
        {"role": "user", "content": "hi"},
        {"role": "assistant", "content": None, "tool_calls": [
            {"id": "0", "function": {"name": "find_user", "arguments": "{}"}}]},
        {"role": "tool", "tool_call_id": "0", "name": "find_user", "content": "Error: user not found"},
        {"role": "assistant", "content": None, "tool_calls": [
            {"id": "0", "function": {"name": "find_user_by_zip", "arguments": "{}"}}]},
        {"role": "tool", "tool_call_id": "0", "name": "find_user_by_zip", "content": "mei_8020"},
        {"role": "assistant", "content": "Found you."},
    ]}
    run = tau.parse("sonnet-35-new-retail", row)
    assert [(c.output, c.error) for c in run.tool_calls] == [("Error: user not found", True), ("mei_8020", False)]


def test_tau_results_use_an_unambiguous_id_out_of_order():
    row = {"task_id": 1, "reward": 1.0, "traj": [
        {"role": "assistant", "content": None, "tool_calls": [
            {"id": "a", "function": {"name": "one", "arguments": "{}"}},
            {"id": "b", "function": {"name": "two", "arguments": "{}"}}]},
        {"role": "tool", "tool_call_id": "b", "name": "two", "content": "second"},
        {"role": "tool", "tool_call_id": "a", "name": "one", "content": "first"},
    ]}
    assert [c.output for c in tau.parse("gpt-4o-retail", row).tool_calls] == ["first", "second"]
