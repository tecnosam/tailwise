package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildInCode builds a File made in code, as a program embedding the engine
// would, with its manifest in a temp dir and the environment in env.
func buildInCode(t *testing.T, f File, env map[string]string) (*Engine, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(testManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if f.Scorer.Manifest == "" {
		f.Scorer.Manifest = "manifest.json"
	}
	if env == nil {
		env = map[string]string{DefaultJevAPIKeyEnv: "test-key"}
	}
	return Build(f, dir, func(k string) string { return env[k] })
}

// A program that embeds the engine configures it in code: no YAML file.
func TestBuildFromAFileMadeInCode(t *testing.T) {
	f := Default()
	f.Scorer.Endpoint = "http://kev.internal:8010/v1/systemone"
	f.Policy.KeepThreshold = 0.4
	got, err := buildInCode(t, f, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Models.Default.Client.Endpoint != f.Scorer.Endpoint || got.Policy.KeepThreshold != 0.4 {
		t.Errorf("settings changed on the way through: %+v", got.Models.Default.Client)
	}
	if !fromTestManifest(got.Models.Default) {
		t.Errorf("manifest not read: questions %s", got.Models.Default.Client.Questions)
	}
}

// A File made in code is validated like a file.
func TestBuildValidates(t *testing.T) {
	f := Default()
	f.Server.Workers = 0
	if _, err := buildInCode(t, f, nil); err == nil || !strings.Contains(err.Error(), "server.workers must be at least 1") {
		t.Errorf("error = %v", err)
	}
}

// The environment is injected: tests and embedding programs decide what it
// holds, and Build leaves the caller's File alone.
func TestBuildReadsTheInjectedEnvironment(t *testing.T) {
	f := Default()
	f.Scorer.APIKeyEnv = "MY_KEY"
	env := map[string]string{EnvScorerEndpoint: "http://from-env:8010/v1/systemone", EnvExportEndpoint: "http://env/v1/traces", "MY_KEY": "secret"}
	got, err := buildInCode(t, f, env)
	if err != nil {
		t.Fatal(err)
	}
	if c := got.Models.Default.Client; c.Endpoint != env[EnvScorerEndpoint] || c.APIKey != "secret" {
		t.Errorf("endpoint %q, key %q", c.Endpoint, c.APIKey)
	}
	if f.Scorer.Endpoint != Default().Scorer.Endpoint || f.Exporters[0].Endpoint != Default().Exporters[0].Endpoint {
		t.Error("Build changed the caller's File")
	}
}

// Every missing secret and invalid value is reported at once.
func TestBuildReportsEveryProblem(t *testing.T) {
	f := Default()
	f.Scorer.Kind = ScorerJev
	f.Server.Workers = 0
	f.Exporters[0].HeadersEnv = "NO_HEADERS"
	_, err := buildInCode(t, f, map[string]string{})
	for _, want := range []string{"server.workers", "$JEV_API_KEY is not set", "$NO_HEADERS is not set"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q", err, want)
		}
	}
}
