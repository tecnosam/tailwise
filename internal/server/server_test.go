package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/tecnosam/tailwise/internal/broker"
	"github.com/tecnosam/tailwise/internal/common"
	"github.com/tecnosam/tailwise/internal/export"
	exportnats "github.com/tecnosam/tailwise/internal/export/natsq"
	"github.com/tecnosam/tailwise/internal/export/otlpbackend"
	"github.com/tecnosam/tailwise/internal/fault"
	"github.com/tecnosam/tailwise/internal/pipeline"
	"github.com/tecnosam/tailwise/internal/retry"
	"github.com/tecnosam/tailwise/internal/scorer"
	"github.com/tecnosam/tailwise/internal/trace"
)

const (
	testdata      = "../../testdata"
	testQuestions = `{"answer_wrong":{"type":"noul","instructions":"Is the final answer wrong or only partially correct for the user's question?"}}`
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func jsonLogger(w io.Writer) *slog.Logger { return slog.New(slog.NewJSONHandler(w, nil)) }

func readFile(t *testing.T, parts ...string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{testdata}, parts...)...))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fakeScorer is an in-process Scorer for handler tests.
type fakeScorer struct {
	score func(ctx context.Context, state trace.State) (pipeline.Score, error)
	ready error
}

func (f *fakeScorer) Score(ctx context.Context, state trace.State) (pipeline.Score, error) {
	return f.score(ctx, state)
}

func (f *fakeScorer) Ready(context.Context) error { return f.ready }

func fixedScore(p float64) func(context.Context, trace.State) (pipeline.Score, error) {
	return func(context.Context, trace.State) (pipeline.Score, error) {
		return pipeline.Score{P: pipeline.Probability(p), Model: "kev-latest", Latency: 12 * time.Millisecond}, nil
	}
}

// fakeQueue is an in-memory Publisher that records what it was given.
type fakeQueue struct {
	mu         sync.Mutex
	published  []trace.Trace
	publishErr error
	readyErr   error
}

func (q *fakeQueue) Publish(_ context.Context, t trace.Trace) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.publishErr != nil {
		return q.publishErr
	}
	q.published = append(q.published, t)
	return nil
}

func (q *fakeQueue) Ready() error { return q.readyErr }

type harness struct {
	srv    *Server
	engine *pipeline.Engine
	queue  *fakeQueue
	// recorder receives every exported trace, unless harnessOpts.uploader is set.
	recorder *capture
}

type harnessOpts struct {
	scorer  pipeline.Scorer
	queue   pipeline.Publisher // defaults to a fakeQueue
	rand    func() float64
	maxBody int64
	logger  *slog.Logger
	// uploader, when set, receives every exported trace instead of the recorder.
	uploader otlpbackend.Uploader
	// retries is how many times the synchronous path retries a failed call.
	retries int
	// models, when set, replaces the single model built from scorer.
	models  pipeline.ModelRouter
	metrics common.RequestMetrics
}

func newHarness(t *testing.T, o harnessOpts) *harness {
	t.Helper()
	if o.rand == nil {
		o.rand = func() float64 { return 0.99 } // never take the random floor
	}
	if o.logger == nil {
		o.logger = quiet
	}
	fq := &fakeQueue{}
	if o.queue == nil {
		o.queue = fq
	}
	rec := &capture{}
	var uploader otlpbackend.Uploader = rec
	if o.uploader != nil {
		uploader = o.uploader
	}
	if o.models == nil {
		o.models = pipeline.SingleModel(pipeline.Model{Name: "kev-latest", Scorer: o.scorer,
			Retry: retry.Policy{Attempts: o.retries + 1, Backoff: []time.Duration{time.Millisecond}}})
	}
	engine := pipeline.NewEngine(pipeline.Deps{
		Models:   o.models,
		Policy:   pipeline.NewPolicy(pipeline.PolicyConfig{KeepThreshold: 0.5, RandomFloor: 0.02}, o.rand),
		Exporter: newExporter(t, uploader),
		Logger:   o.logger,
	})
	srv := New(Options{Engine: engine, Queue: o.queue, MaxBodyBytes: o.maxBody, Metrics: o.metrics})
	return &harness{srv: srv, engine: engine, queue: fq, recorder: rec}
}

// newExporter runs a back end sending to u behind the same asynchronous
// Dispatcher the engine uses, its queue on an embedded NATS server.
func newExporter(t *testing.T, u otlpbackend.Uploader) *export.Dispatcher {
	t.Helper()
	b, err := broker.New(broker.Config{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	backend := otlpbackend.New(u, map[string]string{"openinference.project.name": "tailwise"})
	d, err := export.NewDispatcher(context.Background(), export.DispatcherConfig{
		Backends:   []export.BackendConfig{{Name: "test"}},
		NewBackend: fixedFactory{backend}.Build,
		NewQueue: b.ExportQueues(exportnats.Streams{
			Stream: "EXPORTS", Subject: "exports", Consumer: "workers",
			DLQStream: "EXPORTS_DLQ", DLQSubject: "exports_dlq", DLQMaxAge: time.Hour,
			PollWait: 100 * time.Millisecond,
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Shutdown(context.Background()) })
	return d
}

// fixedFactory hands out one prebuilt back end.
type fixedFactory struct{ backend export.AbstractBackend }

func (f fixedFactory) Build(context.Context, export.BackendConfig) (export.AbstractBackend, error) {
	return f.backend, nil
}

func (h *harness) do(method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	return w
}

func decodeBody[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("response %q is not JSON: %v", w.Body.String(), err)
	}
	return v
}

// attrValue reads an exported span attribute.
type attrValue struct{ v *commonpb.AnyValue }

func (a attrValue) AsString() string   { return a.v.GetStringValue() }
func (a attrValue) AsFloat64() float64 { return a.v.GetDoubleValue() }
func (a attrValue) String() string     { return fmt.Sprint(a.v.GetValue()) }

func rootAttrs(t *testing.T, rec *capture) map[string]attrValue {
	t.Helper()
	for _, s := range rec.Ended() {
		if s.Name == "agent_run" {
			m := map[string]attrValue{}
			for _, kv := range s.Attributes {
				m[kv.Key] = attrValue{kv.Value}
			}
			return m
		}
	}
	t.Fatal("no agent_run span exported")
	return nil
}

// Ended is every span exported so far.
func (c *capture) Ended() []*tracepb.Span {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*tracepb.Span
	for _, upload := range c.uploads {
		for _, rs := range upload {
			for _, ss := range rs.ScopeSpans {
				out = append(out, ss.Spans...)
			}
		}
	}
	return out
}

func TestEnqueuePublishesToQueue(t *testing.T) {
	h := newHarness(t, harnessOpts{scorer: &fakeScorer{score: fixedScore(0.9)}})

	body := string(readFile(t, "traces", "correct_river.json"))
	w := h.do("POST", "/v1/traces", body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body %s", w.Code, w.Body)
	}
	if got := decodeBody[map[string]string](t, w); got["trace_id"] != "7f3a91c2e8" {
		t.Errorf("trace_id = %q, want 7f3a91c2e8", got["trace_id"])
	}

	// No trace_id: the server generates 32 hex chars and queues the trace
	// under that ID.
	w = h.do("POST", "/v1/traces", `{"question": "q2", "final_answer": "a2"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	generated := decodeBody[map[string]string](t, w)["trace_id"]
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(generated) {
		t.Errorf("generated trace_id = %q, want 32 hex chars", generated)
	}

	if len(h.queue.published) != 2 {
		t.Fatalf("%d traces published, want 2", len(h.queue.published))
	}
	first, second := h.queue.published[0], h.queue.published[1]
	if first.TraceID != "7f3a91c2e8" || first.FinalAnswer != "The Seine" || len(first.ToolCalls) != 1 || len(first.Evidence) != 2 {
		t.Errorf("published trace = %+v", first)
	}
	if second.TraceID != generated || second.Question != "q2" {
		t.Errorf("published trace = %+v, want the generated ID", second)
	}
	// Scoring happens in the workers, not in the request.
	if n := len(h.recorder.Ended()); n != 0 {
		t.Errorf("%d spans started by the enqueue handler, want 0", n)
	}
}

func TestBadRequests(t *testing.T) {
	h := newHarness(t, harnessOpts{scorer: &fakeScorer{score: fixedScore(0.9)}})
	tests := []struct{ name, body, wantErr string }{
		{"not JSON", `{"question": `, "invalid JSON"},
		{"empty body", ``, "invalid JSON"},
		{"missing question", `{"final_answer": "a"}`, "question is required"},
		{"missing final_answer", `{"question": "q"}`, "final_answer is required"},
		{"wrong type", `{"question": ["q"], "final_answer": "a"}`, "invalid JSON"},
		{"tool call without name", `{"question": "q", "final_answer": "a", "tool_calls": [{}]}`, "tool_calls[0].name is required"},
	}
	for _, path := range []string{"/v1/traces", "/v1/traces:score"} {
		for _, tt := range tests {
			t.Run(path+"/"+tt.name, func(t *testing.T) {
				w := h.do("POST", path, tt.body)
				if w.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400; body %s", w.Code, w.Body)
				}
				if got := decodeBody[map[string]string](t, w)["error"]; !strings.Contains(got, tt.wantErr) {
					t.Errorf("error = %q, want it to contain %q", got, tt.wantErr)
				}
			})
		}
	}
	if n := len(h.recorder.Ended()); n != 0 {
		t.Errorf("%d spans started for rejected requests", n)
	}
	if n := len(h.queue.published); n != 0 {
		t.Errorf("%d rejected requests reached the queue", n)
	}
}

func TestBodyTooLarge(t *testing.T) {
	h := newHarness(t, harnessOpts{scorer: &fakeScorer{score: fixedScore(0.9)}, maxBody: 64})
	body := `{"question": "` + strings.Repeat("x", 200) + `", "final_answer": "a"}`
	if w := h.do("POST", "/v1/traces", body); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", w.Code)
	}
}

func TestMethodAndRouteMatching(t *testing.T) {
	h := newHarness(t, harnessOpts{scorer: &fakeScorer{score: fixedScore(0.9)}})
	if w := h.do("GET", "/v1/traces", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /v1/traces = %d, want 405", w.Code)
	}
	if w := h.do("POST", "/v1/nope", "{}"); w.Code != http.StatusNotFound {
		t.Errorf("POST /v1/nope = %d, want 404", w.Code)
	}
}

func TestEnqueueWhenQueueRefuses(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		wantErr string
	}{
		{"full", fault.Full(fault.Queue, "publish trace", nil), "queue full"},
		{"wrapped full", fmt.Errorf("publish: %w", fault.Full(fault.Queue, "publish trace", nil)), "queue full"},
		{"unavailable", fault.Unavailable(fault.Queue, "publish trace", errors.New("nats: timeout")), "queue unavailable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, harnessOpts{scorer: &fakeScorer{score: fixedScore(0.9)}})
			h.queue.publishErr = tt.err

			w := h.do("POST", "/v1/traces", `{"question": "q", "final_answer": "a"}`)
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", w.Code)
			}
			if got := w.Header().Get("Retry-After"); got != "1" {
				t.Errorf("Retry-After = %q, want \"1\"", got)
			}
			if got := decodeBody[map[string]string](t, w)["error"]; !strings.Contains(got, tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", got, tt.wantErr)
			}
			// NATS internals stay in the log, not in the response.
			if strings.Contains(w.Body.String(), "nats:") {
				t.Errorf("response leaks the queue error: %s", w.Body)
			}
		})
	}
}

// The sync path does not touch the queue, so it keeps working without it.
func TestScoreWorksWhenQueueIsDown(t *testing.T) {
	h := newHarness(t, harnessOpts{scorer: &fakeScorer{score: fixedScore(0.9)}})
	h.queue.publishErr = fault.Unavailable(fault.Queue, "publish trace", nil)
	h.queue.readyErr = errors.New("nats is reconnecting")

	w := h.do("POST", "/v1/traces:score", `{"question": "q", "final_answer": "a"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := decodeBody[Result](t, w); got.Decision != "keep" {
		t.Errorf("result = %+v", got)
	}
}

func TestHealthz(t *testing.T) {
	// Up even when Kev is down.
	h := newHarness(t, harnessOpts{scorer: &fakeScorer{ready: errors.New("down")}})
	w := h.do("GET", "/healthz", "")
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if got := decodeBody[map[string]string](t, w)["status"]; got != "ok" {
		t.Errorf("status field = %q", got)
	}
}

func TestReadyz(t *testing.T) {
	fs := &fakeScorer{}
	h := newHarness(t, harnessOpts{scorer: fs})
	if w := h.do("GET", "/readyz", ""); w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 when Kev answers", w.Code)
	}

	fs.ready = errors.New("connection refused")
	w := h.do("GET", "/readyz", "")
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when Kev is down", w.Code)
	}
	if got := decodeBody[map[string]string](t, w)["error"]; !strings.Contains(got, "model is not reachable: model kev-latest (default): connection refused") {
		t.Errorf("error = %q", got)
	}

	// Kev back, queue gone.
	fs.ready = nil
	h.queue.readyErr = errors.New("nats is reconnecting")
	w = h.do("GET", "/readyz", "")
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when the queue is down", w.Code)
	}
	if got := decodeBody[map[string]string](t, w)["error"]; !strings.Contains(got, "queue is not reachable: nats is reconnecting") {
		t.Errorf("error = %q", got)
	}
}

// fakeKev is an httptest Kev that records the states it was asked about.
type fakeKev struct {
	*httptest.Server
	mu     sync.Mutex
	states []string
	calls  atomic.Int32
}

func newFakeKev(t *testing.T, handler func(w http.ResponseWriter, state string)) *fakeKev {
	t.Helper()
	k := &fakeKev{}
	k.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Write([]byte(`{"models":[]}`))
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/systemone" {
			http.NotFound(w, r)
			return
		}
		k.calls.Add(1)
		var req struct {
			Model     string          `json:"model"`
			State     string          `json:"state"`
			Questions json.RawMessage `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.State == "" || len(req.Questions) == 0 {
			http.Error(w, `{"detail":"bad request"}`, http.StatusUnprocessableEntity)
			return
		}
		k.mu.Lock()
		k.states = append(k.states, req.State)
		k.mu.Unlock()
		handler(w, req.State)
	}))
	t.Cleanup(k.Close)
	return k
}

func (k *fakeKev) client(t *testing.T) *scorer.Client {
	t.Helper()
	c, err := scorer.New(scorer.Config{
		Endpoint:         k.URL + "/v1/systemone",
		Model:            "kev-latest",
		Questions:        json.RawMessage(testQuestions),
		DecisionQuestion: "answer_wrong",
		Timeout:          2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// End to end through the sync path: real handler, real Kev client against a
// fake Kev, real exporter into a span recorder.
func TestScoreEndToEndKeep(t *testing.T) {
	kev := newFakeKev(t, func(w http.ResponseWriter, _ string) {
		w.Write(readFile(t, "kev", "success.json")) // noul 0.93, latency_ms 93
	})
	h := newHarness(t, harnessOpts{scorer: kev.client(t), retries: 3})

	w := h.do("POST", "/v1/traces:score", string(readFile(t, "traces", "wrong_river.json")))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", w.Code, w.Body)
	}

	// Every documented field is present in the JSON.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"trace_id", "decision", "p_keep", "reason", "model", "latency_ms", "exported"} {
		if _, ok := fields[f]; !ok {
			t.Errorf("response is missing %q: %s", f, w.Body)
		}
	}
	for _, f := range []string{"p_keep_raw", "severity"} {
		if _, ok := fields[f]; ok {
			t.Errorf("response has the removed field %q: %s", f, w.Body)
		}
	}

	got := decodeBody[Result](t, w)
	wantP := 0.93
	if got.TraceID != "wrong-river-001" || got.Decision != "keep" || got.Rule != "above_threshold" ||
		got.Model != "kev-latest" || got.LatencyMS != 93 || !got.Exported {
		t.Errorf("result = %+v", got)
	}
	if got.PKeep == nil || *got.PKeep != wantP {
		t.Errorf("p_keep = %v, want %v", deref(got.PKeep), wantP)
	}

	// Kev was sent exactly the golden slice.
	wantState := string(readFile(t, "golden", "wrong_river.txt"))
	if len(kev.states) != 1 || kev.states[0] != wantState {
		t.Errorf("state sent to Kev:\n%q\nwant:\n%q", kev.states, wantState)
	}

	// The exporter sent the root span and one span per tool call, right
	// after the response.
	waitUntil(t, "the kept trace to be exported", func() bool { return len(h.recorder.Ended()) == 3 })
	a := rootAttrs(t, h.recorder)
	if a["eval.decision"].AsString() != "keep" || a["eval.rule"].AsString() != "above_threshold" ||
		a["eval.model"].AsString() != "kev-latest" {
		t.Errorf("eval attributes = %v", a)
	}
	if a["eval.p_keep"].AsFloat64() != wantP || a["eval.latency_ms"].AsFloat64() != 93 {
		t.Errorf("eval numbers: p_keep=%v latency=%v", a["eval.p_keep"], a["eval.latency_ms"])
	}
	if a["tailwise.source_trace_id"].AsString() != "wrong-river-001" {
		t.Errorf("tailwise.source_trace_id = %v", a["tailwise.source_trace_id"])
	}
}

func TestScoreEndToEndDrop(t *testing.T) {
	kev := newFakeKev(t, func(w http.ResponseWriter, _ string) {
		w.Write(readFile(t, "kev", "success_low.json")) // noul 0.12
	})
	h := newHarness(t, harnessOpts{scorer: kev.client(t), retries: 3})

	w := h.do("POST", "/v1/traces:score", string(readFile(t, "traces", "correct_river.json")))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	got := decodeBody[Result](t, w)
	if got.Decision != "drop" || got.Rule != "below_threshold" || got.Exported {
		t.Errorf("result = %+v", got)
	}
	if got.PKeep == nil || *got.PKeep != 0.12 || got.LatencyMS != 41.5 {
		t.Errorf("p_keep = %v, latency = %v", deref(got.PKeep), got.LatencyMS)
	}
	if n := len(h.recorder.Ended()); n != 0 {
		t.Errorf("%d spans started for a dropped trace, want 0", n)
	}
}

func TestScoreEndToEndRandomFloorKeep(t *testing.T) {
	kev := newFakeKev(t, func(w http.ResponseWriter, _ string) {
		w.Write(readFile(t, "kev", "success_low.json"))
	})
	h := newHarness(t, harnessOpts{scorer: kev.client(t), retries: 3, rand: func() float64 { return 0.001 }})

	got := decodeBody[Result](t, h.do("POST", "/v1/traces:score", `{"question": "q", "final_answer": "a"}`))
	if got.Decision != "keep" || got.Rule != "random_floor" || !got.Exported {
		t.Errorf("result = %+v", got)
	}
}

// Kev failing on every attempt must not fail the request: the decision
// falls back to the random floor with reason "fallback".
func TestScoreEndToEndFallback(t *testing.T) {
	kev := newFakeKev(t, func(w http.ResponseWriter, _ string) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	body := `{"trace_id": "t1", "question": "q", "final_answer": "a"}`

	t.Run("drop", func(t *testing.T) {
		kev.calls.Store(0)
		h := newHarness(t, harnessOpts{scorer: kev.client(t), retries: 2, rand: func() float64 { return 0.5 }})
		w := h.do("POST", "/v1/traces:score", body)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		got := decodeBody[Result](t, w)
		if got.Decision != "drop" || got.Rule != "fallback" || got.Exported {
			t.Errorf("result = %+v", got)
		}
		if got.PKeep != nil {
			t.Errorf("p_keep = %v, want null for a fallback", deref(got.PKeep))
		}
		if !strings.Contains(w.Body.String(), `"p_keep":null`) {
			t.Errorf("body = %s, want explicit nulls", w.Body)
		}
		if got.Model != "kev-latest" {
			t.Errorf("model = %q, want the configured model", got.Model)
		}
		if kev.calls.Load() != 3 {
			t.Errorf("Kev called %d times, want 3 (1 + 2 retries)", kev.calls.Load())
		}
		if n := len(h.recorder.Ended()); n != 0 {
			t.Errorf("%d spans started for a dropped fallback", n)
		}
	})

	t.Run("keep", func(t *testing.T) {
		h := newHarness(t, harnessOpts{scorer: kev.client(t), retries: 2, rand: func() float64 { return 0.001 }})
		got := decodeBody[Result](t, h.do("POST", "/v1/traces:score", body))
		if got.Decision != "keep" || got.Rule != "fallback" || !got.Exported {
			t.Errorf("result = %+v", got)
		}
		waitUntil(t, "the kept trace to be exported", func() bool { return len(h.recorder.Ended()) == 1 })
		a := rootAttrs(t, h.recorder)
		if a["eval.rule"].AsString() != "fallback" {
			t.Errorf("eval.reason = %v", a["eval.reason"])
		}
		if _, ok := a["eval.p_keep"]; ok {
			t.Error("eval.p_keep set on a fallback keep")
		}
	})
}

func TestReadyzAgainstFakeKev(t *testing.T) {
	kev := newFakeKev(t, func(http.ResponseWriter, string) {})
	h := newHarness(t, harnessOpts{scorer: kev.client(t)})
	if w := h.do("GET", "/readyz", ""); w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	kev.Close()
	if w := h.do("GET", "/readyz", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 with Kev down", w.Code)
	}
}

// stuckUploader never answers until released, like a hung back end.
type stuckUploader struct{ release chan struct{} }

func (u stuckUploader) UploadTraces(ctx context.Context, _ []*tracepb.ResourceSpans) error {
	select {
	case <-u.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// The synchronous endpoint answers once the trace is decided and queued for
// export. A hung back end, or a long export queue, must not hold it up.
func TestScoreDoesNotWaitForTheBackEnd(t *testing.T) {
	stuck := stuckUploader{release: make(chan struct{})}
	defer close(stuck.release)
	h := newHarness(t, harnessOpts{scorer: &fakeScorer{score: fixedScore(0.9)}, uploader: stuck})

	for i := range 5 {
		start := time.Now()
		got := decodeBody[Result](t, h.do("POST", "/v1/traces:score", `{"question": "q", "final_answer": "a"}`))
		if took := time.Since(start); took > time.Second {
			t.Fatalf("request %d took %s with the back end stuck", i, took)
		}
		if got.Decision != "keep" || !got.Exported {
			t.Errorf("result = %+v, want keep, queued for export", got)
		}
	}
}

// One JSON log line per decision with the fields operators filter on.
func TestDecisionLogLine(t *testing.T) {
	var buf strings.Builder
	answered := func(context.Context, trace.State) (pipeline.Score, error) {
		return pipeline.Score{P: 0.9, Model: "kev-latest", Latency: 12 * time.Millisecond,
			Answers: map[string]pipeline.Probability{"answer_wrong": 0.95, "unsupported_answer": 0.7}}, nil
	}
	h := newHarness(t, harnessOpts{
		scorer: &fakeScorer{score: answered},
		logger: jsonLogger(&buf),
	})
	h.do("POST", "/v1/traces:score", `{"trace_id": "log-1", "question": "q", "final_answer": "a"}`)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d log lines, want 1:\n%s", len(lines), buf.String())
	}
	var line map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &line); err != nil {
		t.Fatalf("log line is not JSON: %v", err)
	}
	want := map[string]any{"msg": "decision", "trace_id": "log-1", "decision": "keep", "p_keep": 0.9, "rule": "above_threshold", "latency_ms": 12.0}
	for k, v := range want {
		if line[k] != v {
			t.Errorf("log field %s = %v, want %v", k, line[k], v)
		}
	}
	answers, _ := line["answers"].(map[string]any)
	if answers["answer_wrong"] != 0.95 || answers["unsupported_answer"] != 0.7 {
		t.Errorf("log field answers = %v, want every yes/no answer", line["answers"])
	}
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}
