package export

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/tecnosam/tailwise/internal/fault"
	"github.com/tecnosam/tailwise/internal/metrics"
	"github.com/tecnosam/tailwise/internal/observe"
	"github.com/tecnosam/tailwise/internal/pipeline"
)

type DispatcherConfig struct {
	Backends   []BackendConfig
	NewBackend BackendFunc
	NewQueue   QueueFunc
	Reporter   observe.Reporter
	Metrics    SendMetrics
}

type SendMetrics interface {
	SendFinished(ctx context.Context, s metrics.Send)
}

// Dispatcher queues a kept trace for every back end and returns at once. A back
// end whose queue is full skips the trace, so one slow back end never holds up
// scoring or the others.
type Dispatcher struct {
	routes   []route
	reporter observe.Reporter
}

type route struct {
	name    string
	backend AbstractBackend
	queue   AbstractQueue
}

// If a back end cannot be built, those already built are shut down.
func NewDispatcher(ctx context.Context, cfg DispatcherConfig) (*Dispatcher, error) {
	if cfg.Reporter == nil {
		cfg.Reporter = observe.Discard
	}
	if cfg.NewBackend == nil || cfg.NewQueue == nil {
		return nil, errors.New("export: DispatcherConfig.NewBackend and NewQueue are required")
	}
	if cfg.Metrics == nil {
		cfg.Metrics = metrics.Noop{}
	}
	d := &Dispatcher{reporter: cfg.Reporter}
	for _, bc := range cfg.Backends {
		r, err := buildRoute(ctx, cfg, bc)
		if err != nil {
			_ = d.Shutdown(context.Background())
			return nil, fault.Invalid(fault.Export, "build exporter", err).With("exporter", bc.Name)
		}
		d.routes = append(d.routes, r)
	}
	return d, nil
}

func buildRoute(ctx context.Context, cfg DispatcherConfig, bc BackendConfig) (route, error) {
	backend, err := cfg.NewBackend(ctx, bc)
	if err != nil {
		return route{}, err
	}
	q, err := cfg.NewQueue(ctx, QueueConfig{
		Name:        bc.Name,
		Size:        bc.QueueSize,
		Concurrency: bc.Concurrency,
		Retry:       bc.Retry,
		Send:        timed(bc.Name, backend.Send, cfg.Metrics),
		Reporter:    cfg.Reporter,
	})
	if err != nil {
		_ = backend.Shutdown(ctx)
		return route{}, err
	}
	return route{name: bc.Name, backend: backend, queue: q}, nil
}

func timed(exporter string, send SendFunc, m SendMetrics) SendFunc {
	return func(ctx context.Context, rec pipeline.Record) error {
		start := time.Now()
		err := send(ctx, rec)
		m.SendFinished(ctx, metrics.Send{Exporter: exporter, Duration: time.Since(start), Err: err})
		return err
	}
}

func (d *Dispatcher) Depths() []metrics.QueueDepth {
	out := make([]metrics.QueueDepth, len(d.routes))
	for i, r := range d.routes {
		out[i] = metrics.QueueDepth{Queue: "export:" + r.name, Depth: r.queue.Depth}
	}
	return out
}

// Export reports whether at least one back end took the trace.
func (d *Dispatcher) Export(ctx context.Context, rec pipeline.Record) bool {
	if !rec.Decision.Keep {
		return false
	}
	accepted := false
	for _, r := range d.routes {
		if err := r.queue.Enqueue(ctx, rec); err != nil {
			d.reporter.Report(ctx, r.fault("queue trace for export, skipped it", err).WithTrace(rec.Trace.TraceID))
			continue
		}
		accepted = true
	}
	return accepted
}

func (d *Dispatcher) Flush(ctx context.Context) error {
	return d.each(func(r route) error {
		return r.wrap("wait for queued exports", r.queue.Flush(ctx))
	})
}

func (d *Dispatcher) Shutdown(ctx context.Context) error {
	return d.each(func(r route) error {
		return errors.Join(r.wrap("finish queued exports", r.queue.Shutdown(ctx)), r.wrap("shut down", r.backend.Shutdown(ctx)))
	})
}

// each runs fn for every route at once, so one slow back end does not use up
// the others' time.
func (d *Dispatcher) each(fn func(route) error) error {
	errs := make([]error, len(d.routes))
	var wg sync.WaitGroup
	for i, r := range d.routes {
		wg.Go(func() { errs[i] = fn(r) })
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (r route) fault(op fault.Op, err error) *fault.Error {
	return fault.New(fault.KindOf(err), fault.Export, op, err).With("exporter", r.name)
}

func (r route) wrap(op fault.Op, err error) error {
	if err == nil {
		return nil
	}
	return r.fault(op, err)
}
