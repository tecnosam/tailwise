package scorer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tecnosam/tailwise/internal/fault"
)

const questions = `{"answer_wrong":{"type":"noul","instructions":"Is the final answer wrong?"},"answers_question":{"type":"score","instructions":"How good?","criteria":["Wrong","Fully correct"]}}`

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("../../testdata/kev", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// hang blocks a fake-Kev handler until the client gives up. The body must be
// read first: net/http only notices a dropped connection after that.
func hang(r *http.Request) {
	io.Copy(io.Discard, r.Body)
	select {
	case <-r.Context().Done():
	case <-time.After(10 * time.Second):
	}
}

func newClient(t *testing.T, endpoint string, mutate func(*Config)) *Client {
	t.Helper()
	cfg := Config{
		Endpoint:         endpoint,
		Model:            "kev-latest",
		Questions:        json.RawMessage(questions),
		DecisionQuestion: "answer_wrong",
		Timeout:          2 * time.Second,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestScoreSuccess(t *testing.T) {
	var got struct {
		Model     string          `json:"model"`
		State     string          `json:"state"`
		Questions json.RawMessage `json:"questions"`
	}
	var method, path, contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, contentType = r.Method, r.URL.Path, r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(fixture(t, "success.json"))
	}))
	defer srv.Close()

	c := newClient(t, srv.URL+"/v1/systemone", nil)
	res, err := c.Score(context.Background(), "USER QUESTION:\nq")
	if err != nil {
		t.Fatal(err)
	}
	if res.P != 0.93 || res.Model != "kev-latest" || res.Latency != 93*time.Millisecond {
		t.Errorf("result = %+v", res)
	}
	if method != http.MethodPost || path != "/v1/systemone" || contentType != "application/json" {
		t.Errorf("request = %s %s (%s)", method, path, contentType)
	}
	if got.Model != "kev-latest" || got.State != "USER QUESTION:\nq" {
		t.Errorf("request body = %+v", got)
	}
	// Questions go out exactly as configured, in the same key order.
	if string(got.Questions) != questions {
		t.Errorf("questions = %s\nwant %s", got.Questions, questions)
	}
}

func TestScoreJevCompatibleLatency(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(fixture(t, "success_jev.json"))
	}))
	defer srv.Close()

	c := newClient(t, srv.URL, nil)
	res, err := c.Score(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	if res.P != 0.71 || res.Latency != 57250*time.Microsecond || res.Model != "jev-latest" {
		t.Errorf("result = %+v", res)
	}
}

// A hosted scorer such as Jev needs its key on every request, the probe included;
// a local Kev gets no Authorization header at all.
func TestAPIKeyIsSentAsBearerToken(t *testing.T) {
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		w.Write(fixture(t, "success_jev.json"))
	}))
	defer srv.Close()

	c := newClient(t, srv.URL+"/v1/systemone", func(cfg *Config) { cfg.APIKey = "secret" })
	if _, err := c.Score(context.Background(), "s"); err != nil {
		t.Fatal(err)
	}
	if err := c.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	plain := newClient(t, srv.URL+"/v1/systemone", nil)
	if _, err := plain.Score(context.Background(), "s"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"Bearer secret", "Bearer secret", ""}; strings.Join(auth, "|") != strings.Join(want, "|") {
		t.Errorf("Authorization headers = %q, want %q", auth, want)
	}
}

func TestScoreMeasuresLatencyWhenServerReportsNone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"answers":{"answer_wrong":{"type":"noul","noul":0.4}}}`))
	}))
	defer srv.Close()

	c := newClient(t, srv.URL, nil)
	res, err := c.Score(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	if res.Latency <= 0 {
		t.Errorf("latency = %v, want a measured value", res.Latency)
	}
	if res.Model != "kev-latest" {
		t.Errorf("model = %q, want the configured model as fallback", res.Model)
	}
}

func TestScoreTimeout(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		hang(r)
	}))
	defer srv.Close()

	c := newClient(t, srv.URL, func(cfg *Config) { cfg.Timeout = 50 * time.Millisecond })
	_, err := c.Score(context.Background(), "s")
	if !errors.Is(err, context.DeadlineExceeded) || fault.KindOf(err) != fault.KindTimeout {
		t.Fatalf("error = %v, want a timeout fault", err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1: the client does not retry", calls.Load())
	}
}

func TestScore4xxIsRejected(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write(fixture(t, "error_422.json"))
	}))
	defer srv.Close()

	c := newClient(t, srv.URL, nil)
	_, err := c.Score(context.Background(), "s")
	var status *StatusError
	if !errors.As(err, &status) || status.Code != http.StatusUnprocessableEntity {
		t.Fatalf("error = %v, want a 422 StatusError", err)
	}
	if !strings.Contains(status.Body, "Field required") {
		t.Errorf("error body = %q, want the server's message", status.Body)
	}
	if calls.Load() != 1 || fault.KindOf(err) != fault.KindRejected {
		t.Errorf("calls = %d, kind %s; want one call, rejected", calls.Load(), fault.KindOf(err))
	}
}

func TestScoreMalformedResponses(t *testing.T) {
	tests := []struct{ name, body, wantErr string }{
		{"not JSON", `<html>`, "decode model response"},
		{"missing answer", string(fixture(t, "missing_answer.json")), `no answer for "answer_wrong"`},
		{"no noul value", `{"answers":{"answer_wrong":{"type":"score","score":1.2}}}`, "no noul value"},
		{"out of range", `{"answers":{"answer_wrong":{"type":"noul","noul":1.7}}}`, "outside [0, 1]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			c := newClient(t, srv.URL, nil)
			_, err := c.Score(context.Background(), "s")
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
			}
			if calls.Load() != 1 {
				t.Errorf("calls = %d, want 1", calls.Load())
			}
		})
	}
}

func TestScoreStopsWhenCallerCancels(t *testing.T) {
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		cancel()
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newClient(t, srv.URL, nil)
	_, err := c.Score(ctx, "s")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want no retries after cancellation", calls.Load())
	}
}

func TestReady(t *testing.T) {
	var path string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.WriteHeader(status)
	}))
	c := newClient(t, srv.URL+"/v1/systemone", nil)

	if err := c.Ready(context.Background()); err != nil {
		t.Errorf("Ready = %v, want nil", err)
	}
	if path != "/v1/models" {
		t.Errorf("probe path = %q, want /v1/models", path)
	}

	status = http.StatusNotFound // up, just no such route
	if err := c.Ready(context.Background()); err != nil {
		t.Errorf("Ready on 404 = %v, want nil", err)
	}

	status = http.StatusBadGateway
	if err := c.Ready(context.Background()); err == nil {
		t.Error("Ready on 502 = nil, want an error")
	}

	srv.Close()
	if err := c.Ready(context.Background()); err == nil {
		t.Error("Ready with the server down = nil, want an error")
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	base := Config{Endpoint: "http://kev:1/v1/systemone", Questions: json.RawMessage(questions), DecisionQuestion: "answer_wrong"}

	bad := base
	bad.Endpoint = "not a url"
	if _, err := New(bad); err == nil {
		t.Error("expected an error for a bad endpoint")
	}
	bad = base
	bad.Questions = json.RawMessage(`{`)
	if _, err := New(bad); err == nil {
		t.Error("expected an error for invalid questions")
	}
	bad = base
	bad.DecisionQuestion = ""
	if _, err := New(bad); err == nil {
		t.Error("expected an error for a missing decision question")
	}
}

// classify decides between "retry later" and "this request will never work".
func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want fault.Kind
	}{
		{"connection refused", errors.New("dial tcp: connection refused"), fault.KindUnavailable},
		{"timeout", context.DeadlineExceeded, fault.KindTimeout},
		{"500", &StatusError{Code: 500}, fault.KindUnavailable},
		{"503", &StatusError{Code: 503}, fault.KindUnavailable},
		{"404 model or route missing", &StatusError{Code: 404}, fault.KindUnavailable},
		{"429", &StatusError{Code: 429}, fault.KindUnavailable},
		{"401", &StatusError{Code: 401}, fault.KindUnavailable},
		{"malformed response", errors.New("decode model response"), fault.KindUnavailable},
		{"400", &StatusError{Code: 400}, fault.KindRejected},
		{"413", &StatusError{Code: 413}, fault.KindRejected},
		{"422", &StatusError{Code: 422}, fault.KindRejected},
	}
	for _, tt := range tests {
		if got := classify(tt.err); got.Kind != tt.want || got.Component != fault.Scorer {
			t.Errorf("classify(%s) = %s from %s, want %s", tt.name, got.Kind, got.Component, tt.want)
		}
	}
}

// Score returns the classified error.
func TestScoreErrorsAreClassified(t *testing.T) {
	for code, want := range map[int]fault.Kind{500: fault.KindUnavailable, 422: fault.KindRejected} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		c := newClient(t, srv.URL, nil)
		_, err := c.Score(context.Background(), "s")
		srv.Close()
		e, ok := errors.AsType[*fault.Error](err)
		if !ok || e.Kind != want {
			t.Errorf("HTTP %d: error = %v (%+v), want kind %s", code, err, e, want)
		}
	}
}

// A wrong or missing API key makes the model unready: every trace would
// fail, and no retry fixes it.
func TestReadyRejectsARefusedKey(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		c := newClient(t, srv.URL+"/v1/systemone", nil)
		err := c.Ready(context.Background())
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), "refused the API key") {
			t.Errorf("HTTP %d: Ready = %v, want the key refused", code, err)
		}
	}
}

// The reason question's chosen option becomes the cause, and its
// description the plain-English explanation. An option the client never
// offered, or no answer at all, gives none: the score still stands.
func TestScoreReadsTheReason(t *testing.T) {
	reasons := map[string]string{"incomplete": "The final answer leaves out part of what the question asked."}
	for _, tt := range []struct {
		name, answer, cause, explanation string
	}{
		{"chosen", `"why_wrong": {"type": "choice", "choice": "incomplete", "confidence": 0.9}`, "incomplete", reasons["incomplete"]},
		{"not offered", `"why_wrong": {"type": "choice", "choice": "made_up"}`, "", ""},
		{"malformed", `"why_wrong": {"type": "choice", "choice": 7}`, "", ""},
		{"absent", `"other": {"type": "noul", "noul": 0.1}`, "", ""},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"model": "jev-1", "answers": {"answer_wrong": {"type": "noul", "noul": 0.8}, ` + tt.answer + `}}`))
		}))
		c := newClient(t, srv.URL, func(cfg *Config) { cfg.ReasonQuestion, cfg.Reasons = "why_wrong", reasons })
		res, err := c.Score(context.Background(), "s")
		srv.Close()
		if err != nil || res.P != 0.8 || res.Cause != tt.cause || res.Explanation != tt.explanation {
			t.Errorf("%s: score %+v, error %v; want cause %q", tt.name, res, err, tt.cause)
		}
	}
}

// Every yes/no answer is kept. p_keep is the highest answer among the
// questions that decide: the decision question, and any listed in AnyOf.
func TestScoreKeepsTheAnswersAndTakesTheHighest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"answers": {"answer_wrong": {"type": "noul", "noul": 0.2}, "unsupported_answer": {"type": "noul", "noul": 0.8},` +
			` "answers_question": {"type": "score", "score": 1.2}, "bad": {"type": "noul", "noul": 3}}}`))
	}))
	defer srv.Close()

	res, err := newClient(t, srv.URL, nil).Score(context.Background(), "s")
	if err != nil || res.P != 0.2 || len(res.Deciding) != 1 {
		t.Fatalf("decision question alone: %+v, %v", res, err)
	}
	if len(res.Answers) != 2 || res.Answers["answer_wrong"] != 0.2 || res.Answers["unsupported_answer"] != 0.8 {
		t.Errorf("answers = %v, want the two well-formed yes/no answers", res.Answers)
	}

	anyOf := newClient(t, srv.URL, func(cfg *Config) { cfg.AnyOf = []string{"unsupported_answer"} })
	if res, err = anyOf.Score(context.Background(), "s"); err != nil || res.P != 0.8 || len(res.Deciding) != 2 {
		t.Errorf("with any_of: %+v, %v; want p_keep 0.8 from two deciding answers", res, err)
	}

	missing := newClient(t, srv.URL, func(cfg *Config) { cfg.AnyOf = []string{"missing_tool_calls"} })
	if _, err = missing.Score(context.Background(), "s"); err == nil || !strings.Contains(err.Error(), `no answer for "missing_tool_calls"`) {
		t.Errorf("a deciding question without an answer: error = %v", err)
	}
}
