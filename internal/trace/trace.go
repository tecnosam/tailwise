// Package trace is the agent trace the engine receives and the state string
// built from it for the model.
package trace

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// AgentName picks the model that scores the trace; empty means the default.
type AgentName string

const MaxAgentLength = 128

type Trace struct {
	TraceID   string     `json:"trace_id,omitempty"`
	Agent     AgentName  `json:"agent,omitempty"`
	Question  string     `json:"question"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	Evidence  []string   `json:"evidence,omitempty"`
	// FinalAnswer must be present but may be empty: a run that ended without an
	// answer is still scored.
	FinalAnswer string `json:"final_answer"`

	// OTLP holds the original spans of a trace that came through the OTLP
	// receivers; a kept trace is forwarded as them instead of being rebuilt.
	OTLP []byte `json:"otlp_spans,omitempty"`
}

// Args and Status stay raw: the slice writes them as JSON and needs to know
// whether each was present.
type ToolCall struct {
	Name   string          `json:"name"`
	Args   json.RawMessage `json:"args,omitempty"`
	Status json.RawMessage `json:"status,omitempty"`
}

// StatusText is the string itself, "ok" when absent, or compact JSON.
func (c ToolCall) StatusText() string {
	if len(c.Status) == 0 {
		return "ok"
	}
	if trimmed := bytes.TrimSpace(c.Status); len(trimmed) > 0 && trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err == nil {
			return s
		}
	}
	return compact(c.Status)
}

// compact puts a JSON value on one line. Decode only accepts valid JSON, so
// the fallback is never reached for a decoded trace.
func compact(raw json.RawMessage) string {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return string(raw)
	}
	return b.String()
}

func Decode(r io.Reader) (Trace, error) {
	// The outer final_answer shadows Trace's, so a missing key can be told from an
	// empty answer.
	var in struct {
		Trace
		FinalAnswer *string `json:"final_answer"`
	}
	dec := json.NewDecoder(r)
	if err := dec.Decode(&in); err != nil {
		return Trace{}, fmt.Errorf("invalid JSON: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Trace{}, errors.New("invalid JSON: unexpected data after the trace object")
	}
	t := in.Trace
	if in.FinalAnswer != nil {
		t.FinalAnswer = *in.FinalAnswer
	}
	if err := t.Validate(); err != nil {
		return Trace{}, err
	}
	if in.FinalAnswer == nil {
		return Trace{}, errors.New("final_answer is required")
	}
	return t, nil
}

func (t Trace) Validate() error {
	if t.Question == "" {
		return errors.New("question is required")
	}
	if len(t.Agent) > MaxAgentLength {
		return fmt.Errorf("agent is longer than %d bytes", MaxAgentLength)
	}
	for i, c := range t.ToolCalls {
		if c.Name == "" {
			return fmt.Errorf("tool_calls[%d].name is required", i)
		}
	}
	return nil
}

func NewID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails
	return hex.EncodeToString(b[:])
}
