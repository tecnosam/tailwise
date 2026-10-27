package otlpbackend

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/tecnosam/tailwise/internal/export"
	"github.com/tecnosam/tailwise/internal/fault"
	"github.com/tecnosam/tailwise/internal/pipeline"
	"github.com/tecnosam/tailwise/internal/trace"
)

func sampleTrace() trace.Trace {
	return trace.Trace{
		TraceID:  "7f3a91c2e8",
		Question: "Which river flows through the capital of France?",
		ToolCalls: []trace.ToolCall{
			{Name: "search", Args: json.RawMessage(`{"query": "capital of France"}`), Status: json.RawMessage(`"ok"`)},
			{Name: "lookup", Args: json.RawMessage(`"Seine"`), Status: json.RawMessage(`"error"`)},
			{Name: "noop"},
		},
		Evidence:    []string{"Paris is the capital of France.", "The Seine flows through Paris."},
		FinalAnswer: "The Seine",
	}
}

// spansOf flattens one upload.
func spansOf(upload []*tracepb.ResourceSpans) []*tracepb.Span {
	var out []*tracepb.Span
	for _, rs := range upload {
		for _, ss := range rs.ScopeSpans {
			out = append(out, ss.Spans...)
		}
	}
	return out
}

func TestExportKeptTrace(t *testing.T) {
	up := &fakeUploader{}
	exp := New(up, map[string]string{"openinference.project.name": "tailwise"})
	before := time.Now()
	err := exp.Send(context.Background(), pipeline.Record{
		Trace:    sampleTrace(),
		Decision: keepHigh,
		Model:    "kev-latest",
		Latency:  93 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(up.uploads) != 1 {
		t.Fatalf("%d uploads, want 1: the trace is sent before Send returns", len(up.uploads))
	}

	res := attrsOf(up.uploads[0][0].Resource.Attributes)
	if res["service.name"].GetStringValue() != "tailwise" || res["openinference.project.name"].GetStringValue() != "tailwise" {
		t.Errorf("resource = %v, want service.name and the project", res)
	}

	spans := spansOf(up.uploads[0])
	if len(spans) != 4 {
		t.Fatalf("got %d spans, want 4 (root + 3 tool calls)", len(spans))
	}
	root := spans[0]
	if root.Name != "agent_run" || len(root.ParentSpanId) != 0 {
		t.Fatalf("first span = %q with parent %x, want the agent_run root", root.Name, root.ParentSpanId)
	}
	if start := time.Unix(0, int64(root.StartTimeUnixNano)); start.Before(before) || time.Since(start) > time.Minute {
		t.Errorf("root starts at %s, want now", start)
	}

	a := attrsOf(root.Attributes)
	wantStrings := map[string]string{
		"openinference.span.kind":  "AGENT",
		"input.value":              "Which river flows through the capital of France?",
		"output.value":             "The Seine",
		"tailwise.source_trace_id": "7f3a91c2e8",
		"tailwise.evidence.0":      "Paris is the capital of France.",
		"tailwise.evidence.1":      "The Seine flows through Paris.",
		"eval.decision":            "keep",
		"eval.rule":                "above_threshold",

		"eval.model": "kev-latest",
	}
	for k, want := range wantStrings {
		if got := a[k].GetStringValue(); got != want {
			t.Errorf("root %s = %q, want %q", k, got, want)
		}
	}
	for k, want := range map[string]float64{"eval.p_keep": 0.95, "eval.latency_ms": 93} {
		if got := a[k].GetDoubleValue(); got != want {
			t.Errorf("root %s = %v, want %v", k, got, want)
		}
	}
	if _, ok := a["tailwise.evidence.2"]; ok {
		t.Error("unexpected tailwise.evidence.2")
	}

	wantTools := []struct{ name, input, output string }{
		{"search", `{"query":"capital of France"}`, "ok"},
		{"lookup", `"Seine"`, "error"},
		{"noop", "", "ok"},
	}
	for i, want := range wantTools {
		span := spans[i+1]
		if span.Name != want.name {
			t.Errorf("tool span %d name = %q, want %q", i, span.Name, want.name)
		}
		if string(span.ParentSpanId) != string(root.SpanId) || string(span.TraceId) != string(root.TraceId) {
			t.Errorf("tool span %q is not a child of the root", want.name)
		}
		ta := attrsOf(span.Attributes)
		for k, v := range map[string]string{"openinference.span.kind": "TOOL", "tool.name": want.name, "output.value": want.output} {
			if got := ta[k].GetStringValue(); got != v {
				t.Errorf("tool span %q %s = %q, want %q", want.name, k, got, v)
			}
		}
		if got, ok := ta["input.value"]; ok != (want.input != "") || got.GetStringValue() != want.input {
			t.Errorf("tool span %q input.value = %v, want %q", want.name, got, want.input)
		}
		if _, ok := ta["eval.decision"]; ok {
			t.Errorf("tool span %q got eval attributes", want.name)
		}
	}
}

func TestExportFallbackKeepOmitsProbabilities(t *testing.T) {
	up := &fakeUploader{}
	exp := New(up, nil)
	tr := trace.Trace{TraceID: "abc", Question: "q", FinalAnswer: "a"}
	if err := exp.Send(context.Background(), pipeline.Record{
		Trace:    tr,
		Decision: pipeline.Decision{Keep: true, Rule: pipeline.RuleFallback},
		Model:    "kev-latest",
	}); err != nil {
		t.Fatal(err)
	}
	spans := spansOf(up.uploads[0])
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want just the root", len(spans))
	}
	a := attrsOf(spans[0].Attributes)
	if a["eval.rule"].GetStringValue() != "fallback" {
		t.Errorf("eval attributes = %v", a)
	}
	for _, key := range []string{"eval.p_keep", "tailwise.evidence.0"} {
		if _, ok := a[key]; ok {
			t.Errorf("attribute %s present, want it omitted", key)
		}
	}
}

func TestEachKeptTraceIsItsOwnTrace(t *testing.T) {
	up := &fakeUploader{}
	exp := New(up, nil)
	tr := trace.Trace{TraceID: "a", Question: "q", FinalAnswer: "a"}
	for range 2 {
		if err := exp.Send(context.Background(), pipeline.Record{Trace: tr, Decision: keepHigh}); err != nil {
			t.Fatal(err)
		}
	}
	first, second := spansOf(up.uploads[0])[0], spansOf(up.uploads[1])[0]
	if string(first.TraceId) == string(second.TraceId) {
		t.Error("two exports share a trace ID")
	}
	// Phoenix keys spans by span ID alone: a repeat would be dropped.
	if string(first.SpanId) == string(second.SpanId) {
		t.Error("two exports share a span ID")
	}
}

// A back end that is down makes Send fail, so the export queue retries the
// trace and dead-letters it in the end, instead of the trace being lost.
func TestBackendDownFailsTheSend(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL + "/v1/traces"
	srv.Close()

	exp, err := Open(context.Background(), export.BackendConfig{Name: "down", Endpoint: url})
	if err != nil {
		t.Fatalf("Open with the back end down: %v", err)
	}
	defer exp.Shutdown(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = exp.Send(ctx, pipeline.Record{Trace: sampleTrace(), Decision: keepHigh, Model: "kev-latest"})
	if !fault.Retryable(err) {
		t.Errorf("Send with the back end down = %v, want a retryable fault", err)
	}
}

// The real client delivers OTLP to the configured URL path, with its own
// headers and the configured resource attributes (Phoenix picks the project
// from openinference.project.name). The global OTEL_EXPORTER_OTLP_HEADERS
// is ignored, so one back end's credentials never reach another.
func TestBackendReceivesOTLP(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "Authorization=leaked,x-global=1")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS", "x-global-traces=1")
	var path, contentType string
	var header http.Header
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, contentType, header = r.URL.Path, r.Header.Get("Content-Type"), r.Header.Clone()
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer srv.Close()

	exp, err := Open(context.Background(), export.BackendConfig{
		Name:               "phoenix",
		Endpoint:           srv.URL + "/v1/traces",
		Headers:            map[string]string{"Authorization": "Basic abc"},
		ResourceAttributes: map[string]string{"openinference.project.name": "my-project"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer exp.Shutdown(context.Background())
	if err := exp.Send(context.Background(), pipeline.Record{Trace: sampleTrace(), Decision: keepHigh, Model: "kev-latest"}); err != nil {
		t.Fatal(err)
	}

	// No Shutdown or flush needed: the request has been answered.
	if path != "/v1/traces" || contentType != "application/x-protobuf" {
		t.Errorf("request = %s (%s), want /v1/traces (application/x-protobuf)", path, contentType)
	}
	if got := header.Get("Authorization"); got != "Basic abc" {
		t.Errorf("Authorization = %q, want the back end's own", got)
	}
	if header.Get("x-global") != "" || header.Get("x-global-traces") != "" {
		t.Error("headers from OTEL_EXPORTER_OTLP_*HEADERS were sent")
	}
	// Protobuf stores strings as plain bytes, so the attribute names and
	// values can be checked without decoding the message.
	for _, want := range []string{"tailwise", "openinference.project.name", "my-project", "agent_run", "eval.p_keep", "tool.name"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("OTLP payload does not contain %q", want)
		}
	}
}
