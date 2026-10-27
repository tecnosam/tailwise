// Package scorer calls a System One decision model (Jev by default) over
// /v1/systemone.
package scorer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"time"

	"github.com/tecnosam/tailwise/internal/fault"
	"github.com/tecnosam/tailwise/internal/pipeline"
	"github.com/tecnosam/tailwise/internal/trace"
)

const (
	maxResponseBytes = 1 << 20
	maxErrorBody     = 300
	probePath        = "/v1/models"
)

type Config struct {
	Endpoint         string
	Model            string
	Questions        json.RawMessage
	DecisionQuestion string
	// Reasons maps each option of the reason question to the plain-English
	// sentence that becomes the decision's reason.
	ReasonQuestion string
	Reasons        map[string]string
	APIKey         string
	Timeout        time.Duration

	HTTPClient *http.Client
}

type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("model returned HTTP %d: %s", e.Code, e.Body)
}

type Client struct {
	cfg      Config
	probeURL string
}

func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("scorer: invalid endpoint %q", cfg.Endpoint)
	}
	if !json.Valid(cfg.Questions) {
		return nil, errors.New("scorer: questions must be valid JSON")
	}
	if cfg.DecisionQuestion == "" {
		return nil, errors.New("scorer: decision question is required")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{}
	}
	probe := url.URL{Scheme: u.Scheme, Host: u.Host, Path: probePath}
	return &Client{cfg: cfg, probeURL: probe.String()}, nil
}

type request struct {
	Model     string          `json:"model"`
	State     string          `json:"state"`
	Questions json.RawMessage `json:"questions"`
}

type response struct {
	Model            string                     `json:"model"`
	Answers          map[string]json.RawMessage `json:"answers"`
	LatencyMS        *float64                   `json:"latency_ms"`
	EvaluationTimeMS *float64                   `json:"evaluation_time_ms"`
}

type noulAnswer struct {
	Type string   `json:"type"`
	Noul *float64 `json:"noul"`
}

const opScore fault.Op = "score trace"

// Score makes one call: retrying is up to the caller (the queue redelivers,
// the synchronous endpoint retries in the engine).
func (c *Client) Score(ctx context.Context, state trace.State) (pipeline.Score, error) {
	body, err := json.Marshal(request{Model: c.cfg.Model, State: string(state), Questions: c.cfg.Questions})
	if err != nil {
		return pipeline.Score{}, fault.Internal(fault.Scorer, "encode request", err)
	}
	score, err := c.attempt(ctx, body)
	if err != nil {
		return pipeline.Score{}, classify(err)
	}
	return score, nil
}

func (c *Client) attempt(ctx context.Context, body []byte) (pipeline.Score, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	req, err := c.newRequest(ctx, body)
	if err != nil {
		return pipeline.Score{}, err
	}
	start := time.Now()
	raw, err := c.send(req)
	if err != nil {
		return pipeline.Score{}, err
	}
	return c.parse(raw, time.Since(start))
}

func (c *Client) newRequest(ctx context.Context, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	c.authorize(req)
	return req, nil
}

// send returns a 2xx body; any other status is a *StatusError.
func (c *Client) send(req *http.Request) ([]byte, error) {
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &StatusError{Code: resp.StatusCode, Body: truncate(string(raw), maxErrorBody)}
	}
	return raw, nil
}

func (c *Client) parse(raw []byte, elapsed time.Duration) (pipeline.Score, error) {
	var out response
	if err := json.Unmarshal(raw, &out); err != nil {
		return pipeline.Score{}, fmt.Errorf("decode model response: %w", err)
	}
	p, err := c.decisionAnswer(out)
	if err != nil {
		return pipeline.Score{}, err
	}
	score := pipeline.Score{P: p, Model: c.modelName(out), Latency: reportedLatency(out, elapsed)}
	score.Cause, score.Explanation = c.reasonAnswer(out)
	return score, nil
}

// A missing or malformed reason answer gives none: the decision stands on the
// probability alone.
func (c *Client) reasonAnswer(out response) (cause, explanation string) {
	raw, ok := out.Answers[c.cfg.ReasonQuestion]
	if c.cfg.ReasonQuestion == "" || !ok {
		return "", ""
	}
	var ans struct {
		Choice string `json:"choice"`
	}
	if json.Unmarshal(raw, &ans) != nil || ans.Choice == "" {
		return "", ""
	}
	explanation, ok = c.cfg.Reasons[ans.Choice]
	if !ok {
		return "", "" // not one of the options asked: do not repeat it
	}
	return ans.Choice, explanation
}

func (c *Client) decisionAnswer(out response) (pipeline.Probability, error) {
	q := c.cfg.DecisionQuestion
	rawAnswer, ok := out.Answers[q]
	if !ok {
		return 0, fmt.Errorf("model response has no answer for %q", q)
	}
	var ans noulAnswer
	if err := json.Unmarshal(rawAnswer, &ans); err != nil {
		return 0, fmt.Errorf("decode answer %q: %w", q, err)
	}
	if ans.Noul == nil {
		return 0, fmt.Errorf("answer %q has no noul value", q)
	}
	if p := *ans.Noul; math.IsNaN(p) || p < 0 || p > 1 {
		return 0, fmt.Errorf("answer %q has noul %v outside [0, 1]", q, p)
	}
	return pipeline.Probability(*ans.Noul), nil
}

func (c *Client) modelName(out response) pipeline.ModelName {
	if out.Model != "" {
		return pipeline.ModelName(out.Model)
	}
	return pipeline.ModelName(c.cfg.Model)
}

func reportedLatency(out response, measured time.Duration) time.Duration {
	for _, ms := range []*float64{out.LatencyMS, out.EvaluationTimeMS} {
		if ms != nil {
			return time.Duration(*ms * float64(time.Millisecond))
		}
	}
	return measured
}

// Ready also checks the key: 401 and 403 mean it is wrong or missing. Any
// other answer below 500 means the server is up, even without /v1/models.
func (c *Client) Ready(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.probeURL, nil)
	if err != nil {
		return err
	}
	c.authorize(req)
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%s refused the API key (HTTP %d): check it is set and valid", c.probeURL, resp.StatusCode)
	case resp.StatusCode >= 500:
		return &StatusError{Code: resp.StatusCode, Body: "probe " + c.probeURL}
	}
	return nil
}

func (c *Client) authorize(req *http.Request) {
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
}

// classify rejects only a 400, 413 or 422, where the model refuses this
// particular request. Everything else (unreachable, timeouts, 5xx, 401, 404,
// 429, an unparseable answer) is the system's fault and worth retrying later.
func classify(err error) *fault.Error {
	var status *StatusError
	switch {
	case errors.As(err, &status) && rejectedStatus(status.Code):
		return fault.Rejected(fault.Scorer, opScore, err)
	case errors.Is(err, context.DeadlineExceeded):
		return fault.Timeout(fault.Scorer, opScore, err)
	}
	return fault.Unavailable(fault.Scorer, opScore, err)
}

func rejectedStatus(code int) bool {
	switch code {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
