package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"gopkg.in/yaml.v3"
)

// Load reads the YAML file at path and builds the engine's settings with the
// process's environment. An empty path means no file: the built-in defaults.
func Load(path string) (*Engine, error) {
	if path == "" {
		return Build(Default(), ".", os.Getenv)
	}
	f, err := Read(path)
	if err != nil {
		return nil, err
	}
	eng, err := Build(f, filepath.Dir(path), os.Getenv)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return eng, nil
}

// Read decodes strictly, so an unknown key is an error with its line number.
// Each agents.models entry is decoded over DefaultModel(), so a key it leaves
// out gets the built-in default, never the scorer section's value.
func Read(path string) (File, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return File{}, fmt.Errorf("config: %w", err)
	}
	f := Default()
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return File{}, fmt.Errorf("config %s: %w", path, err)
	}
	var doc struct {
		Agents struct {
			Models map[string]yaml.Node `yaml:"models"`
		} `yaml:"agents"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return File{}, fmt.Errorf("config %s: %w", path, err)
	}
	for agent, node := range doc.Agents.Models {
		model := DefaultModel()
		if err := node.Decode(&model); err != nil {
			return File{}, fmt.Errorf("config %s: agents.models.%s: %w", path, agent, err)
		}
		f.Agents.Models[agent] = model
	}
	return f, nil
}

// Build applies the environment overrides, resolves relative paths against
// dir, validates and reads every secret (reporting all problems at once), then
// loads the manifests. getenv is os.Getenv for Load, a map in tests.
func Build(f File, dir string, getenv func(string) string) (*Engine, error) {
	f = f.clone()
	applyEnv(&f, getenv)
	f.resolvePaths(dir)

	p := &problems{}
	f.validate(p)
	sec := readSecrets(f, getenv, p)
	if err := p.err(); err != nil {
		return nil, err
	}
	manifests, warnings, err := loadManifests(f)
	if err != nil {
		return nil, err
	}
	eng := f.engine(sec, manifests)
	eng.Warnings = warnings
	return eng, nil
}

func (f File) clone() File {
	f.Exporters = append([]Exporter(nil), f.Exporters...)
	f.Agents.Models = maps.Clone(f.Agents.Models)
	return f
}

func applyEnv(f *File, getenv func(string) string) {
	fields := map[string]*string{
		EnvScorerEndpoint: &f.Scorer.Endpoint,
		EnvNATSURL:        &f.Queue.URL,
		EnvManifest:       &f.Scorer.Manifest,
		EnvOTLPGRPCAddr:   &f.OTLP.GRPCAddr,
		EnvOTLPHTTPAddr:   &f.OTLP.HTTPAddr,
	}
	if len(f.Exporters) > 0 {
		fields[EnvExportEndpoint] = &f.Exporters[0].Endpoint
	}
	for env, field := range fields {
		if v := getenv(env); v != "" {
			*field = v
		}
	}
}

func (f *File) resolvePaths(dir string) {
	paths := []*string{
		&f.Scorer.Manifest, &f.Queue.StoreDir,
		&f.Queue.Auth.CredentialsFile, &f.Queue.Auth.NkeySeedFile,
		&f.Queue.TLS.CAFile, &f.Queue.TLS.CertFile, &f.Queue.TLS.KeyFile,
	}
	for _, path := range paths {
		if *path != "" && !filepath.IsAbs(*path) {
			*path = filepath.Join(dir, *path)
		}
	}
	for agent, m := range f.Agents.Models {
		if m.Manifest != "" && !filepath.IsAbs(m.Manifest) {
			m.Manifest = filepath.Join(dir, m.Manifest)
			f.Agents.Models[agent] = m
		}
	}
}

// secrets are read from the variables the file names; they never go in a File.
type secrets struct {
	apiKeys         map[string]string // by agent; "" is the default model
	natsPassword    string
	natsToken       string
	exporterHeaders []map[string]string
	metricsHeaders  map[string]string
	sinkHeaders     []map[string]string
}

func readSecrets(f File, getenv func(string) string, p *problems) secrets {
	sec := secrets{apiKeys: map[string]string{}}
	required := func(key, env string) string {
		if env == "" {
			return ""
		}
		v := getenv(env)
		if v == "" {
			p.add("%s: $%s is not set", key, env)
		}
		return v
	}
	apiKey := func(agent string, s Scorer) {
		if s.APIKeyEnv == "" && s.Kind == ScorerJev {
			sec.apiKeys[agent] = getenv(DefaultJevAPIKeyEnv)
			if sec.apiKeys[agent] == "" {
				p.add("%s: $%s is not set: Jev needs your API key (or name another variable in %s.api_key_env)",
					modelKey(agent), DefaultJevAPIKeyEnv, modelKey(agent))
			}
			return
		}
		sec.apiKeys[agent] = required(modelKey(agent)+".api_key_env", s.APIKeyEnv)
	}
	apiKey("", f.Scorer)
	for agent, m := range f.Agents.Models {
		apiKey(agent, m)
	}
	if f.Queue.NATS() {
		sec.natsPassword = required("queue.auth.password_env", f.Queue.Auth.PasswordEnv)
		sec.natsToken = required("queue.auth.token_env", f.Queue.Auth.TokenEnv)
	}
	headers := func(key string, inline map[string]string, env string) map[string]string {
		out := maps.Clone(inline)
		if out == nil {
			out = map[string]string{}
		}
		raw := required(key+".headers_env", env)
		if raw == "" {
			return out
		}
		fromEnv, err := parseHeaders(raw)
		if err != nil {
			p.add("%s.headers_env: $%s: %w", key, env, err)
			return out
		}
		maps.Copy(out, fromEnv)
		p.headerNames(key+".headers", out)
		return out
	}
	for i, e := range f.Exporters {
		sec.exporterHeaders = append(sec.exporterHeaders, headers(fmt.Sprintf("exporters[%d] (%s)", i, e.Name), e.Headers, e.HeadersEnv))
	}
	m := f.Observability.Metrics
	sec.metricsHeaders = headers("observability.metrics", m.Headers, m.HeadersEnv)
	for i, s := range f.Observability.ErrorSinks {
		sec.sinkHeaders = append(sec.sinkHeaders, headers(fmt.Sprintf("observability.error_sinks[%d]", i), s.Headers, s.HeadersEnv))
	}
	return sec
}

// A model with no manifest gets the built-in questions, with a warning saying
// so.
func loadManifests(f File) (map[string]*Manifest, []string, error) {
	out := make(map[string]*Manifest, len(f.Agents.Models)+1)
	var warnings []string
	load := func(agent string, s Scorer) error {
		key := modelKey(agent)
		var m *Manifest
		var err error
		if s.Manifest == "" {
			if m, err = BuiltinManifest(s.DecisionQuestion); err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			warnings = append(warnings, fmt.Sprintf("%s has no manifest: it is asked the built-in questions, "+
				"which are written for any agent, not yours. Set %s.manifest to questions written for your agent.", key, key))
		} else if m, err = LoadManifest(s.Manifest, s.DecisionQuestion); err != nil {
			return err
		}
		found, err := m.readReasons(s.ReasonQuestion)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		if !found {
			warnings = append(warnings, fmt.Sprintf("%s's manifest has no %q question, so decisions say only how "+
				"likely the answer is wrong, not why. Add the choice question, or set %s.reason_question to \"\".",
				key, s.ReasonQuestion, key))
		}
		out[agent] = m
		return nil
	}
	if err := load("", f.Scorer); err != nil {
		return nil, nil, err
	}
	for _, agent := range slices.Sorted(maps.Keys(f.Agents.Models)) {
		if err := load(agent, f.Agents.Models[agent]); err != nil {
			return nil, nil, err
		}
	}
	return out, warnings, nil
}

func modelKey(agent string) string {
	if agent == "" {
		return "scorer"
	}
	return "agents.models." + agent
}
