package observe

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/tecnosam/tailwise/internal/fault"
)

type SinkKind string

const (
	SinkStdout SinkKind = "stdout"
	SinkFile   SinkKind = "file"
	SinkOTLP   SinkKind = "otlp"
)

const (
	serviceName = "tailwise"
	scopeName   = "github.com/tecnosam/tailwise"
)

type SinkConfig struct {
	Kind     SinkKind
	Path     string
	Endpoint string
	Headers  map[string]string
}

// If a sink cannot be built, the ones already built are closed.
func Open(ctx context.Context, sinks []SinkConfig, counter ErrorCounter) (*Pipeline, error) {
	var handlers []slog.Handler
	var closers []func(context.Context) error
	for _, cfg := range sinks {
		h, closer, err := openSink(ctx, cfg)
		if err != nil {
			for _, c := range closers {
				_ = c(ctx)
			}
			return nil, fault.Invalid(fault.Config, "build error sink", err)
		}
		handlers = append(handlers, h)
		if closer != nil {
			closers = append(closers, closer)
		}
	}
	p := New(slog.NewMultiHandler(handlers...), counter)
	p.closers = closers
	return p, nil
}

func openSink(ctx context.Context, cfg SinkConfig) (slog.Handler, func(context.Context) error, error) {
	switch cfg.Kind {
	case SinkStdout:
		return slog.NewJSONHandler(os.Stdout, nil), nil, nil
	case SinkFile:
		f, err := os.OpenFile(cfg.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, nil, fmt.Errorf("open error log %s: %w", cfg.Path, err)
		}
		return slog.NewJSONHandler(f, nil), func(context.Context) error { return f.Close() }, nil
	case SinkOTLP:
		exp, err := otlploghttp.New(ctx,
			otlploghttp.WithEndpointURL(cfg.Endpoint),
			otlploghttp.WithHeaders(maps.Clone(cfg.Headers)))
		if err != nil {
			return nil, nil, fmt.Errorf("otlp error sink: %w", err)
		}
		provider := sdklog.NewLoggerProvider(
			sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)),
			sdklog.WithResource(resource.NewSchemaless(attribute.String("service.name", serviceName))))
		return otelslog.NewHandler(scopeName, otelslog.WithLoggerProvider(provider)), provider.Shutdown, nil
	}
	return nil, nil, fmt.Errorf("unknown sink kind %q", cfg.Kind)
}
