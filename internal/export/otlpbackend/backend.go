// Package otlpbackend sends kept traces to any OTLP/HTTP endpoint: Phoenix,
// Langfuse, an OpenTelemetry Collector.
package otlpbackend

import (
	"context"
	"crypto/rand"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/tecnosam/tailwise/internal/export"
	"github.com/tecnosam/tailwise/internal/fault"
	"github.com/tecnosam/tailwise/internal/observe"
	"github.com/tecnosam/tailwise/internal/otlp"
	"github.com/tecnosam/tailwise/internal/pipeline"
	"github.com/tecnosam/tailwise/internal/trace"
)

const (
	serviceName       = "tailwise"
	scopeName         = "github.com/tecnosam/tailwise"
	decisionKeep      = "keep"
	attrSourceTraceID = "tailwise.source_trace_id"
	attrInputValue    = "input.value"
	attrOutputValue   = "output.value"

	// A send runs inside an export queue worker, so it has to give up well within
	// the queue's ack wait.
	sendTimeout = 10 * time.Second
)

type Uploader interface {
	UploadTraces(ctx context.Context, spans []*tracepb.ResourceSpans) error
}

// Backend sends one request per trace and returns once the endpoint has
// answered, so a failed send is retried by the export queue. A trace that
// arrived as OTLP spans is sent as those spans; any other is rebuilt.
type Backend struct {
	client   Uploader
	resource []*commonpb.KeyValue
	stop     func(context.Context) error
	now      func() time.Time
}

func New(client Uploader, resourceAttrs map[string]string) *Backend {
	return &Backend{client: client, resource: protoAttrs(resourceAttrs), now: time.Now}
}

// Open does not connect until the first send, so the back end may be down at
// start-up.
func Open(ctx context.Context, cfg export.BackendConfig) (*Backend, error) {
	client := otlptracehttp.NewClient(
		otlptracehttp.WithEndpointURL(cfg.Endpoint),
		// Always set, even when empty, so no headers come from
		// OTEL_EXPORTER_OTLP_HEADERS: one back end's credentials never reach another.
		otlptracehttp.WithHeaders(maps.Clone(cfg.Headers)),
		otlptracehttp.WithTimeout(sendTimeout),
		// The export queue retries, with the exporter's own max_deliver and backoff.
		otlptracehttp.WithRetry(otlptracehttp.RetryConfig{Enabled: false}),
	)
	if err := client.Start(ctx); err != nil {
		return nil, fmt.Errorf("export: start OTLP client: %w", err)
	}
	b := New(client, cfg.ResourceAttributes)
	b.stop = client.Stop
	return b, nil
}

func (b *Backend) Send(ctx context.Context, rec pipeline.Record) error {
	a := otlp.Annotation{Resource: b.resource, Root: evalAttrs(rec)}
	var spans []*tracepb.ResourceSpans
	var err error
	if len(rec.Trace.OTLP) > 0 {
		spans, err = otlp.Annotate(rec.Trace.OTLP, a)
	} else {
		spans, err = otlp.AnnotateSpans(b.rebuild(rec.Trace), a)
	}
	if err != nil {
		return fault.Invalid(fault.Export, "annotate spans", err)
	}
	if err := b.client.UploadTraces(ctx, spans); err != nil {
		return fault.Unavailable(fault.Export, "send spans", err)
	}
	return nil
}

func evalAttrs(rec pipeline.Record) []*commonpb.KeyValue {
	d := rec.Decision
	attrs := []*commonpb.KeyValue{
		otlp.StringAttr("eval.agent", string(rec.Trace.Agent)),
		otlp.StringAttr("eval.decision", decisionKeep),
		otlp.StringAttr("eval.rule", string(d.Rule)),
		otlp.StringAttr("eval.reason", d.Reason),
		otlp.StringAttr("eval.model", string(rec.Model)),
		otlp.FloatAttr("eval.latency_ms", pipeline.Milliseconds(rec.Latency)),
	}
	if d.Cause != "" {
		attrs = append(attrs, otlp.StringAttr("eval.cause", d.Cause))
	}
	if d.Scored {
		attrs = append(attrs, otlp.FloatAttr("eval.p_keep", float64(d.PKeep)))
	}
	if len(rec.Flags) > 0 {
		attrs = append(attrs, otlp.StringAttr("eval.flags", strings.Join(rec.Flags, ",")),
			otlp.FloatAttr("eval.severity", d.Severity))
	}
	for _, q := range slices.Sorted(maps.Keys(rec.Answers)) {
		attrs = append(attrs, otlp.FloatAttr("eval.answers."+q, float64(rec.Answers[q])))
	}
	return attrs
}

// rebuild renders a JSON API trace as an agent span with one child per tool
// call, in a new trace timed from now.
func (b *Backend) rebuild(t trace.Trace) []*tracepb.ResourceSpans {
	spans := otlp.SpansFromTrace(randomID(16), t)
	// Phoenix treats span IDs as unique across traces, so replace
	// SpansFromTrace's 1, 2, ... with random ones.
	ids := make(map[string][]byte, len(spans))
	start := uint64(b.now().UnixNano())
	for _, s := range spans {
		id := randomID(8)
		ids[string(s.SpanId)] = id
		s.SpanId = id
		s.StartTimeUnixNano += start
		s.EndTimeUnixNano += start
	}
	for _, s := range spans {
		if len(s.ParentSpanId) > 0 {
			s.ParentSpanId = ids[string(s.ParentSpanId)]
		}
	}
	root := spans[0]
	root.Attributes = otlp.SetAttr(root.Attributes, otlp.StringAttr(attrSourceTraceID, t.TraceID))
	for i, c := range t.ToolCalls {
		s := spans[i+1]
		s.Attributes = otlp.SetAttr(s.Attributes, otlp.StringAttr(attrOutputValue, c.StatusText()))
		if args := c.ArgsText(); args != "" {
			s.Attributes = otlp.SetAttr(s.Attributes, otlp.StringAttr(attrInputValue, args))
		}
	}
	return []*tracepb.ResourceSpans{{
		Resource:   &resourcepb.Resource{Attributes: []*commonpb.KeyValue{otlp.StringAttr("service.name", serviceName)}},
		ScopeSpans: []*tracepb.ScopeSpans{{Scope: &commonpb.InstrumentationScope{Name: scopeName}, Spans: spans}},
	}}
}

func randomID(n int) []byte {
	id := make([]byte, n)
	for {
		_, _ = rand.Read(id) // never fails
		for _, b := range id {
			if b != 0 {
				return id
			}
		}
	}
}

func protoAttrs(attrs map[string]string) []*commonpb.KeyValue {
	out := make([]*commonpb.KeyValue, 0, len(attrs))
	for k, v := range attrs {
		out = append(out, otlp.StringAttr(k, v))
	}
	return out
}

// ReportErrors routes OpenTelemetry's asynchronous errors, such as a failed
// metrics export, to reporter.
func ReportErrors(reporter observe.Reporter) {
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		reporter.Report(context.Background(), fault.Unavailable(fault.Export, "opentelemetry sdk", err))
	}))
}

func (b *Backend) Shutdown(ctx context.Context) error {
	if b.stop == nil {
		return nil
	}
	return b.stop(ctx)
}

func Build(ctx context.Context, cfg export.BackendConfig) (export.AbstractBackend, error) {
	return Open(ctx, cfg)
}

var _ export.BackendFunc = Build

var _ export.AbstractBackend = (*Backend)(nil)
