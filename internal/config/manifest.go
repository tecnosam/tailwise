package config

import (
	"bytes"
	"cmp"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
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
	return nil
}
