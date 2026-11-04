package scorer

import (
	"fmt"

	"github.com/tecnosam/tailwise/internal/pipeline"
	"github.com/tecnosam/tailwise/internal/retry"
	"github.com/tecnosam/tailwise/internal/trace"
)

type ModelConfig struct {
	Client Config
	// Retry is how the synchronous endpoint retries a failed call.
	Retry retry.Policy
	// Checks are the questions that can keep a trace, in priority order.
	Checks []pipeline.Check
	// SystemPrompt shows the model the agent's system prompt.
	SystemPrompt bool
	// Evidence is how tool results reach the model (trace.EvidenceFirst or
	// trace.EvidenceGrounded); EvidenceBudget bounds the grounded results.
	Evidence       string
	EvidenceBudget int
}

type Models struct {
	Default ModelConfig
	ByAgent map[trace.AgentName]ModelConfig
}

func BuildRouter(ms Models) (*pipeline.AgentRouter, error) {
	def, err := buildModel(ms.Default)
	if err != nil {
		return nil, fmt.Errorf("default model: %w", err)
	}
	byAgent := make(map[trace.AgentName]pipeline.Model, len(ms.ByAgent))
	for agent, mc := range ms.ByAgent {
		if byAgent[agent], err = buildModel(mc); err != nil {
			return nil, fmt.Errorf("model for agent %s: %w", agent, err)
		}
	}
	return pipeline.NewAgentRouter(def, byAgent), nil
}

func buildModel(mc ModelConfig) (pipeline.Model, error) {
	s, err := New(mc.Client)
	if err != nil {
		return pipeline.Model{}, err
	}
	return pipeline.Model{Name: pipeline.ModelName(mc.Client.Model), Scorer: s, Retry: mc.Retry, Checks: mc.Checks,
		SystemPrompt: mc.SystemPrompt, Evidence: mc.Evidence, EvidenceBudget: mc.EvidenceBudget}, nil
}
