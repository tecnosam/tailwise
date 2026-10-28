// Package config reads the YAML file into a File and builds the Engine settings
// each component takes from it. tailwise.schema.yaml documents every key.
package config

import (
	"encoding/json"
	"slices"
	"time"
)

const (
	EnvScorerEndpoint = "TAILWISE_SCORER_ENDPOINT"
	EnvExportEndpoint = "TAILWISE_EXPORT_ENDPOINT"
	EnvNATSURL        = "TAILWISE_NATS_URL"
	EnvManifest       = "TAILWISE_MANIFEST"
	EnvOTLPGRPCAddr   = "TAILWISE_OTLP_GRPC_ADDR"
	EnvOTLPHTTPAddr   = "TAILWISE_OTLP_HTTP_ADDR"

	questionTypeNoul = "noul"

	// Any System One model speaks /v1/systemone; the kind only picks the default
	// API key variable.
	ScorerJev = "jev"

	DefaultJevAPIKeyEnv = "JEV_API_KEY"

	JevEndpoint = "https://api.typesafe.ai/v1/systemone"
	JevModel    = "jev-latest"

	SinkStdout = "stdout"
	SinkFile   = "file"
	SinkOTLP   = "otlp"
)

type File struct {
	Server    Server     `yaml:"server"`
	Queue     Queue      `yaml:"queue"`
	OTLP      OTLP       `yaml:"otlp"`
	Scorer    Scorer     `yaml:"scorer"`
	Agents    Agents     `yaml:"agents"`
	Policy    Policy     `yaml:"policy"`
	Exporters []Exporter `yaml:"exporters"`

	Observability Observability `yaml:"observability"`
}

type Agents struct {
	Attributes []string          `yaml:"attributes"`
	Models     map[string]Scorer `yaml:"models,omitempty"`
}

type Server struct {
	Addr            string        `yaml:"addr"`
	Workers         int           `yaml:"workers"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
}

type OTLP struct {
	GRPCAddr string `yaml:"grpc_addr"`
	HTTPAddr string `yaml:"http_addr"`

	IdleTimeout time.Duration `yaml:"idle_timeout"`
	MaxWait     time.Duration `yaml:"max_wait"`
	Assemblers  int           `yaml:"assemblers"`

	SpanMaxAge time.Duration `yaml:"span_max_age"`
}

func (o OTLP) Enabled() bool { return o.GRPCAddr != "" || o.HTTPAddr != "" }

type Queue struct {
	URL            string        `yaml:"url"`
	StoreDir       string        `yaml:"store_dir"`
	ConnectTimeout time.Duration `yaml:"connect_timeout"`

	// Prefix names every NATS stream, subject and consumer (see natsNames), so
	// two deployments can share one NATS server.
	Prefix      string `yaml:"prefix"`
	MaxMessages int64  `yaml:"max_messages"`

	AckWait           time.Duration   `yaml:"ack_wait"`
	MaxDeliver        int             `yaml:"max_deliver"`
	RedeliveryBackoff []time.Duration `yaml:"redelivery_backoff"`

	DLQMaxAge time.Duration `yaml:"dlq_max_age"`

	Replicas int      `yaml:"replicas"`
	Auth     NATSAuth `yaml:"auth"`
	TLS      NATSTLS  `yaml:"tls"`
}

type NATSAuth struct {
	CredentialsFile string `yaml:"credentials_file,omitempty"`
	NkeySeedFile    string `yaml:"nkey_seed_file,omitempty"`
	User            string `yaml:"user,omitempty"`
	PasswordEnv     string `yaml:"password_env,omitempty"`
	TokenEnv        string `yaml:"token_env,omitempty"`
}

func (a NATSAuth) methods() []string {
	var set []string
	for name, v := range map[string]string{
		"credentials_file": a.CredentialsFile, "nkey_seed_file": a.NkeySeedFile, "user": a.User, "token_env": a.TokenEnv,
	} {
		if v != "" {
			set = append(set, name)
		}
	}
	slices.Sort(set)
	return set
}

type NATSTLS struct {
	CAFile   string `yaml:"ca_file,omitempty"`
	CertFile string `yaml:"cert_file,omitempty"`
	KeyFile  string `yaml:"key_file,omitempty"`
}

func (q Queue) NATS() bool { return q.URL != "" }

type Scorer struct {
	Kind      string `yaml:"kind"`
	Endpoint  string `yaml:"endpoint"`
	Model     string `yaml:"model"`
	APIKeyEnv string `yaml:"api_key_env"`

	Manifest         string          `yaml:"manifest"`
	DecisionQuestion string          `yaml:"decision_question"`
	ReasonQuestion   string          `yaml:"reason_question"`
	Timeout          time.Duration   `yaml:"timeout"`
	MaxRetries       int             `yaml:"max_retries"`
	Backoff          []time.Duration `yaml:"backoff"`
}

type Policy struct {
	KeepThreshold float64 `yaml:"keep_threshold"`
	RandomFloor   float64 `yaml:"random_floor"`
}

type Exporter struct {
	Name               string            `yaml:"name"`
	Endpoint           string            `yaml:"endpoint"`
	Headers            map[string]string `yaml:"headers,omitempty"`
	HeadersEnv         string            `yaml:"headers_env,omitempty"`
	ResourceAttributes map[string]string `yaml:"resource_attributes,omitempty"`
	QueueSize          int               `yaml:"queue_size,omitempty"`
	Concurrency        int               `yaml:"concurrency,omitempty"`
	MaxDeliver         int               `yaml:"max_deliver,omitempty"`
	Backoff            []time.Duration   `yaml:"backoff,omitempty"`
}

const (
	DefaultExportQueueSize   = 1024
	DefaultExportConcurrency = 4
)

type Observability struct {
	ErrorSinks []Sink  `yaml:"error_sinks"`
	Metrics    Metrics `yaml:"metrics"`
}

type Metrics struct {
	Endpoint           string            `yaml:"endpoint,omitempty"`
	Headers            map[string]string `yaml:"headers,omitempty"`
	HeadersEnv         string            `yaml:"headers_env,omitempty"`
	Interval           time.Duration     `yaml:"interval"`
	ResourceAttributes map[string]string `yaml:"resource_attributes,omitempty"`
}

func (m Metrics) Enabled() bool { return m.Endpoint != "" }

type Sink struct {
	Kind       string            `yaml:"kind"`
	Path       string            `yaml:"path,omitempty"`
	Endpoint   string            `yaml:"endpoint,omitempty"`
	Headers    map[string]string `yaml:"headers,omitempty"`
	HeadersEnv string            `yaml:"headers_env,omitempty"`
}

type Manifest struct {
	Model string `json:"model"`
	// Questions is kept as written (compacted), so keys reach the model in order.
	Questions json.RawMessage `json:"questions"`
	// Decision, when set, combines the yes/no answers into p_keep; it stays in
	// the engine and is not sent to the model.
	Decision *ManifestDecision `json:"decision"`

	Reasons map[string]string `json:"-"`
}

// ManifestDecision weights each yes/no question's answer; see pipeline.Pool.
type ManifestDecision struct {
	Intercept float64            `json:"intercept"`
	Weights   map[string]float64 `json:"weights"`
}

// DefaultModel is what an agent's model starts from: the built-in scorer
// settings, minus the endpoint and model it must set itself.
func DefaultModel() Scorer {
	s := Default().Scorer
	s.Endpoint, s.Model = "", ""
	return s
}

// Default is the built-in configuration: what runs without a config file, and
// what a file's missing keys keep.
func Default() File {
	return File{
		Server: Server{
			Addr:            ":8080",
			Workers:         8,
			ShutdownTimeout: 30 * time.Second,
		},
		Queue: Queue{
			URL:            "",
			ConnectTimeout: 30 * time.Second,
			Prefix:         "tailwise",
			MaxMessages:    100000,
			AckWait:        2 * time.Minute,
			MaxDeliver:     5,
			RedeliveryBackoff: []time.Duration{
				5 * time.Second,
				30 * time.Second,
				2 * time.Minute,
				5 * time.Minute,
			},
			DLQMaxAge: 7 * 24 * time.Hour,
			Replicas:  1,
		},
		OTLP: OTLP{
			GRPCAddr:    ":4317",
			HTTPAddr:    ":4318",
			IdleTimeout: 10 * time.Second,
			MaxWait:     2 * time.Minute,
			Assemblers:  4,
			SpanMaxAge:  time.Hour,
		},
		Scorer: Scorer{
			Kind:             ScorerJev,
			Endpoint:         JevEndpoint,
			Model:            JevModel,
			DecisionQuestion: "answer_wrong",
			ReasonQuestion:   "why_wrong",
			Timeout:          30 * time.Second,
			MaxRetries:       3,
			Backoff: []time.Duration{
				time.Second,
				5 * time.Second,
				15 * time.Second,
			},
		},
		Agents: Agents{Attributes: []string{"agent.name", "service.name"}},
		Policy: Policy{KeepThreshold: 0.5, RandomFloor: 0.02},
		Exporters: []Exporter{{
			Name:               "phoenix",
			Endpoint:           "http://localhost:6006/v1/traces",
			ResourceAttributes: map[string]string{"openinference.project.name": "tailwise"},
		}},
		Observability: Observability{
			ErrorSinks: []Sink{{Kind: SinkStdout}},
			Metrics:    Metrics{Interval: 15 * time.Second},
		},
	}
}
