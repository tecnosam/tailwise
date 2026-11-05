// Package rules checks traces against heuristic rules written in the config:
// the shape of the final answer and of tool results, and limits on the run.
// They need no model, so they cost nothing and their verdict is certain.
package rules

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/tecnosam/tailwise/internal/pipeline"
	"github.com/tecnosam/tailwise/internal/trace"
)

// Effect is what a rule does when it fires.
type Effect string

const (
	// Keep keeps the trace without asking the model, and ranks it first.
	Keep Effect = "keep"
	// Signal counts 1 in severity, the review order, and decides nothing.
	Signal Effect = "signal"
)

// Config is one rule as written in the config file or a manifest. It sets
// exactly one of FinalAnswer, ToolOutput, ToolArgs and Run.
type Config struct {
	Name string `yaml:"name" json:"name"`
	// OnFail is keep or signal; empty is signal.
	OnFail Effect `yaml:"on_fail" json:"on_fail"`
	// Enabled false turns off a default rule of the same name for one agent.
	Enabled *bool `yaml:"enabled" json:"enabled"`
	// Reason replaces the rule's own description of what the trace broke.
	Reason string `yaml:"reason" json:"reason"`

	FinalAnswer *Text       `yaml:"final_answer" json:"final_answer"`
	ToolOutput  *ToolOutput `yaml:"tool_output" json:"tool_output"`
	ToolArgs    *ToolArgs   `yaml:"tool_args" json:"tool_args"`
	Run         *Run        `yaml:"run" json:"run"`
}

// Text is what a piece of text must be. Each field set is one expectation;
// the rule fires when any is broken.
type Text struct {
	NotEmpty   bool   `yaml:"not_empty" json:"not_empty"`
	MinLength  int    `yaml:"min_length" json:"min_length"` // in characters
	MaxLength  int    `yaml:"max_length" json:"max_length"`
	Matches    string `yaml:"matches" json:"matches"`         // a regular expression it must match
	NotMatches string `yaml:"not_matches" json:"not_matches"` // one it must not
	JSON       bool   `yaml:"json" json:"json"`               // must parse as JSON
	JSONSchema any    `yaml:"json_schema" json:"json_schema"` // must be JSON valid against it
}

// ToolOutput checks the output of every call to Tool, or of every call when
// Tool is empty.
type ToolOutput struct {
	Tool string `yaml:"tool" json:"tool"`
	// OKStatus fires on a call whose status is not ok.
	OKStatus bool `yaml:"ok_status" json:"ok_status"`
	Text     `yaml:",inline"`
}

// ToolArgs checks the arguments of every call to Tool, or of every call when
// Tool is empty.
type ToolArgs struct {
	Tool       string `yaml:"tool" json:"tool"`
	JSONSchema any    `yaml:"json_schema" json:"json_schema"`
}

// Run limits the run as a whole.
type Run struct {
	MaxToolCalls int `yaml:"max_tool_calls" json:"max_tool_calls"`
	// MaxRepeatedCalls fires when one call, same tool and same arguments, is
	// made more than this many times: the agent is looping.
	MaxRepeatedCalls int      `yaml:"max_repeated_calls" json:"max_repeated_calls"`
	RequiresTools    []string `yaml:"requires_tools" json:"requires_tools"`
	ForbidsTools     []string `yaml:"forbids_tools" json:"forbids_tools"`
}

// Merge is an agent's rules: every default, then its own. One of its own
// replaces the default of the same name; with enabled false, it removes it.
func Merge(defaults, own []Config) []Config {
	byName := map[string]Config{}
	for _, c := range own {
		byName[c.Name] = c
	}
	var out []Config
	add := func(c Config) {
		if c.Enabled == nil || *c.Enabled {
			out = append(out, c)
		}
	}
	seen := map[string]bool{}
	for _, c := range defaults {
		seen[c.Name] = true
		if o, ok := byName[c.Name]; ok {
			c = o
		}
		add(c)
	}
	for _, c := range own {
		if !seen[c.Name] {
			add(c)
		}
	}
	return out
}

// Set is compiled rules, safe for concurrent use.
type Set struct {
	rules []rule
}

type rule struct {
	name   string
	keep   bool
	reason string
	check  func(trace.Trace) string // what was broken, "" when nothing
}

// New compiles rules, reporting every problem at once. No rules is a nil Set.
func New(cfgs []Config) (*Set, error) {
	if len(cfgs) == 0 {
		return nil, nil
	}
	var errs []error
	seen := map[string]bool{}
	s := &Set{}
	for i, c := range cfgs {
		key := fmt.Sprintf("rules[%d]", i)
		if c.Name != "" {
			key = fmt.Sprintf("rules[%d] (%s)", i, c.Name)
		}
		r, err := compile(c, seen)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
			continue
		}
		s.rules = append(s.rules, r)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return s, nil
}

func compile(c Config, seen map[string]bool) (rule, error) {
	if c.Name == "" {
		return rule{}, errors.New("name is required")
	}
	if seen[c.Name] {
		return rule{}, errors.New("name is used by another rule")
	}
	seen[c.Name] = true
	r := rule{name: c.Name, reason: c.Reason}
	switch c.OnFail {
	case Keep:
		r.keep = true
	case Signal, "":
	default:
		return rule{}, fmt.Errorf("on_fail is %q; want keep or signal", c.OnFail)
	}
	set := 0
	for _, isSet := range []bool{c.FinalAnswer != nil, c.ToolOutput != nil, c.ToolArgs != nil, c.Run != nil} {
		if isSet {
			set++
		}
	}
	if set != 1 {
		return rule{}, errors.New("set exactly one of final_answer, tool_output, tool_args and run")
	}
	var err error
	switch {
	case c.FinalAnswer != nil:
		r.check, err = finalAnswer(*c.FinalAnswer)
	case c.ToolOutput != nil:
		r.check, err = toolOutput(*c.ToolOutput)
	case c.ToolArgs != nil:
		r.check, err = toolArgs(*c.ToolArgs)
	default:
		r.check, err = run(*c.Run)
	}
	return r, err
}

// Check returns every rule's verdict, in order.
func (s *Set) Check(t trace.Trace) []pipeline.RuleResult {
	out := make([]pipeline.RuleResult, len(s.rules))
	for i, r := range s.rules {
		out[i] = pipeline.RuleResult{Name: r.name, Keep: r.keep}
		if broken := r.check(t); broken != "" {
			out[i].Fired = true
			out[i].Reason = r.reason
			if out[i].Reason == "" {
				out[i].Reason = broken
			}
		}
	}
	return out
}

func (t Text) empty() bool {
	return !t.NotEmpty && t.MinLength == 0 && t.MaxLength == 0 && t.Matches == "" && t.NotMatches == "" &&
		!t.JSON && t.JSONSchema == nil
}

// textCheck returns what a text broke, "" when nothing.
type textCheck func(string) string

func compileText(t Text) (textCheck, error) {
	var checks []textCheck
	if t.NotEmpty {
		checks = append(checks, func(s string) string {
			if strings.TrimSpace(s) == "" {
				return "is empty"
			}
			return ""
		})
	}
	if t.MinLength < 0 || t.MaxLength < 0 {
		return nil, errors.New("min_length and max_length must not be negative")
	}
	if t.MinLength > 0 {
		checks = append(checks, func(s string) string {
			if n := utf8.RuneCountInString(s); n < t.MinLength {
				return fmt.Sprintf("is %d characters, under the minimum of %d", n, t.MinLength)
			}
			return ""
		})
	}
	if t.MaxLength > 0 {
		checks = append(checks, func(s string) string {
			if n := utf8.RuneCountInString(s); n > t.MaxLength {
				return fmt.Sprintf("is %d characters, over the maximum of %d", n, t.MaxLength)
			}
			return ""
		})
	}
	for _, re := range []struct {
		pattern string
		must    bool
	}{{t.Matches, true}, {t.NotMatches, false}} {
		if re.pattern == "" {
			continue
		}
		compiled, err := regexp.Compile(re.pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid regular expression %q: %w", re.pattern, err)
		}
		must, pattern := re.must, re.pattern
		checks = append(checks, func(s string) string {
			switch matched := compiled.MatchString(s); {
			case must && !matched:
				return fmt.Sprintf("does not match %q", pattern)
			case !must && matched:
				return fmt.Sprintf("matches %q", pattern)
			}
			return ""
		})
	}
	if t.JSON && t.JSONSchema == nil {
		checks = append(checks, func(s string) string {
			if !json.Valid([]byte(s)) {
				return "is not valid JSON"
			}
			return ""
		})
	}
	if t.JSONSchema != nil {
		schema, err := compileSchema(t.JSONSchema)
		if err != nil {
			return nil, err
		}
		checks = append(checks, func(s string) string {
			v, err := jsonschema.UnmarshalJSON(strings.NewReader(s))
			if err != nil {
				return "is not valid JSON"
			}
			return schemaBreak(schema, v)
		})
	}
	if len(checks) == 0 {
		return nil, errors.New("set at least one of not_empty, min_length, max_length, matches, not_matches, json and json_schema")
	}
	return func(s string) string {
		for _, c := range checks {
			if broken := c(s); broken != "" {
				return broken
			}
		}
		return ""
	}, nil
}

func compileSchema(doc any) (*jsonschema.Schema, error) {
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("json_schema: %w", err)
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("json_schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("rule.json", v); err != nil {
		return nil, fmt.Errorf("json_schema: %w", err)
	}
	schema, err := c.Compile("rule.json")
	if err != nil {
		return nil, fmt.Errorf("json_schema: %w", err)
	}
	return schema, nil
}

// maxSchemaReason caps the schema errors quoted in a reason.
const maxSchemaReason = 200

func schemaBreak(schema *jsonschema.Schema, v any) string {
	err := schema.Validate(v)
	if err == nil {
		return ""
	}
	// The first line names the schema; the rest are the problems, one a line.
	lines := strings.Split(err.Error(), "\n")
	var problems []string
	for _, l := range lines[1:] {
		if l = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "-")); l != "" {
			problems = append(problems, l)
		}
	}
	detail := strings.Join(problems, "; ")
	if utf8.RuneCountInString(detail) > maxSchemaReason {
		detail = string([]rune(detail)[:maxSchemaReason]) + "…"
	}
	if detail == "" {
		return "does not match the schema"
	}
	return "does not match the schema: " + detail
}

func finalAnswer(t Text) (func(trace.Trace) string, error) {
	check, err := compileText(t)
	if err != nil {
		return nil, fmt.Errorf("final_answer: %w", err)
	}
	return func(tr trace.Trace) string {
		if broken := check(tr.FinalAnswer); broken != "" {
			return "The final answer " + broken + "."
		}
		return ""
	}, nil
}

func toolOutput(o ToolOutput) (func(trace.Trace) string, error) {
	var check textCheck
	if !o.Text.empty() {
		var err error
		if check, err = compileText(o.Text); err != nil {
			return nil, fmt.Errorf("tool_output: %w", err)
		}
	} else if !o.OKStatus {
		return nil, errors.New("tool_output: set ok_status or a text expectation")
	}
	return func(tr trace.Trace) string {
		for i, c := range tr.ToolCalls {
			if o.Tool != "" && c.Name != o.Tool {
				continue
			}
			if o.OKStatus && c.StatusText() != "ok" {
				return fmt.Sprintf("Call %d, to %s, failed with status %s.", i+1, c.Name, c.StatusText())
			}
			if check == nil {
				continue
			}
			if broken := check(c.Output); broken != "" {
				return fmt.Sprintf("The output of call %d, to %s, %s.", i+1, c.Name, broken)
			}
		}
		return ""
	}, nil
}

func toolArgs(a ToolArgs) (func(trace.Trace) string, error) {
	if a.JSONSchema == nil {
		return nil, errors.New("tool_args: json_schema is required")
	}
	schema, err := compileSchema(a.JSONSchema)
	if err != nil {
		return nil, fmt.Errorf("tool_args: %w", err)
	}
	return func(tr trace.Trace) string {
		for i, c := range tr.ToolCalls {
			if a.Tool != "" && c.Name != a.Tool {
				continue
			}
			args := c.Args
			if len(args) == 0 {
				args = json.RawMessage("null")
			}
			v, err := jsonschema.UnmarshalJSON(bytes.NewReader(args))
			if err != nil {
				return fmt.Sprintf("The arguments of call %d, to %s, are not valid JSON.", i+1, c.Name)
			}
			if broken := schemaBreak(schema, v); broken != "" {
				return fmt.Sprintf("The arguments of call %d, to %s, %s.", i+1, c.Name, broken)
			}
		}
		return ""
	}, nil
}

func run(r Run) (func(trace.Trace) string, error) {
	if r.MaxToolCalls < 0 || r.MaxRepeatedCalls < 0 {
		return nil, errors.New("run: max_tool_calls and max_repeated_calls must not be negative")
	}
	if r.MaxToolCalls == 0 && r.MaxRepeatedCalls == 0 && len(r.RequiresTools) == 0 && len(r.ForbidsTools) == 0 {
		return nil, errors.New("run: set at least one of max_tool_calls, max_repeated_calls, requires_tools and forbids_tools")
	}
	return func(tr trace.Trace) string {
		if r.MaxToolCalls > 0 && len(tr.ToolCalls) > r.MaxToolCalls {
			return fmt.Sprintf("The agent made %d tool calls, over the limit of %d.", len(tr.ToolCalls), r.MaxToolCalls)
		}
		called := map[string]bool{}
		repeats := map[string]int{}
		for _, c := range tr.ToolCalls {
			called[c.Name] = true
			key := c.Name + "(" + c.ArgsText() + ")"
			repeats[key]++
			if r.MaxRepeatedCalls > 0 && repeats[key] == r.MaxRepeatedCalls+1 {
				return fmt.Sprintf("The agent called %s more than %d times with the same arguments.", c.Name, r.MaxRepeatedCalls)
			}
		}
		for _, tool := range r.RequiresTools {
			if !called[tool] {
				return fmt.Sprintf("The agent never called %s.", tool)
			}
		}
		for _, tool := range r.ForbidsTools {
			if called[tool] {
				return fmt.Sprintf("The agent called %s, which it must not.", tool)
			}
		}
		return ""
	}, nil
}
