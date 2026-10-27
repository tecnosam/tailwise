// Command tailwise runs the Tailwise inference engine.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tecnosam/tailwise/internal/broker"
	"github.com/tecnosam/tailwise/internal/config"
	"github.com/tecnosam/tailwise/internal/export"
	"github.com/tecnosam/tailwise/internal/export/otlpbackend"
	"github.com/tecnosam/tailwise/internal/fault"
	"github.com/tecnosam/tailwise/internal/metrics"
	"github.com/tecnosam/tailwise/internal/observe"
	"github.com/tecnosam/tailwise/internal/otlp"
	"github.com/tecnosam/tailwise/internal/pipeline"
	"github.com/tecnosam/tailwise/internal/queue"
	"github.com/tecnosam/tailwise/internal/scorer"
	"github.com/tecnosam/tailwise/internal/server"
	"github.com/tecnosam/tailwise/internal/trace"
)

// Separate from the shutdown timeout, so a slow drain cannot leave the final
// export flush with no time.
const exportShutdownTimeout = 10 * time.Second

// version is set at build time: go build -ldflags "-X main.version=0.1.0".
var version = "dev"

func main() {
	configPath := flag.String("config", "", "path to a YAML config file; without one the engine runs on its built-in defaults and the environment")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn or error")
	check := flag.Bool("check", false, "load and validate the config and its manifests, then exit")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("tailwise", version)
		return
	}
	if *check {
		os.Exit(checkConfig(*configPath))
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintf(os.Stderr, "tailwise: invalid -log-level %q\n", *logLevel)
		os.Exit(2)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	if err := run(*configPath, logger); err != nil {
		logger.Error("tailwise stopped", "error", err.Error())
		os.Exit(1)
	}
}

func checkConfig(path string) int {
	eng, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tailwise: %v\n", err)
		return 1
	}
	for _, w := range eng.Warnings {
		fmt.Fprintf(os.Stderr, "tailwise: warning: %s\n", w)
	}
	if path == "" {
		fmt.Println("tailwise: the built-in configuration is valid")
		return 0
	}
	fmt.Printf("tailwise: %s is valid\n", path)
	return 0
}

func run(configPath string, logger *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	a := &app{cfg: cfg, logger: logger}
	if err := a.start(ctx); err != nil {
		a.abort()
		return err
	}
	defer a.close()
	return a.serve(ctx, stop)
}

// app keeps every component main starts, so they stop in the right order.
type app struct {
	cfg    *config.Engine
	logger *slog.Logger

	metrics    *metrics.Pipeline
	reporter   *observe.Pipeline
	broker     *broker.Broker
	queue      queue.AbstractBroker
	exporter   *export.Dispatcher
	engine     *pipeline.Engine
	workers    queue.Stopper
	receiver   *otlp.Receiver
	assemblers queue.Stopper
	http       *http.Server
}

func (a *app) start(ctx context.Context) error {
	steps := []func(context.Context) error{
		a.startMetrics, a.startReporter, a.startBroker, a.startQueue, a.startExporter,
		a.startEngine, a.startWorkers, a.startOTLP, a.startHTTP, a.observeQueues,
	}
	for _, step := range steps {
		if err := step(ctx); err != nil {
			return err
		}
	}
	a.logStart()
	a.probeScorer(ctx)
	return nil
}

func (a *app) startMetrics(ctx context.Context) error {
	m, err := metrics.Start(ctx, a.cfg.Metrics)
	a.metrics = m
	return err
}

func (a *app) startReporter(ctx context.Context) error {
	reporter, err := observe.Open(ctx, a.cfg.ErrorSinks, a.metrics)
	a.reporter = reporter
	return err
}

func (a *app) startBroker(context.Context) error {
	bcfg := a.cfg.Broker
	bcfg.Logger, bcfg.Reporter = a.logger, a.reporter
	b, err := broker.New(bcfg)
	a.broker = b
	return err
}

func (a *app) startQueue(ctx context.Context) error {
	qcfg := a.cfg.Queue
	qcfg.Logger, qcfg.Reporter = a.logger, a.reporter
	q, err := a.broker.Ingest(ctx, qcfg)
	a.queue = q
	return err
}

// startExporter comes after startQueue, which waits for JetStream to be up.
func (a *app) startExporter(ctx context.Context) error {
	otlpbackend.ReportErrors(a.reporter)
	exporter, err := export.NewDispatcher(ctx, export.DispatcherConfig{
		Backends:   a.cfg.Exporters,
		NewBackend: otlpbackend.Build,
		NewQueue:   a.broker.ExportQueues(a.cfg.ExportStreams),
		Reporter:   a.reporter,
		Metrics:    a.metrics,
	})
	a.exporter = exporter
	return err
}

func (a *app) observeQueues(context.Context) error {
	queues := append([]metrics.QueueDepth{{Queue: "traces", Depth: a.queue.Depth}}, a.exporter.Depths()...)
	return a.metrics.ObserveQueues(queues...)
}

func (a *app) startEngine(context.Context) error {
	models, err := scorer.BuildRouter(a.cfg.Models)
	if err != nil {
		return err
	}
	a.engine = pipeline.NewEngine(pipeline.Deps{
		Models:   models,
		Policy:   pipeline.NewPolicy(a.cfg.Policy, nil),
		Exporter: a.exporter,
		Reporter: a.reporter,
		Metrics:  a.metrics,
		Logger:   a.logger,
	})
	return nil
}

func (a *app) startWorkers(context.Context) error {
	a.workers = a.queue.StartWorkers(a.cfg.Server.Workers, func(ctx context.Context, t trace.Trace) error {
		_, err := a.engine.ProcessQueued(ctx, t)
		return err
	})
	return nil
}

func (a *app) startOTLP(context.Context) error {
	if !a.cfg.OTLP.Enabled() {
		return nil
	}
	a.receiver = otlp.NewReceiver(otlp.ReceiverConfig{Sink: a.queue, Reporter: a.reporter, Metrics: a.metrics})
	if err := a.receiver.Start(a.cfg.OTLP.GRPCAddr, a.cfg.OTLP.HTTPAddr); err != nil {
		a.receiver = nil
		return explainAddrInUse(err)
	}
	assembler := otlp.Assembler{AgentAttributes: a.cfg.OTLP.AgentAttributes}
	a.assemblers = a.queue.StartAssemblers(a.cfg.OTLP.Assemblers, assembler.Assemble)
	return nil
}

func explainAddrInUse(err error) error {
	if !errors.Is(err, syscall.EADDRINUSE) {
		return err
	}
	return fmt.Errorf("%w (Phoenix also listens on 4317; set otlp.grpc_addr / otlp.http_addr or %s / %s)",
		err, config.EnvOTLPGRPCAddr, config.EnvOTLPHTTPAddr)
}

func (a *app) startHTTP(context.Context) error {
	srv := server.New(server.Options{Engine: a.engine, Queue: a.queue, Reporter: a.reporter, Metrics: a.metrics})
	a.http = &http.Server{
		Addr:              a.cfg.Server.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	return nil
}

func (a *app) logStart() {
	a.logger.Info("tailwise starting", append([]any{"version", version, "broker", a.broker.Kind()}, a.cfg.Summary...)...)
	for _, w := range a.cfg.Warnings {
		a.logger.Warn(w)
	}
}

// probeScorer warns, without failing start-up, when the model is not up yet.
func (a *app) probeScorer(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := a.engine.Ready(ctx); err != nil {
		a.reporter.Report(ctx, fault.Unavailable(fault.Scorer,
			"probe at start-up, queued traces will be redelivered until it answers", err))
	}
}

func (a *app) serve(ctx context.Context, stop context.CancelFunc) error {
	serveErr := make(chan error, 1)
	go func() { serveErr <- a.http.ListenAndServe() }()
	select {
	case err := <-serveErr:
		// The listener failed before any signal.
		a.abort()
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}
	stop() // a second signal now kills the process the default way
	return a.shutdown()
}

// shutdown stops taking work and lets the work in flight finish within
// server.shutdown_timeout. What is still queued stays in JetStream.
func (a *app) shutdown() error {
	timeout := a.cfg.Server.ShutdownTimeout
	a.logger.Info("shutting down", "timeout", timeout.String())
	drainCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	errs := []error{
		wrapStop("stop http server", a.http.Shutdown(drainCtx)),
		a.stopOTLP(drainCtx),
		wrapStop("stop workers", a.workers.Stop(drainCtx)),
		a.flushExporter(),
		a.flushMetrics(),
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	a.logger.Info("shutdown complete")
	return nil
}

func (a *app) stopOTLP(ctx context.Context) error {
	if a.receiver == nil {
		return nil
	}
	return errors.Join(
		wrapStop("stop otlp receivers", a.receiver.Shutdown(ctx)),
		wrapStop("stop assemblers", a.assemblers.Stop(ctx)),
	)
}

// Without a store_dir the embedded queues vanish with the process, so wait for
// the export queues to empty first.
func (a *app) flushExporter() error {
	ctx, cancel := context.WithTimeout(context.Background(), exportShutdownTimeout)
	defer cancel()
	var drain error
	if a.broker.Ephemeral() {
		drain = wrapStop("send queued exports", a.exporter.Flush(ctx))
	}
	return errors.Join(drain, wrapStop("flush exporter", a.exporter.Shutdown(ctx)))
}

func (a *app) flushMetrics() error {
	ctx, cancel := context.WithTimeout(context.Background(), exportShutdownTimeout)
	defer cancel()
	return wrapStop("flush metrics", a.metrics.Shutdown(ctx))
}

// abort stops whatever start managed to start.
func (a *app) abort() {
	ctx, cancel := context.WithTimeout(context.Background(), a.cfg.Server.ShutdownTimeout)
	defer cancel()
	_ = a.stopOTLP(ctx)
	if a.workers != nil {
		_ = a.workers.Stop(ctx)
	}
	if a.exporter != nil {
		_ = a.exporter.Shutdown(context.Background())
	}
	if a.metrics != nil {
		_ = a.metrics.Shutdown(ctx)
	}
	a.close()
}

func (a *app) close() {
	if a.queue != nil {
		a.queue.Close()
		a.queue = nil
	}
	if a.broker != nil {
		a.broker.Close()
		a.broker = nil
	}
	if a.reporter != nil {
		_ = a.reporter.Close(context.Background())
		a.reporter = nil
	}
}

func wrapStop(what string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", what, err)
}
