package pipeline

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/tecnosam/tailwise/internal/fault"
	"github.com/tecnosam/tailwise/internal/trace"
)

// fixedRules returns the same verdicts for every trace.
type fixedRules []RuleResult

func (f fixedRules) Check(trace.Trace) []RuleResult { return f }

func ruleEngine(s Scorer, r Rules) *Engine {
	m := Model{Name: "m", Scorer: s, Rules: r, Checks: []Check{{Question: "answer_wrong"}}}
	return NewEngine(Deps{Models: SingleModel(m), Policy: keepAtHalf(), Exporter: nowhere{}, Logger: quiet})
}

// A keep rule keeps the trace without asking the model, first in the review
// order.
func TestKeepRuleSkipsTheModel(t *testing.T) {
	s := &stubScorer{raw: 0.1}
	rules := fixedRules{{Name: "signal", Fired: true, Reason: "s"}, {Name: "json", Keep: true, Fired: true, Reason: "The final answer is not valid JSON."}}
	rec := ruleEngine(s, rules).Process(context.Background(), sampleTrace("")).Record
	d := rec.Decision
	if s.calls != 0 {
		t.Errorf("the model was asked %d times", s.calls)
	}
	if !d.Keep || d.Rule != RuleKeepRule || d.Scored || d.Severity != 1 || !d.Ranked() ||
		d.Cause != CauseRule || d.Reason != "The final answer is not valid JSON." {
		t.Errorf("decision = %+v", d)
	}
	if len(rec.Rules) != 2 {
		t.Errorf("rules = %+v, want both fired", rec.Rules)
	}
}

// A signal rule counts 1 in severity when it fires, 0 when not, and decides
// nothing.
func TestSignalRulesJoinSeverity(t *testing.T) {
	rules := fixedRules{{Name: "loop", Fired: true, Reason: "looped"}, {Name: "long"}, {Name: "json", Keep: true}}
	rec := ruleEngine(&askScorer{decide: 0.2}, rules).Process(context.Background(), sampleTrace("")).Record
	d := rec.Decision
	// answer_wrong 0.2, loop 1, long 0; the keep rule did not fire and is not counted.
	if d.Keep || d.Rule != RuleBelowThreshold || math.Abs(d.Severity-0.4) > 1e-9 {
		t.Errorf("decision = %+v", d)
	}
	if len(rec.Rules) != 1 || rec.Rules[0].Name != "loop" {
		t.Errorf("rules = %+v, want loop only", rec.Rules)
	}
}

// When the model cannot score, the fired rules are still recorded.
func TestRulesRecordedOnFallback(t *testing.T) {
	s := &stubScorer{err: fault.Unavailable(fault.Scorer, "score trace", errors.New("down"))}
	rec := ruleEngine(s, fixedRules{{Name: "loop", Fired: true, Reason: "looped"}}).Process(context.Background(), sampleTrace("")).Record
	if rec.Decision.Rule != RuleFallback || rec.Decision.Ranked() || len(rec.Rules) != 1 {
		t.Errorf("record = %+v", rec)
	}
}

func TestDecideWithSignalsAndNoChecks(t *testing.T) {
	d, _ := keepAtHalf().DecideChecks(0.6, nil, nil, 1, 0)
	if !d.Keep || math.Abs(d.Severity-(0.6+1)/3) > 1e-9 {
		t.Errorf("decision = %+v", d)
	}
}
