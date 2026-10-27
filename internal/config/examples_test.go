package config

import (
	"path/filepath"
	"testing"
)

const examplesDir = "../../examples"

// Every example config in examples/ must load and validate as written,
// with the manifests it names.
func TestExampleConfigsLoad(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(examplesDir, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatalf("no example configs in %s", examplesDir)
	}
	t.Setenv(DefaultJevAPIKeyEnv, "test-key")
	t.Setenv("LANGFUSE_OTLP_HEADERS", "Authorization=Basic%20dGVzdDp0ZXN0")
	t.Setenv("PHOENIX_OTLP_HEADERS", "api_key=test")
	t.Setenv("NATS_TOKEN", "test")
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			if _, err := Load(path); err != nil {
				t.Error(err)
			}
		})
	}
}

// Every manifest in examples/manifests is valid for the default question.
func TestExampleManifests(t *testing.T) {
	paths, _ := filepath.Glob(filepath.Join(examplesDir, "manifests", "*.json"))
	if len(paths) == 0 {
		t.Fatal("no example manifests")
	}
	for _, path := range paths {
		if _, err := LoadManifest(path, "answer_wrong"); err != nil {
			t.Error(err)
		}
	}
}
