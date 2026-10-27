package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tecnosam/tailwise/internal/scorer"
)

const testManifest = `{
  "model": "m",
  "questions": {
    "answer_wrong": {"type": "noul", "instructions": "wrong?"},
    "quality": {"type": "score", "instructions": "how good?", "criteria": ["a", "b"]}
  },
  "temperatures": {"answer_wrong": 0.86},
  "extra": {"ignored": true}
}`

// fromTestManifest tells testManifest's questions from the built-in ones.
func fromTestManifest(m scorer.ModelConfig) bool {
	return strings.Contains(string(m.Client.Questions), `"quality"`)
}

// write puts a config and manifest in a temp dir and returns the config path.
func write(t *testing.T, yaml, manifest string) string {
	t.Helper()
	dir := t.TempDir()
	if manifest != "" {
		if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "tailwise.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadDefaults(t *testing.T) {
	cfg, err := Read(write(t, "scorer:\n  manifest: manifest.json\n", testManifest))
	if err != nil {
		t.Fatal(err)
	}
	want := Default()
	if cfg.Server != want.Server {
		t.Errorf("server = %+v, want %+v", cfg.Server, want.Server)
	}
	if q := cfg.Queue; q.URL != "" || q.NATS() || q.Prefix != "tailwise" || q.MaxDeliver != 5 ||
		q.AckWait != 2*time.Minute || len(q.RedeliveryBackoff) != 4 {
		t.Errorf("queue defaults not applied: %+v", q)
	}
	if cfg.Policy != want.Policy {
		t.Errorf("policy defaults not applied: %+v", cfg)
	}
	if e := cfg.Exporters; len(e) != 1 || e[0].Name != "phoenix" || e[0].Endpoint != want.Exporters[0].Endpoint {
		t.Errorf("exporter defaults not applied: %+v", e)
	}
	if s := cfg.Scorer; s.Kind != ScorerJev || s.Endpoint != JevEndpoint || s.Model != JevModel ||
		s.Timeout != 30*time.Second || s.MaxRetries != 3 || len(s.Backoff) != 3 {
		t.Errorf("scorer defaults not applied: %+v", cfg.Scorer)
	}
}

// Load reads the manifest the file names into the default model.
func TestLoadReadsTheManifest(t *testing.T) {
	cfg, err := Load(write(t, "scorer:\n  manifest: manifest.json\n", testManifest))
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.Models.Default
	// Compacted, with the manifest's key order preserved.
	wantQ := `{"answer_wrong":{"type":"noul","instructions":"wrong?"},"quality":{"type":"score","instructions":"how good?","criteria":["a","b"]}}`
	if string(m.Client.Questions) != wantQ {
		t.Errorf("questions = %s\nwant %s", m.Client.Questions, wantQ)
	}
	// scorer.max_retries is the synchronous path's retries.
	if m.Retry.Attempts != 4 || len(m.Retry.Backoff) != 3 || !m.Retry.Jitter {
		t.Errorf("retry = %+v, want 4 attempts with the default backoff, jittered", m.Retry)
	}
}

func TestLoadOverrides(t *testing.T) {
	yaml := `
server:
  addr: ":9999"
  workers: 2
queue:
  url: nats://nats:4222
  prefix: staging
  max_messages: 7
  ack_wait: 45s
  max_deliver: 2
  redelivery_backoff: [1s]
  dlq_max_age: 1h
scorer:
  endpoint: http://kev:1/v1/systemone
  manifest: manifest.json
  timeout: 250ms
  max_retries: 0
  backoff: [10ms, 1s]
policy:
  keep_threshold: 0
  random_floor: 1
exporters:
  - name: phoenix
    endpoint: http://phoenix:2/v1/traces
    resource_attributes:
      openinference.project.name: p
    queue_size: 8
    concurrency: 2
`
	cfg, err := Load(write(t, yaml, testManifest))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Addr != ":9999" || cfg.Server.Workers != 2 {
		t.Errorf("server = %+v", cfg.Server)
	}
	q := cfg.Queue
	if cfg.Broker.URL != "nats://nats:4222" || q.Stream != "STAGING_TRACES" || q.Subject != "staging.traces" || q.Consumer != "staging-workers" ||
		q.MaxMessages != 7 || q.AckWait != 45*time.Second || q.Redelivery.Attempts != 2 ||
		len(q.Redelivery.Backoff) != 1 || q.DLQStream != "STAGING_TRACES_DLQ" || q.DLQSubject != "staging.traces.dlq" || q.DLQMaxAge != time.Hour {
		t.Errorf("queue = %+v", q)
	}
	m := cfg.Models.Default
	if m.Client.Timeout != 250*time.Millisecond || m.Retry.Attempts != 1 {
		t.Errorf("model = %+v", m)
	}
	if got := m.Retry.Backoff; len(got) != 2 || got[0] != 10*time.Millisecond || got[1] != time.Second {
		t.Errorf("backoff = %v", got)
	}
	// Explicit zero must survive, not be replaced by the default.
	if cfg.Policy.KeepThreshold != 0 || cfg.Policy.RandomFloor != 1 {
		t.Errorf("policy = %+v", cfg.Policy)
	}
	if e := cfg.Exporters; len(e) != 1 || e[0].Endpoint != "http://phoenix:2/v1/traces" ||
		e[0].ResourceAttributes["openinference.project.name"] != "p" || e[0].QueueSize != 8 || e[0].Concurrency != 2 {
		t.Errorf("exporters = %+v", e)
	}
}

func TestEnvOverrides(t *testing.T) {
	t.Setenv(EnvScorerEndpoint, "http://env-kev:8010/v1/systemone")
	t.Setenv(EnvExportEndpoint, "http://env-phoenix:6006/v1/traces")
	t.Setenv(EnvNATSURL, "nats://env-nats:4222")
	path := write(t, "scorer:\n  manifest: does-not-exist.json\n  endpoint: http://file:1/x\n", testManifest)
	t.Setenv(EnvManifest, filepath.Join(filepath.Dir(path), "manifest.json"))
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Broker.URL != "nats://env-nats:4222" {
		t.Errorf("nats url = %q", cfg.Broker.URL)
	}
	if !fromTestManifest(cfg.Models.Default) {
		t.Errorf("manifest from %s was not loaded", EnvManifest)
	}
	if got := cfg.Models.Default.Client.Endpoint; got != "http://env-kev:8010/v1/systemone" {
		t.Errorf("scorer endpoint = %q", got)
	}
	if cfg.Exporters[0].Endpoint != "http://env-phoenix:6006/v1/traces" {
		t.Errorf("export endpoint = %q", cfg.Exporters[0].Endpoint)
	}
}

func TestLoadErrors(t *testing.T) {
	base := "scorer:\n  manifest: manifest.json\n"
	tests := []struct {
		name     string
		yaml     string
		manifest string
		wantErr  string
	}{
		{"threshold above 1", base + "policy:\n  keep_threshold: 1.5\n", testManifest, "policy.keep_threshold must be in [0, 1]"},
		{"negative floor", base + "policy:\n  random_floor: -0.1\n", testManifest, "policy.random_floor must be in [0, 1]"},
		{"removed alert section", base + "alert:\n  severity_high: 0.8\n", testManifest, "alert"},
		{"bad duration", "scorer:\n  manifest: manifest.json\n  timeout: soon\n", testManifest, "soon"},
		{"bad backoff", "scorer:\n  manifest: manifest.json\n  backoff: [1s, later]\n", testManifest, "later"},
		{"zero timeout", "scorer:\n  manifest: manifest.json\n  timeout: 0s\n", testManifest, "scorer.timeout must be positive"},
		{"zero workers", base + "server:\n  workers: 0\n", testManifest, "server.workers must be at least 1"},
		{"removed queue_size", base + "server:\n  queue_size: 10\n", testManifest, "queue_size"},
		{"bad nats url", base + "queue:\n  url: http://nats:4222\n", testManifest, "queue.url"},
		{"prefix with a dot", base + "queue:\n  prefix: a.b\n", testManifest, "queue.prefix must be letters, digits"},
		{"empty prefix", base + "queue:\n  prefix: \"\"\n", testManifest, "queue.prefix must be letters, digits"},
		{"removed stream key", base + "queue:\n  stream: T\n", testManifest, "stream"},
		{"zero max_deliver", base + "queue:\n  max_deliver: 0\n", testManifest, "queue.max_deliver must be at least 1"},
		{"zero max_messages", base + "queue:\n  max_messages: 0\n", testManifest, "queue.max_messages must be at least 1"},
		{"otlp same ports", base + "otlp:\n  grpc_addr: \":4317\"\n  http_addr: \":4317\"\n", testManifest, "otlp.grpc_addr and otlp.http_addr must differ"},
		{"otlp port clashes with api", base + "otlp:\n  http_addr: \":8080\"\n", testManifest, "otlp.http_addr must differ from server.addr"},
		{"otlp max_wait below idle", base + "otlp:\n  idle_timeout: 1m\n  max_wait: 30s\n", testManifest, "otlp.max_wait (30s) must be at least otlp.idle_timeout (1m0s)"},
		{"otlp spans expire too soon", base + "otlp:\n  span_max_age: 1m\n", testManifest, "otlp.span_max_age (1m0s) must exceed otlp.max_wait (2m0s)"},
		{"otlp zero assemblers", base + "otlp:\n  assemblers: 0\n", testManifest, "otlp.assemblers must be at least 1"},
		{"ack_wait too short", base + "queue:\n  ack_wait: 5s\n", testManifest, "queue.ack_wait (5s) must exceed the longest scoring call (30s, scorer.timeout)"},
		{"bad endpoint", "scorer:\n  manifest: manifest.json\n  endpoint: localhost:8010\n", testManifest, "scorer.endpoint"},
		{"unknown key", base + "polcy:\n  keep_threshold: 0.5\n", testManifest, "polcy"},
		{"manifest file missing", base, "", "manifest:"},
		{"question missing", "scorer:\n  manifest: manifest.json\n  decision_question: nope\n", testManifest, `decision question "nope" not found`},
		{"question not noul", "scorer:\n  manifest: manifest.json\n  decision_question: quality\n", testManifest, `has type "score", want "noul"`},
		{"manifest not JSON", base, "{", "manifest"},
		{"no questions", base, `{"model": "m"}`, "must be a non-empty object"},
		{"questions is a list", base, `{"questions": []}`, "must be a non-empty object"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(write(t, tt.yaml, tt.manifest))
			if err == nil {
				t.Fatalf("expected an error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestJevScorerReadsItsKeyFromTheEnvironment(t *testing.T) {
	jev := "scorer:\n  kind: jev\n  manifest: manifest.json\n"

	t.Setenv(DefaultJevAPIKeyEnv, "")
	if _, err := Load(write(t, jev, testManifest)); err == nil || !strings.Contains(err.Error(), "$JEV_API_KEY is not set") {
		t.Fatalf("error = %v, want the missing JEV_API_KEY named", err)
	}

	t.Setenv(DefaultJevAPIKeyEnv, "secret")
	cfg, err := Load(write(t, jev, testManifest))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Models.Default.Client.APIKey; got != "secret" {
		t.Errorf("key %q, want the one in $%s", got, DefaultJevAPIKeyEnv)
	}

	t.Setenv("MY_JEV_KEY", "other-secret")
	cfg, err = Load(write(t, "scorer:\n  kind: jev\n  api_key_env: MY_JEV_KEY\n  manifest: manifest.json\n", testManifest))
	if err != nil || cfg.Models.Default.Client.APIKey != "other-secret" {
		t.Errorf("custom variable: error %v", err)
	}
}

// Another System One model (a self-hosted Kev, Laya, ...) needs no key
// unless api_key_env names one, and never picks up Jev's.
func TestOtherModelKindsNeedNoKey(t *testing.T) {
	cfg, err := Load(write(t, "scorer:\n  kind: kev\n  endpoint: http://kev:8010/v1/systemone\n  model: kev-latest\n", ""))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Models.Default.Client.APIKey; got != "" {
		t.Errorf("key %q: a kev scorer must not pick up a key it was not given", got)
	}
	if _, err := Load(write(t, "scorer:\n  kind: \"\"\n", "")); err == nil || !strings.Contains(err.Error(), "scorer.kind") {
		t.Errorf("empty kind: error = %v", err)
	}
}

// With no file at all, the engine runs on its built-in defaults: Jev, the
// built-in questions, and a warning that they are not written for the agent.
func TestLoadWithoutAFile(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.Models.Default
	if m.Client.Endpoint != JevEndpoint || m.Client.Model != JevModel || m.Client.APIKey != "test-key" {
		t.Errorf("default model = %+v", m.Client)
	}
	if len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0], "scorer has no manifest") ||
		!strings.Contains(cfg.Warnings[0], "built-in questions") {
		t.Errorf("warnings = %q, want one about the missing manifest", cfg.Warnings)
	}
	// The built-in questions ask why the answer is wrong.
	if m.Client.ReasonQuestion != "why_wrong" || m.Client.Reasons["contradicts_evidence"] != "The final answer contradicts the evidence the agent gathered." {
		t.Errorf("reason question %q, reasons %v", m.Client.ReasonQuestion, m.Client.Reasons)
	}
	t.Setenv(DefaultJevAPIKeyEnv, "")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "$JEV_API_KEY is not set") {
		t.Errorf("no key: error = %v", err)
	}
}

// A manifest file is used as given, with no warning.
func TestAManifestSilencesTheWarning(t *testing.T) {
	cfg, err := Load(write(t, "scorer:\n  manifest: manifest.json\n  reason_question: \"\"\n", testManifest))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Warnings) != 0 || !fromTestManifest(cfg.Models.Default) {
		t.Errorf("warnings %q, questions %s", cfg.Warnings, cfg.Models.Default.Client.Questions)
	}
	// The same manifest with the default reason question warns that it has none.
	cfg, err = Load(write(t, "scorer:\n  manifest: manifest.json\n", testManifest))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0], `no "why_wrong" question`) {
		t.Errorf("warnings = %q, want one about the missing reason question", cfg.Warnings)
	}
	// A decision question the built-in questions lack needs a manifest.
	if _, err := Load(write(t, "scorer:\n  decision_question: tone\n", "")); err == nil || !strings.Contains(err.Error(), "built-in manifest") {
		t.Errorf("unknown question without a manifest: error = %v", err)
	}
}

func TestMissingConfigFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("expected an error")
	}
}

func TestOTLPDefaultsAndOverrides(t *testing.T) {
	cfg, err := Load(write(t, "scorer:\n  manifest: manifest.json\n", testManifest))
	if err != nil {
		t.Fatal(err)
	}
	o, sp := cfg.OTLP, cfg.Queue.Spans
	if o.GRPCAddr != ":4317" || o.HTTPAddr != ":4318" || sp.IdleTimeout != 10*time.Second ||
		sp.MaxWait != 2*time.Minute || o.Assemblers != 4 || sp.Stream != "TAILWISE_SPANS" ||
		sp.PendingStream != "TAILWISE_PENDING" || sp.MaxAge != time.Hour || !o.Enabled() {
		t.Errorf("otlp defaults = %+v, spans %+v", o, sp)
	}

	t.Setenv(EnvOTLPGRPCAddr, ":14317")
	t.Setenv(EnvOTLPHTTPAddr, ":14318")
	cfg, err = Load(write(t, "scorer:\n  manifest: manifest.json\n", testManifest))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OTLP.GRPCAddr != ":14317" || cfg.OTLP.HTTPAddr != ":14318" {
		t.Errorf("otlp addresses = %q, %q; want the env overrides", cfg.OTLP.GRPCAddr, cfg.OTLP.HTTPAddr)
	}
}

// Empty addresses switch OTLP off, and then its other settings are not checked.
func TestOTLPDisabled(t *testing.T) {
	yaml := "scorer:\n  manifest: manifest.json\notlp:\n  grpc_addr: \"\"\n  http_addr: \"\"\n  assemblers: 0\n"
	cfg, err := Load(write(t, yaml, testManifest))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OTLP.Enabled() {
		t.Error("OTLP reported enabled with both addresses empty")
	}
}

// Every test runs with a Jev key, as a user of the default model must;
// a test about the key unsets it with t.Setenv.
func TestMain(m *testing.M) {
	os.Setenv(DefaultJevAPIKeyEnv, "test-key")
	os.Exit(m.Run())
}

// A reason question that is not a choice question is a config error.
func TestReasonQuestionMustBeAChoice(t *testing.T) {
	_, err := Load(write(t, "scorer:\n  manifest: manifest.json\n  reason_question: quality\n", testManifest))
	if err == nil || !strings.Contains(err.Error(), `reason question "quality" must be a choice question`) {
		t.Errorf("error = %v", err)
	}
}

// The default prefix gives the names earlier versions used, so an existing
// NATS server's streams are picked up as they are.
func TestNATSNamesFromThePrefix(t *testing.T) {
	cfg, err := Load(write(t, "scorer:\n  manifest: manifest.json\n", testManifest))
	if err != nil {
		t.Fatal(err)
	}
	q, s, e := cfg.Queue, cfg.Queue.Spans, cfg.ExportStreams
	got := []string{q.Stream, q.Subject, q.Consumer, q.DLQStream, q.DLQSubject,
		e.Stream, e.Subject, e.DLQStream, e.DLQSubject,
		s.Stream, s.Subject, s.PendingStream, s.PendingSubject}
	want := []string{"TAILWISE_TRACES", "tailwise.traces", "tailwise-workers", "TAILWISE_TRACES_DLQ", "tailwise.traces.dlq",
		"TAILWISE_EXPORTS", "tailwise.exports", "TAILWISE_EXPORTS_DLQ", "tailwise.exports_dlq",
		"TAILWISE_SPANS", "tailwise.spans", "TAILWISE_PENDING", "tailwise.pending"}
	if !slices.Equal(got, want) {
		t.Errorf("names = %q\nwant    %q", got, want)
	}
}
