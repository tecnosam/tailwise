package config

import (
	"bytes"
	"cmp"
	_ "embed"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

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
	if err := m.checkActions(questions); err != nil {
		return err
	}
	return m.checkDecision(questions, decisionQuestion)
}

// checkActions accepts named tools with a description each, and keeps the
// action checks' answer names free.
func (m *Manifest) checkActions(questions map[string]json.RawMessage) error {
	for tool, what := range m.Actions {
		if strings.TrimSpace(tool) == "" || strings.TrimSpace(what) == "" {
			return fmt.Errorf("actions: every tool needs a name and a description of what it does, got %q: %q", tool, what)
		}
	}
	if len(m.Actions) > 0 {
		for _, q := range []string{pipeline.ActionWrong, pipeline.UnhandledRequest} {
			if _, ok := questions[q]; ok {
				return fmt.Errorf("questions: %q is the name of an action check's answer; rename the question", q)
			}
		}
	}
	return nil
}

// actions is the action checks' configuration, nil without an actions block.
func (m *Manifest) actions() *pipeline.Actions {
	if len(m.Actions) == 0 {
		return nil
	}
	return &pipeline.Actions{
		Tools:                 maps.Clone(m.Actions),
		ActionQuestions:       actionQuestions,
		CompletenessQuestions: completenessQuestions(m.Actions),
	}
}

// One-of-several keeps a request handled by another action from being blamed
// on this one.
var actionQuestions = mustQuestions([][2]string{{pipeline.ActionWrong,
	"Does the ACTION use any argument that is not what the user asked for: a different item, product " +
		"option, order, account, booking, recipient, amount, date, address or payment method than the user " +
		"requested, according to the USER REQUEST and the LOOKUP RESULTS? Judge this ACTION only: the user's " +
		"other requests may be handled by the LATER ACTIONS, so a request this ACTION does not cover is not " +
		"a problem with it."}})

// completenessQuestions asks, for each action in name order, whether the user
// asked for it and whether every such request was handled.
func completenessQuestions(actions map[string]string) json.RawMessage {
	var qs [][2]string
	for _, tool := range slices.Sorted(maps.Keys(actions)) {
		what := actions[tool]
		qs = append(qs,
			[2]string{pipeline.RequestedQuestion(tool), "At any point in the USER REQUEST, did the user ask the agent to " + what + "?"},
			[2]string{pipeline.HandledQuestion(tool), "Was every user request to " + what + " HANDLED, as defined in the NOTES?"})
	}
	return mustQuestions(qs)
}

// mustQuestions encodes yes/no questions as a questions object, in order.
func mustQuestions(qs [][2]string) json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, q := range qs {
		if i > 0 {
			b.WriteByte(',')
		}
		name, _ := json.Marshal(q[0])
		spec, _ := json.Marshal(map[string]string{"type": questionTypeNoul, "instructions": q[1]})
		b.Write(name)
		b.WriteByte(':')
		b.Write(spec)
	}
	b.WriteByte('}')
	return b.Bytes()
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
	if len(m.Actions) > 0 {
		deciding[pipeline.ActionWrong], deciding[pipeline.UnhandledRequest] = true, true
	}
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
			return fmt.Errorf("decision.thresholds: question %q is neither the decision question, in any_of, nor an action check", q)
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

// checks are the questions that decide, the decision question first, then
// any_of in order, then the action checks.
func (m *Manifest) checks(decisionQuestion string) []pipeline.Check {
	questions := append([]string{decisionQuestion}, m.anyOf()...)
	if len(m.Actions) > 0 {
		questions = append(questions, pipeline.ActionWrong, pipeline.UnhandledRequest)
	}
	checks := make([]pipeline.Check, len(questions))
	for i, q := range questions {
		checks[i] = pipeline.Check{Question: q}
		if m.Decision != nil {
			checks[i].Threshold = pipeline.Probability(m.Decision.Thresholds[q])
		}
	}
	return checks
}
