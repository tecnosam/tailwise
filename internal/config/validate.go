package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	exportnats "github.com/tecnosam/tailwise/internal/export/natsq"
)

func (c File) validate(p *problems) {
	c.validateServer(p)
	validateScorer(p, "scorer", c.Scorer)
	c.validateAgents(p)
	c.validateQueue(p)
	c.validateOTLP(p)
	c.validateObservability(p)
	c.validatePolicy(p)
	c.validateExporters(p)
}

func (c File) validateServer(p *problems) {
	p.nonEmpty("server.addr", c.Server.Addr)
	p.atLeast("server.workers", c.Server.Workers, 1)
	p.positive("server.shutdown_timeout", c.Server.ShutdownTimeout)
}

func validateScorer(p *problems, key string, s Scorer) {
	p.nonEmpty(key+".kind", s.Kind)
	p.url(key+".endpoint", s.Endpoint)
	p.nonEmpty(key+".model", s.Model)
	p.nonEmpty(key+".decision_question", s.DecisionQuestion)
	p.positive(key+".timeout", s.Timeout)
	p.atLeast(key+".max_retries", s.MaxRetries, 0)
	p.backoff(key+".backoff", s.Backoff)
}

func (c File) validateAgents(p *problems) {
	for i, a := range c.Agents.Attributes {
		p.nonEmpty(fmt.Sprintf("agents.attributes[%d]", i), a)
	}
	if len(c.Agents.Models) > 0 && len(c.Agents.Attributes) == 0 {
		p.add("agents.attributes must name at least one attribute when agents.models is set")
	}
	for agent, model := range c.Agents.Models {
		if strings.TrimSpace(agent) == "" {
			p.add("agents.models: an agent name must not be empty")
		}
		key := modelKey(agent)
		for field, v := range map[string]string{"endpoint": model.Endpoint, "model": model.Model} {
			if v == "" {
				p.add("%s.%s is required: an agent's model takes nothing from the scorer section", key, field)
			}
		}
		if model.Endpoint != "" && model.Model != "" {
			validateScorer(p, key, model)
		}
	}
}

func (c File) validateQueue(p *problems) {
	q := c.Queue
	if u, err := url.Parse(q.URL); q.NATS() && (err != nil || u.Host == "" || (u.Scheme != "nats" && u.Scheme != "tls")) {
		p.add("queue.url: %q is not a nats:// or tls:// URL (leave it empty to queue in process)", q.URL)
	}
	p.positive("queue.connect_timeout", q.ConnectTimeout)
	p.atLeast("queue.max_messages", int(q.MaxMessages), 1)
	p.atLeast("queue.max_deliver", q.MaxDeliver, 1)
	p.backoff("queue.redelivery_backoff", q.RedeliveryBackoff)
	p.positive("queue.dlq_max_age", q.DLQMaxAge)
	if q.Replicas < 1 || q.Replicas > 5 {
		p.add("queue.replicas must be between 1 and 5, got %d", q.Replicas)
	}
	if !q.NATS() && q.Replicas > 1 {
		p.add("queue.replicas must be 1 without queue.url: the embedded NATS server is a single server")
	}
	if !natsPrefix.MatchString(q.Prefix) {
		p.add("queue.prefix must be letters, digits, - or _, got %q", q.Prefix)
	}
	c.validateAckWait(p)
	if q.NATS() {
		validateNATSAuth(p, q.Auth)
		validateNATSTLS(p, q.TLS)
	}
}

func validateNATSAuth(p *problems, a NATSAuth) {
	if set := a.methods(); len(set) > 1 {
		p.add("queue.auth: set one way to authenticate, got %s", strings.Join(set, ", "))
	}
	if (a.User == "") != (a.PasswordEnv == "") {
		p.add("queue.auth.user and queue.auth.password_env go together")
	}
	p.readable("queue.auth.credentials_file", a.CredentialsFile)
	p.readable("queue.auth.nkey_seed_file", a.NkeySeedFile)
}

func validateNATSTLS(p *problems, t NATSTLS) {
	if (t.CertFile == "") != (t.KeyFile == "") {
		p.add("queue.tls.cert_file and queue.tls.key_file go together")
	}
	p.readable("queue.tls.ca_file", t.CAFile)
	p.readable("queue.tls.cert_file", t.CertFile)
	p.readable("queue.tls.key_file", t.KeyFile)
}

func (c File) validateAckWait(p *problems) {
	worst, key := c.Scorer.Timeout, "scorer.timeout"
	for agent, model := range c.Agents.Models {
		if model.Timeout > worst {
			worst, key = model.Timeout, modelKey(agent)+".timeout"
		}
	}
	if c.Queue.AckWait <= worst {
		p.add("queue.ack_wait (%s) must exceed the longest scoring call (%s, %s)", c.Queue.AckWait, worst, key)
	}
}

func (c File) validateOTLP(p *problems) {
	o := c.OTLP
	if !o.Enabled() {
		return
	}
	c.validateOTLPAddrs(p)
	p.positive("otlp.idle_timeout", o.IdleTimeout)
	if o.MaxWait < o.IdleTimeout {
		p.add("otlp.max_wait (%s) must be at least otlp.idle_timeout (%s)", o.MaxWait, o.IdleTimeout)
	}
	// Spans must outlive the wait for their trace to complete.
	if o.SpanMaxAge <= o.MaxWait {
		p.add("otlp.span_max_age (%s) must exceed otlp.max_wait (%s)", o.SpanMaxAge, o.MaxWait)
	}
	p.atLeast("otlp.assemblers", o.Assemblers, 1)
}

func (c File) validateOTLPAddrs(p *problems) {
	o := c.OTLP
	if o.GRPCAddr != "" && o.GRPCAddr == o.HTTPAddr {
		p.add("otlp.grpc_addr and otlp.http_addr must differ, both are %q", o.GRPCAddr)
	}
	for name, addr := range map[string]string{"otlp.grpc_addr": o.GRPCAddr, "otlp.http_addr": o.HTTPAddr} {
		if addr != "" && addr == c.Server.Addr {
			p.add("%s must differ from server.addr, both are %q", name, addr)
		}
	}
}

func (c File) validateObservability(p *problems) {
	c.validateErrorSinks(p)
	m := c.Observability.Metrics
	if !m.Enabled() {
		return
	}
	p.url("observability.metrics.endpoint", m.Endpoint)
	p.positive("observability.metrics.interval", m.Interval)
	p.headerNames("observability.metrics.headers", m.Headers)
}

func (c File) validateErrorSinks(p *problems) {
	for i, sink := range c.Observability.ErrorSinks {
		switch sink.Kind {
		case SinkStdout:
		case SinkFile:
			if sink.Path == "" {
				p.add("observability.error_sinks[%d]: a file sink needs a path", i)
			}
		case SinkOTLP:
			p.url(fmt.Sprintf("observability.error_sinks[%d].endpoint", i), sink.Endpoint)
			p.headerNames(fmt.Sprintf("observability.error_sinks[%d].headers", i), sink.Headers)
		default:
			p.add("observability.error_sinks[%d].kind must be %q, %q or %q, got %q", i, SinkStdout, SinkFile, SinkOTLP, sink.Kind)
		}
	}
}

func (c File) validatePolicy(p *problems) {
	p.unit("policy.keep_threshold", c.Policy.KeepThreshold)
	p.unit("policy.random_floor", c.Policy.RandomFloor)
}

// Safe as a NATS subject token and stream suffix.
var (
	exporterName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	natsPrefix   = exporterName
)

func (c File) validateExporters(p *problems) {
	seen := map[string]bool{}
	for i, e := range c.Exporters {
		key := fmt.Sprintf("exporters[%d]", i)
		stream := exportnats.StreamName("", e.Name)
		switch {
		case !exporterName.MatchString(e.Name):
			p.add("%s.name %q must be letters, digits, '-' and '_' only", key, e.Name)
		case seen[stream]:
			p.add("%s.name %q is used twice (names differing only in case or '-'/'_' count as the same)", key, e.Name)
		}
		seen[stream] = true
		validateExporter(p, key, e)
	}
}

func validateExporter(p *problems, key string, e Exporter) {
	p.url(key+".endpoint", e.Endpoint)
	p.atLeast(key+".queue_size", e.QueueSize, 0)
	p.atLeast(key+".concurrency", e.Concurrency, 0)
	p.atLeast(key+".max_deliver", e.MaxDeliver, 0)
	p.backoff(key+".backoff", e.Backoff)
	p.headerNames(key+".headers", e.Headers)
}

type problems struct{ errs []error }

func (p *problems) add(format string, args ...any) {
	p.errs = append(p.errs, fmt.Errorf(format, args...))
}

func (p *problems) err() error { return errors.Join(p.errs...) }

func (p *problems) nonEmpty(key, v string) {
	if v == "" {
		p.add("%s must not be empty", key)
	}
}

func (p *problems) positive(key string, d time.Duration) {
	if d <= 0 {
		p.add("%s must be positive, got %s", key, d)
	}
}

func (p *problems) atLeast(key string, v, least int) {
	if v >= least {
		return
	}
	if least == 0 {
		p.add("%s must not be negative, got %d", key, v)
		return
	}
	p.add("%s must be at least %d, got %d", key, least, v)
}

// NaN fails too.
func (p *problems) unit(key string, v float64) {
	if !(v >= 0 && v <= 1) {
		p.add("%s must be in [0, 1], got %v", key, v)
	}
}

func (p *problems) url(key, v string) {
	if err := checkURL(v); err != nil {
		p.add("%s: %w", key, err)
	}
}

func (p *problems) backoff(key string, ds []time.Duration) {
	for i, d := range ds {
		if d < 0 {
			p.add("%s[%d] must not be negative, got %s", key, i, d)
		}
	}
}

func (p *problems) headerNames(key string, headers map[string]string) {
	for k := range headers {
		if strings.TrimSpace(k) == "" {
			p.add("%s: a header name must not be empty", key)
		}
	}
}

// A wrong path fails at start-up rather than at the first reconnect.
func (p *problems) readable(key, path string) {
	if path == "" {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		p.add("%s: %w", key, err)
		return
	}
	f.Close()
}
