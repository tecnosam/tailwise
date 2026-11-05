package rules

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tecnosam/tailwise/internal/trace"
)

func run1(t *testing.T, c Config, tr trace.Trace) (bool, string) {
	t.Helper()
	s, err := New([]Config{c})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r := s.Check(tr)[0]
	return r.Fired, r.Reason
}

var order = trace.Trace{
	Question:    "Where is order #7?",
	FinalAnswer: `{"order": 7, "status": "shipped"}`,
	ToolCalls: []trace.ToolCall{
		{Name: "get_order", Args: json.RawMessage(`{"id": 7}`), Output: `{"id": 7, "status": "shipped"}`},
		{Name: "search", Args: json.RawMessage(`{"q": "x"}`), Status: json.RawMessage(`"error"`), Output: "Error: timeout"},
		{Name: "search", Args: json.RawMessage(`{"q": "x"}`)},
		{Name: "search", Args: json.RawMessage(`{"q": "x"}`)},
	},
}

func TestRules(t *testing.T) {
	answerSchema := map[string]any{"type": "object", "required": []any{"order", "status"},
		"properties": map[string]any{"status": map[string]any{"enum": []any{"shipped", "pending"}}}}
	tests := []struct {
		name   string
		rule   Config
		fires  bool
		reason string // a part of it
	}{
		{"answer is JSON", Config{FinalAnswer: &Text{JSON: true}}, false, ""},
		{"answer matches its schema", Config{FinalAnswer: &Text{JSONSchema: answerSchema}}, false, ""},
		{"answer too short", Config{FinalAnswer: &Text{MinLength: 100}}, true, "under the minimum of 100"},
		{"answer too long", Config{FinalAnswer: &Text{MaxLength: 10}}, true, "over the maximum of 10"},
		{"answer must match", Config{FinalAnswer: &Text{Matches: `#\d+`}}, true, `does not match "#\\d+"`},
		{"answer must not match", Config{FinalAnswer: &Text{NotMatches: `shipped`}}, true, `matches "shipped"`},
		{"tool failed", Config{ToolOutput: &ToolOutput{OKStatus: true}}, true, "Call 2, to search, failed with status error"},
		{"other tool's status ignored", Config{ToolOutput: &ToolOutput{Tool: "get_order", OKStatus: true}}, false, ""},
		{"tool output schema", Config{ToolOutput: &ToolOutput{Tool: "get_order",
			Text: Text{JSONSchema: map[string]any{"required": []any{"id", "eta"}}}}}, true, "The output of call 1, to get_order, does not match the schema"},
		{"tool args schema", Config{ToolArgs: &ToolArgs{Tool: "get_order",
			JSONSchema: map[string]any{"properties": map[string]any{"id": map[string]any{"type": "string"}}}}}, true, "The arguments of call 1, to get_order"},
		{"loop", Config{Run: &Run{MaxRepeatedCalls: 2}}, true, "called search more than 2 times"},
		{"no loop at the limit", Config{Run: &Run{MaxRepeatedCalls: 3}}, false, ""},
		{"too many calls", Config{Run: &Run{MaxToolCalls: 3}}, true, "4 tool calls, over the limit of 3"},
		{"required tool", Config{Run: &Run{RequiresTools: []string{"refund"}}}, true, "never called refund"},
		{"forbidden tool", Config{Run: &Run{ForbidsTools: []string{"search"}}}, true, "called search, which it must not"},
		{"own reason", Config{Reason: "No refund.", Run: &Run{RequiresTools: []string{"refund"}}}, true, "No refund."},
	}
	for _, tt := range tests {
		tt.rule.Name = "r"
		fired, reason := run1(t, tt.rule, order)
		if fired != tt.fires || !strings.Contains(reason, tt.reason) {
			t.Errorf("%s: fired %v, reason %q; want %v, %q", tt.name, fired, reason, tt.fires, tt.reason)
		}
	}
}

func TestEmptyAndInvalidAnswer(t *testing.T) {
	empty := trace.Trace{Question: "q", FinalAnswer: "  "}
	if fired, reason := run1(t, Config{Name: "r", FinalAnswer: &Text{NotEmpty: true}}, empty); !fired || reason != "The final answer is empty." {
		t.Errorf("empty: %v %q", fired, reason)
	}
	prose := trace.Trace{Question: "q", FinalAnswer: "it shipped"}
	if fired, reason := run1(t, Config{Name: "r", FinalAnswer: &Text{JSONSchema: map[string]any{"type": "object"}}}, prose); !fired || !strings.Contains(reason, "not valid JSON") {
		t.Errorf("schema on prose: %v %q", fired, reason)
	}
}

func TestNewReportsEveryProblem(t *testing.T) {
	_, err := New([]Config{
		{FinalAnswer: &Text{JSON: true}},
		{Name: "a", OnFail: "maybe", FinalAnswer: &Text{JSON: true}},
		{Name: "b"},
		{Name: "c", FinalAnswer: &Text{}},
		{Name: "d", FinalAnswer: &Text{Matches: "("}},
		{Name: "e", ToolArgs: &ToolArgs{}},
		{Name: "f", FinalAnswer: &Text{JSONSchema: map[string]any{"type": 5}}},
		{Name: "a", Run: &Run{MaxToolCalls: 1}},
	})
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{"rules[0]: name is required", "on_fail is \"maybe\"", "(b): set exactly one",
		"(c): final_answer: set at least one", "(d): final_answer: invalid regular expression", "(e): tool_args: json_schema is required",
		"(f): final_answer: json_schema", "rules[7] (a): name is used"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
}

func TestMerge(t *testing.T) {
	off := false
	defaults := []Config{{Name: "json", OnFail: Keep}, {Name: "loop"}, {Name: "length"}}
	own := []Config{{Name: "loop", OnFail: Keep}, {Name: "length", Enabled: &off}, {Name: "schema"}}
	got := Merge(defaults, own)
	var names []string
	for _, c := range got {
		names = append(names, c.Name+":"+string(c.OnFail))
	}
	if strings.Join(names, ",") != "json:keep,loop:keep,schema:" {
		t.Errorf("merged %v", names)
	}
}

func TestNoRulesIsNil(t *testing.T) {
	if s, err := New(nil); s != nil || err != nil {
		t.Errorf("New(nil) = %v, %v", s, err)
	}
}
