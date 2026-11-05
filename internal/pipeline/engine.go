package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"time"

	"github.com/tecnosam/tailwise/internal/fault"
	"github.com/tecnosam/tailwise/internal/observe"
	"github.com/tecnosam/tailwise/internal/retry"
	"github.com/tecnosam/tailwise/internal/trace"
)

type Deps struct {
	Models   ModelRouter
	Policy   *Policy
	Exporter Exporter
	Reporter observe.Reporter // defaults to observe.Discard
	Metrics  Metrics
	Logger   *slog.Logger // decision log lines; defaults to slog.Default()
}

// Engine runs one trace through route, slice, score, decide and
// export.
type Engine struct {
	deps Deps
}

func NewEngine(deps Deps) *Engine {
	if deps.Reporter == nil {
		deps.Reporter = observe.Discard
	}
	if deps.Metrics == nil {
		deps.Metrics = noMetrics{}
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Engine{deps: deps}
}

// Process always decides: if the model cannot score the trace, the policy's
// fallback does.
func (e *Engine) Process(ctx context.Context, t trace.Trace) Outcome {
	out, _ := e.run(ctx, t, false)
	return out
}

// ProcessQueued returns a system error without deciding, so the queue can
// redeliver the trace. A trace the model rejects still gets the fallback,
// since retrying it cannot help.
func (e *Engine) ProcessQueued(ctx context.Context, t trace.Trace) (Outcome, error) {
	return e.run(ctx, t, true)
}

func (e *Engine) Ready(ctx context.Context) error {
	return e.deps.Models.Ready(ctx)
}

func (e *Engine) run(ctx context.Context, t trace.Trace, deferSystemErrors bool) (Outcome, error) {
	start := time.Now()
	record, err := e.decide(ctx, t, start, deferSystemErrors)
	if err != nil {
		return Outcome{}, err
	}
	out := Outcome{Record: record, Exported: e.deps.Exporter.Export(ctx, record), Duration: time.Since(start)}
	e.deps.Metrics.TraceDecided(ctx, record)
	e.logDecision(out)
	return out, nil
}

func (e *Engine) decide(ctx context.Context, t trace.Trace, start time.Time, deferSystemErrors bool) (Record, error) {
	model := e.deps.Models.Route(t.Agent)
	var verdicts []RuleResult
	if model.Rules != nil {
		verdicts = model.Rules.Check(t)
	}
	// A keep rule is certain, so the model is not asked.
	if r, ok := firstKeep(verdicts); ok {
		return keptByRule(t, model, r, verdicts), nil
	}
	state, trimmed := buildState(t, model)
	score, err := e.score(ctx, t, state, model, deferSystemErrors)
	var findings actionFindings
	if err == nil && model.Actions != nil {
		findings, err = e.checkActions(ctx, t, model, deferSystemErrors)
		findings.apply(&score, hasAction(t, model.Actions))
	}
	e.deps.Metrics.ScoreFinished(ctx, ScoreResult{Agent: model.Agent, Model: model.Name, Duration: time.Since(start), Err: err})
	var rec Record
	switch {
	case err == nil:
		rec = e.scored(t, model, score, findings, verdicts)
	case deferSystemErrors && isSystemError(err):
		return Record{}, err
	default:
		rec = e.fallback(ctx, t, model, err, time.Since(start))
	}
	rec.Trimmed = trimmed
	rec.Rules = fired(verdicts)
	return rec, nil
}

// keptByRule is the decision a keep rule made: kept, first in the review
// order, with the rule's reason.
func keptByRule(t trace.Trace, model Model, r RuleResult, verdicts []RuleResult) Record {
	d := Decision{Keep: true, Rule: RuleKeepRule, Reason: r.Reason, Cause: CauseRule, Severity: 1}
	return Record{Trace: t, Agent: model.Agent, Decision: d, Model: model.Name, Rules: fired(verdicts)}
}

// buildState renders the trace as the model is set to see it, cut to
// trace.MaxStateChars.
func buildState(t trace.Trace, model Model) (trace.State, bool) {
	if !model.SystemPrompt {
		t.SystemPrompt = ""
	}
	build := trace.BuildState
	if model.Evidence == trace.EvidenceGrounded {
		build = func(t trace.Trace) trace.State { return trace.BuildStateGrounded(t, model.EvidenceBudget) }
	}
	return trace.Fit(t, trace.MaxStateChars, build)
}

// On the queued path a system error is returned at once and the queue
// redelivers; the synchronous path has no queue, so it retries here.
func (e *Engine) score(ctx context.Context, t trace.Trace, state trace.State, model Model, queued bool) (Score, error) {
	var score Score
	err := e.withRetry(ctx, t, model, queued, "score trace", func() error {
		var err error
		score, err = model.Scorer.Score(ctx, state)
		return err
	})
	return score, err
}

// withRetry makes a model call, retrying it on the synchronous path.
func (e *Engine) withRetry(ctx context.Context, t trace.Trace, model Model, queued bool, op string, call func() error) error {
	for attempt := 1; ; attempt++ {
		err := call()
		if err == nil || queued || !fault.Retryable(err) || model.Retry.Exhausted(attempt) || ctx.Err() != nil {
			return err
		}
		delay := model.Retry.Delay(attempt)
		e.deps.Reporter.Report(ctx, fault.New(fault.KindOf(err), fault.Pipeline, fault.Op(op+", will retry"), err).
			WithTrace(t.TraceID).With("model", model.Name).With("attempt", attempt).
			With("max_attempts", model.Retry.Attempts).With("retry_in", delay.String()))
		if err := retry.Sleep(ctx, delay); err != nil {
			return err
		}
	}
}

func (e *Engine) reportCheckFailure(ctx context.Context, t trace.Trace, model Model, err error) {
	e.deps.Reporter.Report(ctx, fault.New(fault.KindOf(err), fault.Pipeline, "action check failed, deciding without it", err).
		WithTrace(t.TraceID).With("agent", model.Agent).With("model", model.Name))
}

func hasAction(t trace.Trace, a *Actions) bool {
	for _, c := range t.ToolCalls {
		if a.isAction(c.Name) {
			return true
		}
	}
	return false
}

func (e *Engine) scored(t trace.Trace, model Model, score Score, findings actionFindings, verdicts []RuleResult) Record {
	d, flags := e.deps.Policy.DecideChecks(score.P, model.Checks, score.Deciding, signals(verdicts)...)
	d.Cause, d.Reason = score.Cause, score.Explanation
	// The decision question's reason explains it best; when only an action
	// check kept the trace, say what that check found instead.
	if len(flags) > 0 && len(model.Checks) > 0 && !slices.Contains(flags, model.Checks[0].Question) {
		d.Cause, d.Reason = findings.reason(flags[0])
	}
	if d.Reason == "" {
		d.Reason = probabilityReason(score.P)
	}
	return Record{Trace: t, Agent: model.Agent, Decision: d, Model: score.Model, Latency: score.Latency,
		Answers: score.Answers, Flags: flags, InputTokens: score.InputTokens}
}

// probabilityReason is the reason for a model that gave none of its own.
func probabilityReason(p Probability) string {
	pct := int(math.Round(float64(p) * 100))
	if p >= 0.5 {
		return fmt.Sprintf("The model judged the final answer likely wrong (%d%% likely).", pct)
	}
	return fmt.Sprintf("The model judged the final answer likely right (%d%% likely to be wrong).", pct)
}

func (e *Engine) fallback(ctx context.Context, t trace.Trace, model Model, cause error, elapsed time.Duration) Record {
	e.deps.Reporter.Report(ctx, fault.New(fault.KindOf(cause), fault.Pipeline, "score trace, falling back to a random decision", cause).
		WithTrace(t.TraceID).With("agent", model.Agent).With("model", model.Name))
	d := e.deps.Policy.Fallback()
	d.Reason = "The model could not score this trace, so it was dropped at random."
	if d.Keep {
		d.Reason = "The model could not score this trace, so it was kept at random for review."
	}
	return Record{Trace: t, Agent: model.Agent, Decision: d, Model: model.Name, Latency: elapsed}
}

// Only a request the model refused, or an invalid one, is not the system's
// fault.
func isSystemError(err error) bool {
	switch fault.KindOf(err) {
	case fault.KindRejected, fault.KindInvalid:
		return false
	}
	return true
}

func (e *Engine) logDecision(out Outcome) {
	rec, d := out.Record, out.Record.Decision
	attrs := []any{
		"trace_id", rec.Trace.TraceID,
		"agent", rec.Agent,
		"decision", d.Label(),
		"rule", d.Rule,
		"reason", d.Reason,
		"model", rec.Model,
		"latency_ms", Milliseconds(rec.Latency),
		"duration_ms", Milliseconds(out.Duration),
		"exported", out.Exported,
	}
	if d.Cause != "" {
		attrs = append(attrs, "cause", d.Cause)
	}
	if d.Scored {
		attrs = append(attrs, "p_keep", float64(d.PKeep))
	}
	if d.Ranked() {
		attrs = append(attrs, "severity", d.Severity)
	}
	if len(rec.Rules) > 0 {
		names := make([]string, len(rec.Rules))
		for i, r := range rec.Rules {
			names[i] = r.Name
		}
		attrs = append(attrs, "rules", names)
	}
	if rec.Flags != nil {
		attrs = append(attrs, "flags", rec.Flags)
	}
	if rec.Trimmed {
		attrs = append(attrs, "trimmed", true)
	}
	if len(rec.Answers) > 0 {
		attrs = append(attrs, "answers", rec.Answers)
	}
	e.deps.Logger.Info("decision", attrs...)
}

func (d Decision) Label() string {
	if d.Keep {
		return "keep"
	}
	return "drop"
}

func Milliseconds(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}

type noMetrics struct{}

func (noMetrics) ScoreFinished(context.Context, ScoreResult) {}
func (noMetrics) TraceDecided(context.Context, Record)       {}
