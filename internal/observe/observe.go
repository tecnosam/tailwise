// Package observe writes the engine's own errors as structured log records to
// every configured sink, and counts them for metrics.
package observe

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"

	"github.com/tecnosam/tailwise/internal/fault"
)

type Reporter interface {
	Report(ctx context.Context, err error)
}

var Discard Reporter = discard{}

type discard struct{}

func (discard) Report(context.Context, error) {}

type ErrorCounter interface {
	ErrorReported(ctx context.Context, kind, component string)
}

type Pipeline struct {
	logger  *slog.Logger
	closers []func(context.Context) error
}

// New counts nothing when counter is nil.
func New(h slog.Handler, counter ErrorCounter) *Pipeline {
	if counter != nil {
		h = counting{Handler: h, counter: counter}
	}
	return &Pipeline{logger: slog.New(h)}
}

// Report logs work lost and bugs as errors, anything retried or refused as a
// warning. A foreign error is reported as an internal error.
func (p *Pipeline) Report(ctx context.Context, err error) {
	if err == nil {
		return
	}
	e, ok := errors.AsType[*fault.Error](err)
	if !ok {
		e = fault.Internal("unknown", "unclassified error", err)
	}
	level := slog.LevelWarn
	if e.Severe || e.Kind == fault.KindInternal {
		level = slog.LevelError
	}
	attrs := []slog.Attr{
		slog.String(attrKind, e.Kind.String()),
		slog.String(attrComponent, string(e.Component)),
	}
	if e.TraceID != "" {
		attrs = append(attrs, slog.String("trace_id", e.TraceID))
	}
	if e.Cause != nil {
		attrs = append(attrs, slog.String("error", e.Cause.Error()))
	}
	for _, k := range slices.Sorted(maps.Keys(e.Attrs)) {
		attrs = append(attrs, slog.Any(k, e.Attrs[k]))
	}
	p.logger.LogAttrs(ctx, level, string(e.Op), attrs...)
}

func (p *Pipeline) Close(ctx context.Context) error {
	var errs []error
	for _, c := range p.closers {
		errs = append(errs, c(ctx))
	}
	return errors.Join(errs...)
}
