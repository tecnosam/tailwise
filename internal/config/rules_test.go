package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tecnosam/tailwise/internal/trace"
)

const rulesManifest = `{
  "model": "support",
  "questions": {"answer_wrong": {"type": "noul", "instructions": "is the support answer wrong?"}},
  "rules": [
    {"name": "answer_json", "on_fail": "signal", "final_answer": {"json": true}},
    {"name": "no_loops", "enabled": false},
    {"name": "order_schema", "on_fail": "keep", "tool_output": {"tool": "get_order", "json_schema": {"required": ["id"]}}}
  ]
}`

const defaultRules = `
rules:
  - name: answer_json
    on_fail: keep
    final_answer: {json: true}
  - name: no_loops
    run: {max_repeated_calls: 3}
agents:
  models:
    support-bot:
      endpoint: http://kev-support:8010/v1/systemone
      model: kev-support
      manifest: rules.json
`

// The config's rules apply to every model; a manifest overrides them by name,
// turns one off, and adds its own.
func TestRulesMergeWithManifest(t *testing.T) {
	path := writeAgents(t, defaultRules)
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "rules.json"), []byte(rulesManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	looping := trace.Trace{Question: "q", FinalAnswer: "not json", ToolCalls: []trace.ToolCall{
		{Name: "get_order"}, {Name: "get_order"}, {Name: "get_order"}, {Name: "get_order"}}}
	def := cfg.Models.Default.Rules.Check(looping)
	if len(def) != 2 || !def[0].Keep || !def[0].Fired || def[1].Name != "no_loops" || !def[1].Fired {
		t.Errorf("default model: %+v", def)
	}
	own := cfg.Models.ByAgent["support-bot"].Rules.Check(looping)
	if len(own) != 2 || own[0].Name != "answer_json" || own[0].Keep || own[1].Name != "order_schema" || !own[1].Keep {
		t.Errorf("support-bot: %+v", own)
	}
}

func TestRulesErrorsNameTheModel(t *testing.T) {
	_, err := Load(write(t, "scorer:\n  manifest: manifest.json\nrules:\n  - name: bad\n    final_answer: {matches: '('}\n", testManifest))
	if err == nil || !strings.Contains(err.Error(), "scorer: rules[0] (bad): final_answer: invalid regular expression") {
		t.Errorf("err = %v", err)
	}
}
