package trace

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFitLeavesShortStatesAlone(t *testing.T) {
	tr := Trace{Question: "q", FinalAnswer: "a"}
	state, trimmed := Fit(tr, MaxStateChars, BuildState)
	if trimmed || state != BuildState(tr) {
		t.Errorf("trimmed = %v, state =\n%s", trimmed, state)
	}
}

// A giant patch as the final answer is cut in the middle, keeping its start
// and end, and the trace itself is untouched.
func TestFitCutsTheLongestField(t *testing.T) {
	patch := "diff --git a/x.py b/x.py\n" + strings.Repeat("+line\n", 50000) + "END OF PATCH"
	tr := Trace{Question: "Fix the bug.", ToolCalls: []ToolCall{call("ls", `{"path":"."}`, "x.py")}, FinalAnswer: patch}
	state, trimmed := Fit(tr, 10000, BuildState)
	if !trimmed || len(state) > 10000 {
		t.Fatalf("trimmed = %v, %d characters, want at most 10000", trimmed, len(state))
	}
	for _, want := range []string{"USER QUESTION:\nFix the bug.", "1. ls({\"path\":\".\"}) -> ok",
		"FINAL ANSWER:\ndiff --git a/x.py", cutMark, "END OF PATCH"} {
		if !strings.Contains(string(state), want) {
			t.Errorf("state lost %q", want)
		}
	}
	if tr.FinalAnswer != patch {
		t.Error("Fit changed the caller's trace")
	}
}

func TestFitCutsArgumentsToAJSONString(t *testing.T) {
	big, _ := json.Marshal(map[string]string{"content": strings.Repeat("é", 20000)})
	tr := Trace{Question: "q", ToolCalls: []ToolCall{{Name: "write", Args: big}}, FinalAnswer: "a"}
	state, trimmed := Fit(tr, 5000, func(t Trace) State { return BuildStateGrounded(t, 12000) })
	if !trimmed || len(state) > 5000 || !utf8.ValidString(string(state)) {
		t.Fatalf("trimmed = %v, %d characters, valid UTF-8 %v", trimmed, len(state), utf8.ValidString(string(state)))
	}
	if !strings.Contains(string(state), `1. write("{\"content\":\"éé`) {
		t.Errorf("arguments not cut to a JSON string:\n%.200s", state)
	}
}

// BuildState does not show tool results, so cutting the longest one does
// nothing; Fit moves on to the final answer.
func TestFitSkipsFieldsTheStateDoesNotShow(t *testing.T) {
	tr := Trace{Question: "q", ToolCalls: []ToolCall{call("cat", `{}`, strings.Repeat("x", 90000))},
		FinalAnswer: strings.Repeat("y", 30000)}
	state, trimmed := Fit(tr, 10000, BuildState)
	if !trimmed || len(state) > 10000 {
		t.Errorf("trimmed = %v, %d characters, want at most 10000", trimmed, len(state))
	}
}

// When every field is already short, Fit stops rather than cut them away.
func TestFitGivesUpOnManyShortFields(t *testing.T) {
	var calls []ToolCall
	for range 500 {
		calls = append(calls, call("step", `{"n":1}`, ""))
	}
	tr := Trace{Question: "q", ToolCalls: calls, FinalAnswer: "a"}
	state, _ := Fit(tr, 1000, BuildState)
	if !strings.Contains(string(state), "500. step") {
		t.Error("Fit dropped tool calls")
	}
}
