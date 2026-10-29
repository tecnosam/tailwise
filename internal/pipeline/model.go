package pipeline

import (
	"context"
	"fmt"

	"github.com/tecnosam/tailwise/internal/retry"
	"github.com/tecnosam/tailwise/internal/trace"
)

// Model is one decision model and the scorer that asks it.
type Model struct {
	// Name labels the model in fallback decisions, when it reported nothing.
	Name   ModelName
	Scorer Scorer
	// Retry is used on the synchronous path only; the queued path leaves retrying
	// to the queue, so the two never multiply.
	Retry retry.Policy
	// Checks are the questions that can keep a trace, in priority order. With
	// none, p_keep alone decides.
	Checks []Check
	// Agent is set by Route.
	Agent AgentLabel
}

// AgentLabel is a trace's agent as metrics and logs show it. Unlike the name a
// client sends, its values are bounded by the config, so it is safe as a metric
// attribute.
type AgentLabel string

const (
	AgentNone  AgentLabel = "none"  // the trace named no agent
	AgentOther AgentLabel = "other" // it named one with no model of its own
)

type ModelRouter interface {
	// Route returns the default model for an unknown or empty agent.
	Route(agent trace.AgentName) Model
	// Ready checks the default model only: one agent's model being down must not
	// make the whole engine unready.
	Ready(ctx context.Context) error
}

type AgentRouter struct {
	fallback Model
	byAgent  map[trace.AgentName]Model
}

// byAgent may be nil: one model for everything.
func NewAgentRouter(def Model, byAgent map[trace.AgentName]Model) *AgentRouter {
	return &AgentRouter{fallback: def, byAgent: byAgent}
}

func SingleModel(m Model) *AgentRouter { return NewAgentRouter(m, nil) }

func (r *AgentRouter) Route(agent trace.AgentName) Model {
	if m, ok := r.byAgent[agent]; ok {
		m.Agent = AgentLabel(agent)
		return m
	}
	m := r.fallback
	m.Agent = AgentOther
	if agent == "" {
		m.Agent = AgentNone
	}
	return m
}

func (r *AgentRouter) Ready(ctx context.Context) error {
	if err := r.fallback.Scorer.Ready(ctx); err != nil {
		return fmt.Errorf("model %s (default): %w", r.fallback.Name, err)
	}
	return nil
}
