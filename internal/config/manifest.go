package config

import (
	"bytes"
	"cmp"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"slices"

	"github.com/tecnosam/tailwise/internal/pipeline"
)

//go:embed builtin_manifest.json
var builtinManifest []byte

func BuiltinManifest(decisionQuestion string) (*Manifest, error) {
	m, err := ParseManifest(builtinManifest, decisionQuestion)
	if err != nil {
		return nil, fmt.Errorf("built-in manifest: %w", err)
	}
	return m, nil
}

func LoadManifest(path, decisionQuestion string) (*Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	m, err := ParseManifest(raw, decisionQuestion)
	if err != nil {
		return nil, fmt.Errorf("manifest %s: %w", path, err)
	}
	return m, nil
}

// readReasons maps each option of the reason question to its description. A
// manifest without the question has no reasons; one where it is not a choice
// is an error.
func (m *Manifest) readReasons(question string) (bool, error) {
	m.Reasons = nil
	if question == "" {
		return true, nil
	}
	var questions map[string]json.RawMessage
	if err := json.Unmarshal(m.Questions, &questions); err != nil {
		return false, err
	}
	raw, ok := questions[question]
	if !ok {
		return false, nil
	}
	var q struct {
		Type     string            `json:"type"`
		Criteria map[string]string `json:"criteria"`
	}
	if err := json.Unmarshal(raw, &q); err != nil || q.Type != "choice" || len(q.Criteria) == 0 {
		return false, fmt.Errorf("reason question %q must be a choice question with plain-text criteria", question)
	}
	m.Reasons = make(map[string]string, len(q.Criteria))
	for choice, description := range q.Criteria {
		m.Reasons[choice] = cmp.Or(description, choice)
	}
	return true, nil
}

func ParseManifest(raw []byte, decisionQuestion string) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if err := m.prepare(decisionQuestion); err != nil {
		return nil, err
	}
	return &m, nil
}

// prepare compacts the questions (keys and order untouched) and checks the
// decision question.
func (m *Manifest) prepare(decisionQuestion string) error {
	var questions map[string]json.RawMessage
	if err := json.Unmarshal(m.Questions, &questions); err != nil || len(questions) == 0 {
		return fmt.Errorf("\"questions\" must be a non-empty object")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, m.Questions); err != nil {
		return err
	}
	m.Questions = compact.Bytes()

	q, ok := questions[decisionQuestion]
	if !ok {
		return fmt.Errorf("decision question %q not found in questions", decisionQuestion)
	}
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(q, &head); err != nil {
		return fmt.Errorf("question %q: %w", decisionQuestion, err)
	}
	if head.Type != questionTypeNoul {
		return fmt.Errorf("decision question %q has type %q, want %q", decisionQuestion, head.Type, questionTypeNoul)
	}
	return m.checkDecision(questions, decisionQuestion)
}

// checkDecision accepts a decision block that lists only yes/no questions of
// the manifest, each once, and sets thresholds only for questions that decide.
func (m *Manifest) checkDecision(questions map[string]json.RawMessage, decisionQuestion string) error {
	if m.Decision == nil {
		return nil
	}
	if len(m.Decision.AnyOf) == 0 && len(m.Decision.Thresholds) == 0 {
		return fmt.Errorf("\"decision\" must list questions in any_of or set thresholds")
	}
	deciding := map[string]bool{decisionQuestion: true}
	for _, q := range m.Decision.AnyOf {
		if deciding[q] {
			return fmt.Errorf("decision.any_of: question %q is listed twice, or is the decision question, which always comes first", q)
		}
		deciding[q] = true
		raw, ok := questions[q]
		if !ok {
			return fmt.Errorf("decision.any_of: question %q not found in questions", q)
		}
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &head); err != nil || head.Type != questionTypeNoul {
			return fmt.Errorf("decision.any_of: question %q has type %q, want %q", q, head.Type, questionTypeNoul)
		}
	}
	for q, t := range m.Decision.Thresholds {
		if !deciding[q] {
			return fmt.Errorf("decision.thresholds: question %q is neither the decision question nor in any_of", q)
		}
		if t <= 0 || t > 1 {
			return fmt.Errorf("decision.thresholds: %q is %v, want a number in (0, 1]", q, t)
		}
	}
	return nil
}

// anyOf is the decision block's questions, none without one.
func (m *Manifest) anyOf() []string {
	if m.Decision == nil {
		return nil
	}
	return slices.Clone(m.Decision.AnyOf)
}

// checks are the questions that decide, the decision question first and then
// any_of in order, weighted by that priority.
func (m *Manifest) checks(decisionQuestion string) []pipeline.Check {
	questions := append([]string{decisionQuestion}, m.anyOf()...)
	weights := pipeline.PriorityWeights(len(questions))
	checks := make([]pipeline.Check, len(questions))
	for i, q := range questions {
		checks[i] = pipeline.Check{Question: q, Weight: weights[i]}
		if m.Decision != nil {
			checks[i].Threshold = pipeline.Probability(m.Decision.Thresholds[q])
		}
	}
	return checks
}
