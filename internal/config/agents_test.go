package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tecnosam/tailwise/internal/trace"
)

const supportManifest = `{
  "model": "support",
  "questions": {"answer_wrong": {"type": "noul", "instructions": "is the support answer wrong?"}}
}`

// writeAgents writes a config with one agent model and its manifest next to it.
func writeAgents(t *testing.T, agents string) string {
	t.Helper()
	path := write(t, "scorer:\n  manifest: manifest.json\n  timeout: 2s\n"+agents, testManifest)
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "support.json"), []byte(supportManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// An agent's model is complete on its own: what it leaves out gets the
// engine's built-in defaults, never the scorer section's values.
func TestAgentModelTakesNothingFromScorer(t *testing.T) {
	cfg, err := Load(writeAgents(t, `
agents:
  models:
    support-bot:
      endpoint: http://kev-support:8010/v1/systemone
      model: kev-support
      manifest: support.json
`))
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.Models.ByAgent[trace.AgentName("support-bot")]
	if m.Client.Endpoint != "http://kev-support:8010/v1/systemone" || m.Client.Model != "kev-support" ||
		!strings.Contains(string(m.Client.Questions), "support answer") {
		t.Errorf("own keys and manifest = %+v", m)
	}
	// scorer.timeout is 2s in this file; the agent's model keeps the built-in 10s.
	if def := DefaultModel(); m.Client.Timeout != def.Timeout || m.Retry.Attempts != def.MaxRetries+1 || m.Client.DecisionQuestion != def.DecisionQuestion {
		t.Errorf("optional keys = %+v, want the built-in defaults", m)
	}
	if d := cfg.Models.Default; d.Client.Timeout != 2*time.Second || d.Client.Model != JevModel || !fromTestManifest(d) {
		t.Errorf("default model = %+v", d)
	}
}

// Environment overrides change the scorer section only, and an agent's
// model never picks them up.
func TestAgentModelIgnoresScorerEnvironment(t *testing.T) {
	t.Setenv(EnvScorerEndpoint, "http://host.docker.internal:8010/v1/systemone")
	cfg, err := Load(writeAgents(t, `
agents:
  models:
    support-bot: {endpoint: "http://kev-support:8010/v1/systemone", model: kev-support, manifest: support.json}
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Models.ByAgent["support-bot"].Client.Endpoint; got != "http://kev-support:8010/v1/systemone" {
		t.Errorf("agent endpoint = %q", got)
	}
}

func TestAgentModelErrors(t *testing.T) {
	const ok = "      endpoint: http://kev:8010/v1/systemone\n      model: kev-a\n"
	cases := []struct {
		name, agents, want string
	}{
		{"no endpoint", "agents:\n  models:\n    a:\n      model: kev-a\n      manifest: support.json\n", "agents.models.a.endpoint is required"},
		{"no model", "agents:\n  models:\n    a:\n      endpoint: http://kev:8010/v1/systemone\n      manifest: support.json\n", "agents.models.a.model is required"},
		{"unknown key", "agents:\n  models:\n    a:\n" + ok + "      modle: x\n", "field modle not found"},
		{"bad value", "agents:\n  models:\n    a:\n" + ok + "      manifest: support.json\n      timeout: 0s\n", "agents.models.a.timeout must be positive"},
		{"missing manifest file", "agents:\n  models:\n    a:\n" + ok + "      manifest: nope.json\n", "nope.json"},
		{"no attributes", "agents:\n  attributes: []\n  models:\n    a:\n" + ok + "      manifest: support.json\n", "agents.attributes must name at least one"},
		{"slow model", "agents:\n  models:\n    a:\n" + ok + "      manifest: support.json\n      timeout: 10m\n", "agents.models.a.timeout)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeAgents(t, tc.agents))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

// Each model reads its own API key.
func TestAgentModelAPIKey(t *testing.T) {
	t.Setenv("SUPPORT_KEY", "s3cret")
	cfg, err := Load(writeAgents(t, `
agents:
  models:
    support-bot:
      kind: jev
      endpoint: https://api.typesafe.ai/v1/systemone
      model: jev-latest
      api_key_env: SUPPORT_KEY
      manifest: support.json
`))
	if err != nil {
		t.Fatal(err)
	}
	if agent, def := cfg.Models.ByAgent["support-bot"].Client.APIKey, cfg.Models.Default.Client.APIKey; agent != "s3cret" || def != "test-key" {
		t.Errorf("keys: agent %q, default %q", agent, def)
	}
}

func TestMetricsConfig(t *testing.T) {
	t.Setenv("METRICS_HEADERS", "Authorization=Bearer%20abc")
	cfg, err := Load(write(t, `
scorer:
  manifest: manifest.json
observability:
  metrics:
    endpoint: http://collector:4318/v1/metrics
    headers_env: METRICS_HEADERS
    interval: 30s
`, testManifest))
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.Metrics
	if m.Endpoint != "http://collector:4318/v1/metrics" || m.Interval != 30*time.Second || m.Headers["Authorization"] != "Bearer abc" {
		t.Errorf("metrics = %+v", m)
	}

	if cfg := Default(); cfg.Observability.Metrics.Enabled() {
		t.Error("metrics are on by default")
	}
	_, err = Load(write(t, "scorer:\n  manifest: manifest.json\nobservability:\n  metrics:\n    endpoint: collector:4318\n", testManifest))
	if err == nil || !strings.Contains(err.Error(), "observability.metrics.endpoint") {
		t.Errorf("error = %v", err)
	}
}

// An agent's model may leave out its manifest too: it gets the built-in
// questions and its own warning.
func TestAgentModelWithoutAManifest(t *testing.T) {
	cfg, err := Load(writeAgents(t, "agents:\n  models:\n    a:\n      endpoint: http://kev:8010/v1/systemone\n      model: kev-a\n"))
	if err != nil {
		t.Fatal(err)
	}
	if m := cfg.Models.ByAgent["a"]; !strings.Contains(string(m.Client.Questions), "why_wrong") {
		t.Errorf("agent model = %+v", m)
	}
	if !slices.ContainsFunc(cfg.Warnings, func(w string) bool { return strings.Contains(w, "agents.models.a has no manifest") }) {
		t.Errorf("warnings = %q", cfg.Warnings)
	}
}
