package pipeline

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tecnosam/tailwise/internal/fault"
	"github.com/tecnosam/tailwise/internal/retry"
	"github.com/tecnosam/tailwise/internal/trace"
)

// stubScorer answers every trace with raw, or fails with err.
type stubScorer struct {
	raw      Probability
	err      error
	readyErr error
	mu       sync.Mutex
	calls    int
}

func (s *stubScorer) Score(context.Context, trace.State) (Score, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	if s.err != nil {
		return Score{}, s.err
	}
	return Score{P: s.raw, Model: "reported"}, nil
}

func (s *stubScorer) Ready(context.Context) error { return s.readyErr }

// keepAtHalf keeps at 0.5 and never samples at random.
func keepAtHalf() *Policy {
	return NewPolicy(PolicyConfig{KeepThreshold: 0.5}, func() float64 { return 0.99 })
}

type nowhere struct{}

func (nowhere) Export(context.Context, Record) bool { return true }

type recordingMetrics struct {
	mu      sync.Mutex
	scores  []ScoreResult
	records []Record
}

func (m *recordingMetrics) ScoreFinished(_ context.Context, r ScoreResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scores = append(m.scores, r)
}

func (m *recordingMetrics) TraceDecided(_ context.Context, rec Record) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records = append(m.records, rec)
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func sampleTrace(agent trace.AgentName) trace.Trace {
	return trace.Trace{TraceID: "t1", Agent: agent, Question: "q", FinalAnswer: "a"}
}

func TestAgentRouterRoutes(t *testing.T) {
	def := Model{Name: "default"}
	support := Model{Name: "support"}
	r := NewAgentRouter(def, map[trace.AgentName]Model{"support-bot": support})
	for agent, want := range map[trace.AgentName]ModelName{"support-bot": "support", "other": "default", "": "default"} {
		if got := r.Route(agent).Name; got != want {
			t.Errorf("Route(%q) = %s, want %s", agent, got, want)
		}
	}
}

// Readiness follows the default model only: one agent's model being down
// must not take the whole engine out of a load balancer.
func TestAgentRouterReadyChecksTheDefaultOnly(t *testing.T) {
	agentDown := NewAgentRouter(Model{Name: "default", Scorer: &stubScorer{}},
		map[trace.AgentName]Model{"support-bot": {Name: "support", Scorer: &stubScorer{readyErr: errors.New("refused")}}})
	if err := agentDown.Ready(context.Background()); err != nil {
		t.Errorf("an agent's model down: Ready = %v, want nil", err)
	}

	defaultDown := NewAgentRouter(Model{Name: "default", Scorer: &stubScorer{readyErr: errors.New("refused")}}, nil)
	err := defaultDown.Ready(context.Background())
	if err == nil || !strings.Contains(err.Error(), "model default (default): refused") {
		t.Errorf("default down: Ready = %v", err)
	}
}

// Each trace is scored by its agent's model.
func TestEngineScoresWithTheAgentsModel(t *testing.T) {
	def := &stubScorer{raw: 0.6}
	support := &stubScorer{raw: 0.6}
	m := &recordingMetrics{}
	e := NewEngine(Deps{
		Models: NewAgentRouter(Model{Name: "default", Scorer: def},
			map[trace.AgentName]Model{"support-bot": {Name: "support", Scorer: support}}),
		Policy: keepAtHalf(), Exporter: nowhere{}, Metrics: m, Logger: quiet,
	})

	out := e.Process(context.Background(), sampleTrace("support-bot"))
	if support.calls != 1 || def.calls != 0 {
		t.Fatalf("calls: support %d, default %d", support.calls, def.calls)
	}
	if out.Record.Decision.PKeep != 0.6 {
		t.Errorf("p_keep = %v, want the model's 0.6", out.Record.Decision.PKeep)
	}

	out = e.Process(context.Background(), sampleTrace("unknown"))
	if def.calls != 1 || out.Record.Decision.PKeep != 0.6 {
		t.Errorf("unknown agent: default calls %d, p_keep %v", def.calls, out.Record.Decision.PKeep)
	}

	if len(m.scores) != 2 || m.scores[0].Agent != "support-bot" || m.scores[0].Model != "support" || m.scores[0].Err != nil {
		t.Errorf("score metrics = %+v", m.scores)
	}
	if len(m.records) != 2 || m.records[0].Trace.Agent != "support-bot" {
		t.Errorf("decision metrics = %+v", m.records)
	}
}

// A fallback is labelled with the model that failed, and the failure is
// recorded with its error.
func TestEngineFallbackNamesTheAgentsModel(t *testing.T) {
	down := &stubScorer{err: fault.Unavailable(fault.Scorer, "score trace", errors.New("refused"))}
	m := &recordingMetrics{}
	e := NewEngine(Deps{
		Models: NewAgentRouter(Model{Name: "default", Scorer: &stubScorer{}},
			map[trace.AgentName]Model{"support-bot": {Name: "support", Scorer: down}}),
		Policy: keepAtHalf(), Exporter: nowhere{}, Metrics: m, Logger: quiet,
	})
	out := e.Process(context.Background(), sampleTrace("support-bot"))
	if out.Record.Model != "support" || out.Record.Decision.Rule != RuleFallback {
		t.Errorf("record = %+v", out.Record)
	}
	if len(m.scores) != 1 || fault.KindOf(m.scores[0].Err) != fault.KindUnavailable {
		t.Errorf("score metrics = %+v", m.scores)
	}
}

func TestAgentRouterLabels(t *testing.T) {
	r := NewAgentRouter(Model{Name: "default"}, map[trace.AgentName]Model{"support-bot": {Name: "support"}})
	for agent, want := range map[trace.AgentName]AgentLabel{"support-bot": "support-bot", "made-up-123": AgentOther, "": AgentNone} {
		if got := r.Route(agent).Agent; got != want {
			t.Errorf("Route(%q).Agent = %q, want %q", agent, got, want)
		}
	}
}

// However many agent names clients invent, metrics and logs see only the
// configured agents, "other" and "none".
func TestEngineBoundsTheAgentLabel(t *testing.T) {
	m := &recordingMetrics{}
	e := NewEngine(Deps{
		Models: NewAgentRouter(Model{Name: "default", Scorer: &stubScorer{raw: 0.1}}, nil),
		Policy: keepAtHalf(), Exporter: nowhere{}, Metrics: m, Logger: quiet,
	})
	for _, agent := range []trace.AgentName{"a1", "a2", "a3", ""} {
		e.Process(context.Background(), sampleTrace(agent))
	}
	labels := map[AgentLabel]int{}
	for _, s := range m.scores {
		labels[s.Agent]++
	}
	for _, r := range m.records {
		if r.Agent != AgentOther && r.Agent != AgentNone {
			t.Errorf("record label %q", r.Agent)
		}
	}
	if len(labels) != 2 || labels[AgentOther] != 3 || labels[AgentNone] != 1 {
		t.Errorf("score labels = %v, want 3 other and 1 none", labels)
	}
	if m.records[0].Trace.Agent != "a1" {
		t.Error("the trace lost its own agent name")
	}
}

// flakyScorer fails its first fails calls with err, then answers raw.
type flakyScorer struct {
	stubScorer
	fails int
}

func (s *flakyScorer) Score(context.Context, trace.State) (Score, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.calls <= s.fails {
		return Score{}, s.err
	}
	return Score{P: s.raw, Model: "reported"}, nil
}

func retryingEngine(s Scorer, attempts int) *Engine {
	return NewEngine(Deps{
		Models: NewAgentRouter(Model{Name: "m", Scorer: s,
			Retry: retry.Policy{Attempts: attempts, Backoff: []time.Duration{time.Millisecond}}}, nil),
		Policy: keepAtHalf(), Exporter: nowhere{}, Logger: quiet,
	})
}

// The synchronous path has no queue behind it, so it retries a system
// error itself, up to the model's retry policy.
func TestProcessRetriesSystemErrors(t *testing.T) {
	down := fault.Unavailable(fault.Scorer, "score trace", errors.New("503"))
	s := &flakyScorer{stubScorer: stubScorer{raw: 0.9, err: down}, fails: 2}
	out := retryingEngine(s, 4).Process(context.Background(), sampleTrace(""))
	if s.calls != 3 || !out.Record.Decision.Scored {
		t.Errorf("calls %d, decision %+v; want two failures then a scored decision", s.calls, out.Record.Decision)
	}

	s = &flakyScorer{stubScorer: stubScorer{raw: 0.9, err: down}, fails: 10}
	out = retryingEngine(s, 3).Process(context.Background(), sampleTrace(""))
	if s.calls != 3 || out.Record.Decision.Rule != RuleFallback {
		t.Errorf("calls %d, reason %s; want 3 attempts then the fallback", s.calls, out.Record.Decision.Reason)
	}
}

// A rejected request is not retried: asking again gets the same answer.
func TestProcessDoesNotRetryRejections(t *testing.T) {
	s := &stubScorer{err: fault.Rejected(fault.Scorer, "score trace", errors.New("422"))}
	retryingEngine(s, 4).Process(context.Background(), sampleTrace(""))
	if s.calls != 1 {
		t.Errorf("calls = %d, want 1", s.calls)
	}
}

// The queued path calls the model once per delivery and hands the error back,
// so the queue's redelivery is the only retry and the two never multiply.
func TestProcessQueuedCallsOnce(t *testing.T) {
	s := &stubScorer{err: fault.Unavailable(fault.Scorer, "score trace", errors.New("503"))}
	_, err := retryingEngine(s, 4).ProcessQueued(context.Background(), sampleTrace(""))
	if s.calls != 1 || fault.KindOf(err) != fault.KindUnavailable {
		t.Errorf("calls %d, error %v; want one call and the error back", s.calls, err)
	}
}

// The decision carries the model's reason in plain English, and its cause;
// without one, a sentence from the probability; on fallback, what happened.
func TestDecisionReason(t *testing.T) {
	explained := scorerFunc(func() (Score, error) {
		return Score{P: 0.9, Cause: "incomplete", Explanation: "The final answer leaves out part of what the question asked."}, nil
	})
	d := retryingEngine(explained, 1).Process(context.Background(), sampleTrace("")).Record.Decision
	if d.Rule != RuleAboveThreshold || d.Cause != "incomplete" || d.Reason != "The final answer leaves out part of what the question asked." {
		t.Errorf("explained: %+v", d)
	}

	bare := scorerFunc(func() (Score, error) { return Score{P: 0.2}, nil })
	d = retryingEngine(bare, 1).Process(context.Background(), sampleTrace("")).Record.Decision
	if d.Cause != "" || d.Reason != "The model judged the final answer likely right (20% likely to be wrong)." {
		t.Errorf("no explanation: %+v", d)
	}

	down := &stubScorer{err: fault.Rejected(fault.Scorer, "score trace", errors.New("422"))}
	d = retryingEngine(down, 1).Process(context.Background(), sampleTrace("")).Record.Decision
	if d.Rule != RuleFallback || !strings.Contains(d.Reason, "could not score this trace") {
		t.Errorf("fallback: %+v", d)
	}
}

// scorerFunc is a Scorer from a function.
type scorerFunc func() (Score, error)

func (f scorerFunc) Score(context.Context, trace.State) (Score, error) { return f() }
func (scorerFunc) Ready(context.Context) error                         { return nil }
