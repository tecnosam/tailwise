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
}

func resultOf(out pipeline.Outcome) Result {
	rec, d := out.Record, out.Record.Decision
	res := Result{
		TraceID:   rec.Trace.TraceID,
		Agent:     string(rec.Trace.Agent),
		Decision:  d.Label(),
		Rule:      string(d.Rule),
		Reason:    d.Reason,
		Cause:     d.Cause,
		Model:     string(rec.Model),
		LatencyMS: pipeline.Milliseconds(rec.Latency),
		Exported:  out.Exported,
	}
	if d.Scored {
		pKeep := float64(d.PKeep)
		res.PKeep = &pKeep
	}
	return res
}
