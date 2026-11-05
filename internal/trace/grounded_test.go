package trace

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func call(name, args, output string) ToolCall {
	return ToolCall{Name: name, Args: json.RawMessage(args), Output: output}
}

// A trace with no tool outputs renders exactly as BuildState does.
func TestGroundedWithoutOutputsMatchesBuildState(t *testing.T) {
	tr := Trace{
		Question:    "Which river flows through Paris?",
		ToolCalls:   []ToolCall{call("search", `{"q":"river Paris"}`, "")},
		Evidence:    []string{"The Seine flows through Paris."},
		FinalAnswer: "The Seine",
	}
	if got, want := BuildStateGrounded(tr, 12000), BuildState(tr); got != want {
		t.Errorf("grounded:\n%s\nwant:\n%s", got, want)
	}
}

func TestGroundedShowsResultsOnTheirLines(t *testing.T) {
	tr := Trace{
		Question: "Cancel order #W1.",
		ToolCalls: []ToolCall{
			call("get_order", `{"order_id":"#W1"}`, `{"status":"pending"}`),
			{Name: "cancel", Args: json.RawMessage(`{"order_id":"#W1"}`), Status: json.RawMessage(`"error"`),
				Output: "Error: line one\nline two"},
		},
		FinalAnswer: "Cancelled.",
	}
	got := string(BuildStateGrounded(tr, 12000))
	if !strings.Contains(got, "1. get_order({\"order_id\":\"#W1\"}) -> ok: {\"status\":\"pending\"}\n") {
		t.Errorf("short result not inline:\n%s", got)
	}
	if !strings.Contains(got, "2. cancel({\"order_id\":\"#W1\"}) -> error:\n   Error: line one\n   line two\n") {
		t.Errorf("multi-line result not indented below its call:\n%s", got)
	}
}

// With a small budget, the result a later call's arguments came from is shown
// before more recent ones that nothing used.
func TestGroundedPrefersResultsLaterCallsUse(t *testing.T) {
	filler := strings.Repeat("x", 150)
	tr := Trace{
		Question: "Exchange my keyboard.",
		ToolCalls: []ToolCall{
			call("get_product", `{"id":"1656367028"}`, `{"variants":{"9690244451":{"item_id":"9690244451","size":"L"}}}`),
			call("get_user", `{"id":"u1"}`, `{"note":"`+filler+`"}`),
			call("get_policy", `{}`, `{"note":"`+filler+`"}`),
			call("exchange", `{"new_item_ids":["9690244451"]}`, `{"ok":true}`),
		},
		FinalAnswer: "Done.",
	}
	got := string(BuildStateGrounded(tr, 200))
	if !strings.Contains(got, `1. get_product({"id":"1656367028"}) -> ok: {"variants":{"9690244451"`) {
		t.Errorf("the grounding result is not shown:\n%s", got)
	}
	if strings.Contains(got, filler) {
		t.Errorf("an unused result was shown before the budget ran out:\n%s", got)
	}
}

// A long JSON result keeps the object holding the value a later call used,
// and the top-level fields that say what it is.
func TestGroundedTrimsLongResultsToWhatWasUsed(t *testing.T) {
	variants := map[string]any{}
	for i := range 60 {
		id := 9000000000 + i
		variants[strconv.Itoa(id)] = map[string]any{"item_id": strconv.Itoa(id), "options": map[string]string{"size": "S"}}
	}
	variants["9690244451"] = map[string]any{"item_id": "9690244451", "options": map[string]string{"size": "L"}}
	out, _ := json.Marshal(map[string]any{"name": "Keyboard", "product_id": "1656367028", "variants": variants})
	if len(out) <= MaxResultChars {
		t.Fatalf("fixture result is only %d bytes", len(out))
	}
	tr := Trace{
		Question: "Exchange for size L.",
		ToolCalls: []ToolCall{
			call("get_product", `{"id":"1656367028"}`, string(out)),
			call("exchange", `{"new_item_ids":["9690244451"]}`, `{"ok":true}`),
		},
		FinalAnswer: "Done.",
	}
	got := string(BuildStateGrounded(tr, 12000))
	want := `{"name":"Keyboard","product_id":"1656367028","variants":{"9690244451":{"item_id":"9690244451","options":{"size":"L"}}}} [trimmed]`
	if !strings.Contains(got, want) {
		t.Errorf("trimmed result missing %s in:\n%s", want, got)
	}
}

func TestGroundedStaysWithinBudget(t *testing.T) {
	var calls []ToolCall
	for i := range 30 {
		calls = append(calls, call("get", `{"i":`+strconv.Itoa(i)+`}`, strings.Repeat("y", 900)))
	}
	tr := Trace{Question: "q", ToolCalls: calls, FinalAnswer: "a"}
	plain := len(BuildState(tr))
	for _, budget := range []int{500, 5000} {
		extra := len(BuildStateGrounded(tr, budget)) - plain
		// Each shown result adds ": " (or ":\n" and an indent) to its line.
		if extra > budget+len(calls)*(2+len(resultIndent)) {
			t.Errorf("budget %d: results added %d characters", budget, extra)
		}
	}
}

// Evidence taken from tool results is not repeated; other evidence stays.
func TestGroundedDoesNotRepeatToolEvidence(t *testing.T) {
	tr := Trace{
		Question:    "q",
		ToolCalls:   []ToolCall{call("search", `{}`, "Paris is in France.")},
		Evidence:    []string{"Paris is in France.", "A retrieved document."},
		FinalAnswer: "a",
	}
	got := string(BuildStateGrounded(tr, 12000))
	if !strings.Contains(got, "EVIDENCE:\n[1] A retrieved document.\n") || strings.Count(got, "Paris is in France.") != 1 {
		t.Errorf("evidence section:\n%s", got)
	}
}
