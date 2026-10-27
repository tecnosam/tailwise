package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeNATS writes a config using NATS with the given queue keys, and a
// creds file next to it.
func writeNATS(t *testing.T, queue string) string {
	t.Helper()
	path := write(t, "scorer:\n  manifest: manifest.json\nqueue:\n  url: nats://nats:4222\n"+queue, testManifest)
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "engine.creds"), []byte("creds"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNATSAuthAndTLS(t *testing.T) {
	t.Setenv("NATS_PASSWORD", "pw")
	cfg, err := Load(writeNATS(t, `
  replicas: 3
  auth:
    user: tailwise
    password_env: NATS_PASSWORD
  tls:
    ca_file: engine.creds
`))
	if err != nil {
		t.Fatal(err)
	}
	b := cfg.Broker
	if b.Auth.User != "tailwise" || b.Auth.Password != "pw" {
		t.Errorf("auth = %+v", b.Auth)
	}
	if !filepath.IsAbs(b.TLS.CAFile) || filepath.Base(b.TLS.CAFile) != "engine.creds" {
		t.Errorf("ca_file = %q, want resolved against the config's directory", b.TLS.CAFile)
	}
	if b.Replicas != 3 {
		t.Error("replicas did not reach the broker, which applies them to every stream")
	}
}

func TestNATSConfigErrors(t *testing.T) {
	t.Setenv("NATS_TOKEN", "x")
	cases := []struct{ name, queue, want string }{
		{"replicas", "  replicas: 7\n", "queue.replicas must be between 1 and 5"},
		{"two methods", "  auth:\n    credentials_file: engine.creds\n    token_env: NATS_TOKEN\n", "set one way to authenticate, got credentials_file, token_env"},
		{"user alone", "  auth:\n    user: tailwise\n", "user and queue.auth.password_env go together"},
		{"secret unset", "  auth:\n    token_env: NO_SUCH_VAR\n", "queue.auth.token_env: $NO_SUCH_VAR is not set"},
		{"missing file", "  auth:\n    credentials_file: nope.creds\n", "queue.auth.credentials_file"},
		{"cert alone", "  tls:\n    cert_file: engine.creds\n", "cert_file and queue.tls.key_file go together"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeNATS(t, tc.queue))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

// A secret never shows in an error.
func TestNATSSecretNotInErrors(t *testing.T) {
	t.Setenv("NATS_TOKEN", "t0ken-value")
	_, err := Load(writeNATS(t, "  replicas: 9\n  auth:\n    token_env: NATS_TOKEN\n"))
	if err == nil || strings.Contains(err.Error(), "t0ken-value") {
		t.Errorf("error = %v", err)
	}
}

// With the embedded server the external connection settings are not checked.
func TestNATSSettingsIgnoredWhenEmbedded(t *testing.T) {
	_, err := Load(write(t, "scorer:\n  manifest: manifest.json\nqueue:\n  auth:\n    user: tailwise\n", testManifest))
	if err != nil {
		t.Errorf("error = %v", err)
	}
}

// The embedded NATS server is a single server, so it cannot keep replicas.
func TestEmbeddedNATSNeedsOneReplica(t *testing.T) {
	_, err := Load(write(t, "scorer:\n  manifest: manifest.json\nqueue:\n  replicas: 3\n", testManifest))
	if err == nil || !strings.Contains(err.Error(), "queue.replicas must be 1 without queue.url") {
		t.Errorf("error = %v, want the embedded replicas rule", err)
	}
	if _, err := Load(write(t, "scorer:\n  manifest: manifest.json\nqueue:\n  url: nats://nats:4222\n  replicas: 3\n", testManifest)); err != nil {
		t.Errorf("replicas with an external server: %v", err)
	}
}

// A relative store_dir resolves against the config file's directory.
func TestStoreDirIsResolved(t *testing.T) {
	path := write(t, "scorer:\n  manifest: manifest.json\nqueue:\n  store_dir: data/nats\n", testManifest)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(filepath.Dir(path), "data/nats"); cfg.Broker.StoreDir != want {
		t.Errorf("store dir = %q, want %q", cfg.Broker.StoreDir, want)
	}
}
