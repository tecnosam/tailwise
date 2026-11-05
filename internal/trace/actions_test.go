package trace

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func isWrite(name string) bool { return name == "exchange" || name == "cancel" }

// A retail-like run: the agent looks up an order and a product, exchanges an
// item for a variant, then cancels another order.
func exchangeRun() Trace {
	return Trace{
		Question: "Exchange my keyboard in order #W100 for the size L one. Also cancel order #W200.",
		ToolCalls: []ToolCall{
			call("get_order", `{"order_id":"#W100"}`, `{"order_id":"#W100","items":[{"item_id":"1111"}]}`),
			call("get_product", `{"product_id":"77"}`,
				`{"variants":{"2222":{"size":"M"},"3333":{"size":"L"}},"name":"keyboard"}`),
			call("get_user", `{"user_id":"yusuf_1"}`, `{"name":"Yusuf"}`),
			call("exchange", `{"order_id":"#W100","item_ids":["1111"],"new_item_ids":["2222"]}`, `{"status":"ok"}`),
			{Name: "cancel", Args: json.RawMessage(`{"order_id":"#W200"}`), Status: json.RawMessage(`"error"`),
				Output: "Error: order #W200 is already delivered"},
		},
		FinalAnswer: "Done: exchanged your keyboard. I could not cancel #W200: it was delivered.",
	}
}

func TestActionStateShowsTheLookupsTheActionRestsOn(t *testing.T) {
	got := string(BuildActionState(exchangeRun(), 3, isWrite))
	want := "USER REQUEST:\nExchange my keyboard in order #W100 for the size L one. Also cancel order #W200.\n\n" +
		"LOOKUP RESULTS THE ACTION'S VALUES CAME FROM:\n" +
		`[1] get_order({"order_id":"#W100"}) -> ok: {"order_id":"#W100","items":[{"item_id":"1111"}]}` + "\n" +
		`[2] get_product({"product_id":"77"}) -> ok: {"variants":{"2222":{"size":"M"},"3333":{"size":"L"}},"name":"keyboard"}` + "\n\n" +
		"ACTION:\n" + `exchange({"order_id":"#W100","item_ids":["1111"],"new_item_ids":["2222"]}) -> ok` + "\n\n" +
		"LATER ACTIONS IN THIS RUN:\n" + `- cancel({"order_id":"#W200"}) -> error`
	if got != want {
		t.Errorf("state =\n%s\nwant\n%s", got, want)
	}
}

// A long lookup is cut to the objects holding the action's values.
func TestActionStateTrimsLongLookups(t *testing.T) {
	tr := exchangeRun()
	var other strings.Builder
	for i := range 300 {
		fmt.Fprintf(&other, `"9%03d":{"item_id":"9%03d","size":"XL"},`, i, i)
	}
	tr.ToolCalls[1].Output = `{"variants":{"2222":{"item_id":"2222","size":"M"},` + other.String() + `"3333":{"item_id":"3333","size":"L"}}}`
	got := string(BuildActionState(tr, 3, isWrite))
	if !strings.Contains(got, `"item_id":"2222"`) || strings.Contains(got, "9001") || len(got) > 2*MaxLookupChars {
		t.Errorf("lookup not trimmed to the used variant (%d characters):\n%.600s", len(got), got)
	}
}

func TestCompletenessStateListsActionsOnly(t *testing.T) {
	got := string(BuildCompletenessState(exchangeRun(), isWrite))
	for _, want := range []string{HandledNote, "USER REQUEST:\nExchange my keyboard",
		"AGENT ACTIONS:\n1. exchange(" + `{"order_id":"#W100","item_ids":["1111"],"new_item_ids":["2222"]}` + ") -> ok\n" +
			`2. cancel({"order_id":"#W200"}) -> error: Error: order #W200 is already delivered`,
		"FINAL ANSWER:\nDone: exchanged your keyboard."} {
		if !strings.Contains(got, want) {
			t.Errorf("state lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "get_order") {
		t.Errorf("lookups shown:\n%s", got)
	}
	if got := string(BuildCompletenessState(Trace{Question: "q", FinalAnswer: "a"}, isWrite)); !strings.Contains(got, "AGENT ACTIONS:\n(none)") {
		t.Errorf("no actions:\n%s", got)
	}
}

// With a conversation, the completeness check sees what the agent said: here,
// that the user withdrew a request.
func TestCompletenessStateShowsTheConversation(t *testing.T) {
	tr := exchangeRun()
	tr.Conversation = []Message{
		{Role: "user", Content: "Change my order to the 10x zoom camera."},
		{Role: "assistant", Content: "That variant costs $3,066. Shall I proceed?" + strings.Repeat(" More details.", 100)},
		{Role: "user", Content: "Too expensive. Cancel the order instead."},
	}
	got := string(BuildCompletenessState(tr, isWrite))
	if !strings.Contains(got, "CONVERSATION:\nUSER: Change my order to the 10x zoom camera.\n\nAGENT: That variant costs") ||
		!strings.Contains(got, "[...]\n\nUSER: Too expensive.") || strings.Contains(got, "USER REQUEST:") {
		t.Errorf("state:\n%s", got)
	}
}
