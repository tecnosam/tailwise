package pipeline

import "github.com/tecnosam/tailwise/internal/trace"

// Rules checks a trace against heuristic rules: certain, free checks that
// need no model, such as "the final answer is valid JSON".
type Rules interface {
	// Check returns every rule's verdict, in the config's order.
	Check(t trace.Trace) []RuleResult
}

// RuleResult is one rule's verdict on a trace.
type RuleResult struct {
	Name string
	// Keep is the rule's effect when it fires: keep the trace without asking
	// the model. Otherwise the rule is a signal: it counts 1 in severity when
	// it fires and 0 when it does not, and does not decide keep or drop.
	Keep   bool
	Fired  bool
	Reason string // what the trace broke, in plain English; empty if not fired
}

// CauseRule is the cause of a decision a keep rule made.
const CauseRule = "rule"

func fired(results []RuleResult) []RuleResult {
	var out []RuleResult
	for _, r := range results {
		if r.Fired {
			out = append(out, r)
		}
	}
	return out
}

func firstKeep(results []RuleResult) (RuleResult, bool) {
	for _, r := range results {
		if r.Fired && r.Keep {
			return r, true
		}
	}
	return RuleResult{}, false
}

// signals are the signal rules' contributions to severity: 1 fired, 0 not.
func signals(results []RuleResult) []Probability {
	var out []Probability
	for _, r := range results {
		if r.Keep {
			continue
		}
		if r.Fired {
			out = append(out, 1)
		} else {
			out = append(out, 0)
		}
	}
	return out
}
