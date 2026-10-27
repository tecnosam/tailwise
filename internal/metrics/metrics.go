// Package metrics records the engine's own OpenTelemetry metrics; the README's
// Metrics section lists the instruments.
package metrics

import (
	"context"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/tecnosam/tailwise/internal/fault"
	"github.com/tecnosam/tailwise/internal/pipeline"
)

const MeterName = "github.com/tecnosam/tailwise"

const (
	ProtocolHTTP = "http"
	ProtocolGRPC = "grpc"
)

var latencyBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

type Request struct {
	Server   string // "api" or "otlp"
	Protocol string
	Method   string // the HTTP method, or the gRPC full method name
	Route    string // empty for gRPC
	Status   int    // the HTTP status, or the gRPC status code
	Duration time.Duration
}

type Send struct {
	Exporter string
	Duration time.Duration
	Err      error
}

type QueueDepth struct {
	Queue string // "traces", or "export:<exporter>"
	Depth func(ctx context.Context) (int64, error)
}

type Recorder struct {
	meter          metric.Meter
	httpDuration   metric.Float64Histogram
	rpcDuration    metric.Float64Histogram
	scoreDuration  metric.Float64Histogram
	exportDuration metric.Float64Histogram
	decisions      metric.Int64Counter
	errors         metric.Int64Counter
}

func New(meter metric.Meter) (*Recorder, error) {
	b := builder{meter: meter}
	r := &Recorder{
		meter:          meter,
		httpDuration:   b.histogram("http.server.request.duration", "Duration of HTTP requests the engine answered."),
		rpcDuration:    b.histogram("rpc.server.call.duration", "Duration of gRPC calls the engine answered."),
		scoreDuration:  b.histogram("tailwise.scorer.duration", "Duration of calls to a decision model, retries included."),
		exportDuration: b.histogram("tailwise.export.duration", "Duration of sending a kept trace to a back end."),
		decisions:      b.counter("tailwise.decisions", "{trace}", "Traces decided, by decision and reason."),
		errors:         b.counter("tailwise.errors", "{error}", "Errors reported to the error pipeline."),
	}
	return r, b.err
}

// A queue that cannot be read is skipped for that collection.
func (r *Recorder) ObserveQueues(queues ...QueueDepth) error {
	gauge, err := r.meter.Int64ObservableGauge("tailwise.queue.depth",
		metric.WithUnit("{trace}"), metric.WithDescription("Traces waiting in a queue: to be scored, or to be sent to a back end."))
	if err != nil {
		return err
	}
	_, err = r.meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		for _, q := range queues {
			if n, err := q.Depth(ctx); err == nil {
				o.ObserveInt64(gauge, n, metric.WithAttributes(attribute.String("tailwise.queue", q.Queue)))
			}
		}
		return nil
	}, gauge)
	return err
}

func (r *Recorder) RequestFinished(ctx context.Context, req Request) {
	if req.Protocol == ProtocolGRPC {
		r.rpcDuration.Record(ctx, req.Duration.Seconds(), metric.WithAttributes(grpcAttrs(req)...))
		return
	}
	r.httpDuration.Record(ctx, req.Duration.Seconds(), metric.WithAttributes(httpAttrs(req)...))
}

func (r *Recorder) ScoreFinished(ctx context.Context, res pipeline.ScoreResult) {
	attrs := append(modelAttrs(string(res.Agent), string(res.Model)), errorAttrs(res.Err)...)
	r.scoreDuration.Record(ctx, res.Duration.Seconds(), metric.WithAttributes(attrs...))
}

func (r *Recorder) TraceDecided(ctx context.Context, rec pipeline.Record) {
	attrs := append(modelAttrs(string(rec.Agent), string(rec.Model)),
		attribute.String("tailwise.decision", rec.Decision.Label()),
		attribute.String("tailwise.rule", string(rec.Decision.Rule)))
	// The cause is bounded by the manifest's choices; the reason itself is free
	// text and stays out of metrics.
	if c := rec.Decision.Cause; c != "" {
		attrs = append(attrs, attribute.String("tailwise.cause", c))
	}
	r.decisions.Add(ctx, 1, metric.WithAttributes(attrs...))
}

func (r *Recorder) SendFinished(ctx context.Context, s Send) {
	attrs := append([]attribute.KeyValue{attribute.String("tailwise.exporter", s.Exporter)}, errorAttrs(s.Err)...)
	r.exportDuration.Record(ctx, s.Duration.Seconds(), metric.WithAttributes(attrs...))
}

func (r *Recorder) ErrorReported(ctx context.Context, kind, component string) {
	r.errors.Add(ctx, 1, metric.WithAttributes(
		attribute.String("tailwise.component", component),
		attribute.String("error.type", kind)))
}

func httpAttrs(req Request) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("tailwise.server", req.Server),
		attribute.String("http.request.method", req.Method),
		attribute.String("http.route", req.Route),
		attribute.Int("http.response.status_code", req.Status),
	}
	if req.Status >= 500 {
		attrs = append(attrs, attribute.String("error.type", strconv.Itoa(req.Status)))
	}
	return attrs
}

func grpcAttrs(req Request) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("tailwise.server", req.Server),
		attribute.String("rpc.system", "grpc"),
		attribute.String("rpc.method", req.Method),
		attribute.Int("rpc.grpc.status_code", req.Status),
	}
	if req.Status != 0 {
		attrs = append(attrs, attribute.String("error.type", strconv.Itoa(req.Status)))
	}
	return attrs
}

func modelAttrs(agent, model string) []attribute.KeyValue {
	return []attribute.KeyValue{attribute.String("tailwise.agent", agent), attribute.String("tailwise.model", model)}
}

func errorAttrs(err error) []attribute.KeyValue {
	if err == nil {
		return nil
	}
	return []attribute.KeyValue{attribute.String("error.type", fault.KindOf(err).String())}
}

type builder struct {
	meter metric.Meter
	err   error
}

func (b *builder) histogram(name, desc string) metric.Float64Histogram {
	h, err := b.meter.Float64Histogram(name, metric.WithUnit("s"), metric.WithDescription(desc),
		metric.WithExplicitBucketBoundaries(latencyBuckets...))
	b.keep(err)
	return h
}

func (b *builder) counter(name, unit, desc string) metric.Int64Counter {
	c, err := b.meter.Int64Counter(name, metric.WithUnit(unit), metric.WithDescription(desc))
	b.keep(err)
	return c
}

func (b *builder) keep(err error) {
	if b.err == nil {
		b.err = err
	}
}
