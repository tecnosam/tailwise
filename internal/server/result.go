package server

import "github.com/tecnosam/tailwise/internal/pipeline"

// Result is the POST /v1/traces:score response. p_keep is null on fallback.
type Result struct {
	TraceID   string   `json:"trace_id"`
	Agent     string   `json:"agent"`
	Decision  string   `json:"decision"`
	PKeep     *float64 `json:"p_keep"`
	Rule      string   `json:"rule"`
	Reason    string   `json:"reason"`
	Cause     string   `json:"cause,omitempty"`
	Model     string   `json:"model"`
	LatencyMS float64  `json:"latency_ms"`
	// Exported means queued for export to at least one back end.
	Exported bool `json:"exported"`
	// Answers, Flags and Severity are as in the decision log; all are absent
	// on fallback.
	Answers  map[string]float64 `json:"answers,omitempty"`
	Flags    []string           `json:"flags,omitempty"`
	Severity float64            `json:"severity,omitempty"`
	// InputTokens is the model's billed input for this trace, absent if it
	// did not report it.
	InputTokens int `json:"input_tokens,omitempty"`
	// Trimmed means the model saw the trace with the middle of its longest
	// fields cut, to fit its input limit.
	Trimmed bool `json:"trimmed,omitempty"`
}

func resultOf(out pipeline.Outcome) Result {
	rec, d := out.Record, out.Record.Decision
	res := Result{
		TraceID:     rec.Trace.TraceID,
		Agent:       string(rec.Trace.Agent),
		Decision:    d.Label(),
		Rule:        string(d.Rule),
		Reason:      d.Reason,
		Cause:       d.Cause,
		Model:       string(rec.Model),
		LatencyMS:   pipeline.Milliseconds(rec.Latency),
		Exported:    out.Exported,
		Flags:       rec.Flags,
		Severity:    d.Severity,
		InputTokens: rec.InputTokens,
		Trimmed:     rec.Trimmed,
	}
	if len(rec.Answers) > 0 {
		res.Answers = make(map[string]float64, len(rec.Answers))
		for q, p := range rec.Answers {
			res.Answers[q] = float64(p)
		}
	}
	if d.Scored {
		pKeep := float64(d.PKeep)
		res.PKeep = &pKeep
	}
	return res
}
