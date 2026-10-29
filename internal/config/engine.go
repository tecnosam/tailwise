package config

import (
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/tecnosam/tailwise/internal/broker"
	"github.com/tecnosam/tailwise/internal/export"
	exportnats "github.com/tecnosam/tailwise/internal/export/natsq"
	"github.com/tecnosam/tailwise/internal/metrics"
	"github.com/tecnosam/tailwise/internal/observe"
	"github.com/tecnosam/tailwise/internal/pipeline"
	"github.com/tecnosam/tailwise/internal/queue"
	"github.com/tecnosam/tailwise/internal/retry"
	"github.com/tecnosam/tailwise/internal/scorer"
	"github.com/tecnosam/tailwise/internal/trace"
)

// Engine is every component's settings in the component's own type, secrets and
// manifests already read. Main adds runtime dependencies such as loggers.
type Engine struct {
	Server ServerSettings
	OTLP   OTLPSettings

	Broker        broker.Config
	Queue         queue.Config
	ExportStreams exportnats.Streams
	Exporters     []export.BackendConfig

	Models scorer.Models
	Policy pipeline.PolicyConfig

	Metrics    metrics.Config
	ErrorSinks []observe.SinkConfig

	// Summary is what main logs at start-up; never a secret.
	Summary []any
	// Warnings are settings that work but deserve attention, logged at start-up.
	Warnings []string
}

type ServerSettings struct {
	Addr            string
	Workers         int
	ShutdownTimeout time.Duration
}

type OTLPSettings struct {
	GRPCAddr, HTTPAddr string // empty turns that protocol off
	Assemblers         int
	AgentAttributes    []string
}

func (o OTLPSettings) Enabled() bool { return o.GRPCAddr != "" || o.HTTPAddr != "" }

func (f File) engine(sec secrets, manifests map[string]*Manifest) *Engine {
	q := f.Queue
	n := natsNames(q.Prefix)
	eng := &Engine{
		Server: ServerSettings{Addr: f.Server.Addr, Workers: f.Server.Workers, ShutdownTimeout: f.Server.ShutdownTimeout},
		OTLP: OTLPSettings{
			GRPCAddr: f.OTLP.GRPCAddr, HTTPAddr: f.OTLP.HTTPAddr,
			Assemblers: f.OTLP.Assemblers, AgentAttributes: slices.Clone(f.Agents.Attributes),
		},
		Broker: broker.Config{
			URL:      q.URL,
			StoreDir: q.StoreDir,
			Replicas: q.Replicas,
			Auth: broker.NATSAuth{
				CredentialsFile: q.Auth.CredentialsFile, NkeySeedFile: q.Auth.NkeySeedFile,
				User: q.Auth.User, Password: sec.natsPassword, Token: sec.natsToken,
			},
			TLS: broker.NATSTLS{CAFile: q.TLS.CAFile, CertFile: q.TLS.CertFile, KeyFile: q.TLS.KeyFile},
		},
		Queue: queue.Config{
			ConnectTimeout: q.ConnectTimeout,
			Stream:         n.stream("TRACES"),
			Subject:        n.subject("traces"),
			Consumer:       n.consumer("workers"),
			MaxMessages:    q.MaxMessages,
			AckWait:        q.AckWait,
			Redelivery:     retry.Policy{Attempts: q.MaxDeliver, Backoff: q.RedeliveryBackoff},
			DLQStream:      n.stream("TRACES_DLQ"),
			DLQSubject:     n.subject("traces.dlq"),
			DLQMaxAge:      q.DLQMaxAge,
			Spans:          f.OTLP.spans(n),
		},
		ExportStreams: exportnats.Streams{
			Stream:     n.stream("EXPORTS"),
			Subject:    n.subject("exports"),
			Consumer:   n.consumer("workers-export"),
			DLQStream:  n.stream("EXPORTS_DLQ"),
			DLQSubject: n.subject("exports_dlq"),
			DLQMaxAge:  q.DLQMaxAge,
		},
		Models: scorer.Models{
			Default: model(f.Scorer, sec.apiKeys[""], manifests[""]),
			ByAgent: make(map[trace.AgentName]scorer.ModelConfig, len(f.Agents.Models)),
		},
		Policy: pipeline.PolicyConfig{
			KeepThreshold: pipeline.Probability(f.Policy.KeepThreshold),
			RandomFloor:   pipeline.Probability(f.Policy.RandomFloor),
		},
		Metrics: metrics.Config{
			Endpoint:           f.Observability.Metrics.Endpoint,
			Headers:            sec.metricsHeaders,
			Interval:           f.Observability.Metrics.Interval,
			ResourceAttributes: maps.Clone(f.Observability.Metrics.ResourceAttributes),
		},
	}
	for agent, m := range f.Agents.Models {
		eng.Models.ByAgent[trace.AgentName(agent)] = model(m, sec.apiKeys[agent], manifests[agent])
	}
	for i, e := range f.Exporters {
		eng.Exporters = append(eng.Exporters, export.BackendConfig{
			Name:               e.Name,
			Endpoint:           e.Endpoint,
			Headers:            sec.exporterHeaders[i],
			ResourceAttributes: maps.Clone(e.ResourceAttributes),
			QueueSize:          e.QueueSize,
			Concurrency:        e.Concurrency,
			Retry:              retry.Policy{Attempts: e.MaxDeliver, Backoff: e.Backoff},
		})
	}
	for i, s := range f.Observability.ErrorSinks {
		eng.ErrorSinks = append(eng.ErrorSinks, observe.SinkConfig{
			Kind: observe.SinkKind(s.Kind), Path: s.Path, Endpoint: s.Endpoint, Headers: sec.sinkHeaders[i],
		})
	}
	eng.Summary = f.summary(manifests[""])
	return eng
}

func model(s Scorer, apiKey string, m *Manifest) scorer.ModelConfig {
	return scorer.ModelConfig{
		Client: scorer.Config{
			Endpoint:         s.Endpoint,
			Model:            s.Model,
			Questions:        m.Questions,
			DecisionQuestion: s.DecisionQuestion,
			AnyOf:            m.anyOf(),
			ReasonQuestion:   reasonQuestion(s, m),
			Reasons:          m.Reasons,
			APIKey:           apiKey,
			Timeout:          s.Timeout,
		},
		Retry:  retry.Policy{Attempts: s.MaxRetries + 1, Backoff: s.Backoff, Jitter: true},
		Checks: m.checks(s.DecisionQuestion),
	}
}

func reasonQuestion(s Scorer, m *Manifest) string {
	if m.Reasons == nil {
		return ""
	}
	return s.ReasonQuestion
}

// natsNames builds every NATS name from queue.prefix. With the default,
// "tailwise", the streams are TAILWISE_TRACES, TAILWISE_TRACES_DLQ,
// TAILWISE_EXPORTS, TAILWISE_EXPORTS_DLQ, TAILWISE_SPANS and TAILWISE_PENDING,
// the subjects tailwise.traces and so on, and the consumers tailwise-workers
// and tailwise-workers-export.
type natsNames string

func (n natsNames) stream(name string) string   { return strings.ToUpper(string(n)) + "_" + name }
func (n natsNames) subject(name string) string  { return string(n) + "." + name }
func (n natsNames) consumer(name string) string { return string(n) + "-" + name }

// With OTLP off the span config is zero, and the queue creates no span streams.
func (o OTLP) spans(n natsNames) queue.SpanConfig {
	if !o.Enabled() {
		return queue.SpanConfig{}
	}
	return queue.SpanConfig{
		Stream:         n.stream("SPANS"),
		Subject:        n.subject("spans"),
		MaxAge:         o.SpanMaxAge,
		PendingStream:  n.stream("PENDING"),
		PendingSubject: n.subject("pending"),
		IdleTimeout:    o.IdleTimeout,
		MaxWait:        o.MaxWait,
	}
}

func (f File) summary(m *Manifest) []any {
	agents := make([]string, 0, len(f.Agents.Models))
	for agent, s := range f.Agents.Models {
		agents = append(agents, agent+"="+s.Model+"@"+s.Endpoint)
	}
	slices.Sort(agents)
	exporters := make([]string, len(f.Exporters))
	for i, e := range f.Exporters {
		exporters[i] = e.Name + "=" + e.Endpoint
	}
	return []any{
		"addr", f.Server.Addr,
		"workers", f.Server.Workers,
		"otlp_grpc_addr", f.OTLP.GRPCAddr,
		"otlp_http_addr", f.OTLP.HTTPAddr,
		"otlp_idle_timeout", f.OTLP.IdleTimeout.String(),
		"nats_url", f.Queue.URL,
		"nats_prefix", f.Queue.Prefix,
		"max_deliver", f.Queue.MaxDeliver,
		"scorer_kind", f.Scorer.Kind,
		"scorer_api_key_env", f.Scorer.APIKeyEnv,
		"scorer_endpoint", f.Scorer.Endpoint,
		"scorer_model", f.Scorer.Model,
		"agent_models", agents,
		"agent_attributes", f.Agents.Attributes,
		"metrics_endpoint", f.Observability.Metrics.Endpoint,
		"manifest_model", m.Model,
		"decision_question", f.Scorer.DecisionQuestion,
		"keep_threshold", f.Policy.KeepThreshold,
		"random_floor", f.Policy.RandomFloor,
		"exporters", exporters,
	}
}
