// Package pipeline is the engine's core: the domain types, the interfaces it
// needs from its parts, and the Engine. It imports none of its adapters.
package pipeline

import (
	"time"

	"github.com/tecnosam/tailwise/internal/trace"
)

type Probability float64

type ModelName string

// Rule is which part of the policy decided. Bounded, so safe as a metric
// attribute.
type Rule string

const (
	RuleAboveThreshold Rule = "above_threshold"
	RuleRandomFloor    Rule = "random_floor"
	RuleBelowThreshold Rule = "below_threshold"
	RuleFallback       Rule = "fallback"
)

type Score struct {
	// P is p_keep: the highest answer among the questions that decide, the
	// decision question alone unless the manifest lists more.
	P       Probability
	Model   ModelName
	Latency time.Duration // as the server reported it, else the round trip
	// Cause is the model's choice for the reason question, Explanation its
	// plain-English description; both empty when it gave none.
	Cause       string
	Explanation string
	// Answers holds every yes/no (noul) answer the model gave, by question.
	Answers map[string]Probability
	// Deciding holds the answers of the questions that decide; P is their
	// highest.
	Deciding map[string]Probability
}

type Decision struct {
	Keep bool
	// Scored is false when the model gave no answer, so the probabilities are
	// meaningless.
	Scored bool
	PKeep  Probability
	Rule   Rule
	// Reason is why the model thinks the final answer is wrong, or right, in plain
	// English.
	Reason string
	// Cause is bounded by the manifest, so safe as a metric attribute.
	Cause string
	// Severity is the sum of the flagged checks' weight × answer: higher for
	// more, and higher-priority, problems. 0 when nothing was flagged.
	Severity float64
}

type Record struct {
	Trace    trace.Trace
	Agent    AgentLabel
	Decision Decision
	Model    ModelName
	Latency  time.Duration
	// Answers are the model's yes/no answers, none on fallback.
	Answers map[string]Probability
	// Flags names the checks whose answer reached their threshold, in
	// priority order: the problems the model found.
	Flags []string
}

type Outcome struct {
	Record   Record
	Exported bool
	Duration time.Duration
}
