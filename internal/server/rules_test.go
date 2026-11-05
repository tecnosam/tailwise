package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/tecnosam/tailwise/internal/pipeline"
	"github.com/tecnosam/tailwise/internal/retry"
	"github.com/tecnosam/tailwise/internal/rules"
)

// A keep rule decides without Kev: the response names the rule, has no
// p_keep, ranks the trace first, and the export carries the rule.
func TestScoreKeepRule(t *testing.T) {
	kev := newFakeKev(t, func(w http.ResponseWriter, _ string) {
		w.Write(readFile(t, "kev", "success_low.json"))
	})
	set, err := rules.New([]rules.Config{
		{Name: "answer_json", OnFail: rules.Keep, FinalAnswer: &rules.Text{JSON: true}},
		{Name: "short", FinalAnswer: &rules.Text{MinLength: 50}},
	})
	if err != nil {
		t.Fatal(err)
	}
	models := pipeline.SingleModel(pipeline.Model{Name: "kev-latest", Scorer: kev.client(t), Rules: set,
		Retry: retry.Policy{Attempts: 1, Backoff: []time.Duration{time.Millisecond}}})
	h := newHarness(t, harnessOpts{models: models})

	got := decodeBody[Result](t, h.do("POST", "/v1/traces:score", `{"question": "q", "final_answer": "not json"}`))
	if got.Decision != "keep" || got.Rule != "keep_rule" || got.Cause != "rule" ||
		got.Reason != "The final answer is not valid JSON." || got.PKeep != nil || got.Severity == nil || *got.Severity != 1 {
		t.Errorf("result = %+v", got)
	}
	if len(got.Rules) != 2 || got.Rules[0] != (RuleHit{Name: "answer_json", OnFail: "keep", Reason: "The final answer is not valid JSON."}) ||
		got.Rules[1].Name != "short" || got.Rules[1].OnFail != "signal" {
		t.Errorf("rules = %+v", got.Rules)
	}
	if len(kev.states) != 0 {
		t.Errorf("Kev was asked %d times", len(kev.states))
	}
	waitUntil(t, "the kept trace to be exported", func() bool { return len(h.recorder.Ended()) > 0 })
	a := rootAttrs(t, h.recorder)
	if a["eval.rules"].AsString() != "answer_json,short" || a["eval.rules.answer_json"].AsString() != "The final answer is not valid JSON." ||
		a["eval.severity"].AsFloat64() != 1 {
		t.Errorf("eval attributes = %v", a)
	}
	if _, ok := a["eval.p_keep"]; ok {
		t.Error("eval.p_keep exported for a trace the model did not score")
	}
}
