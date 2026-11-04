package trace

import (
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Evidence strategies: how tool results reach the model.
const (
	// EvidenceFirst shows the first MaxEvidence evidence items (BuildState).
	EvidenceFirst = "first"
	// EvidenceGrounded shows tool results on their tool-call lines, chosen by
	// priority within a budget (BuildStateGrounded).
	EvidenceGrounded = "grounded"
)

const (
	// MaxResultChars caps one tool result in the grounded slice.
	MaxResultChars = 2000
	// maxInlineChars is the longest result kept on its tool-call line; a
	// longer one, or one spanning lines, goes on indented lines below it.
	maxInlineChars = 300
	minResultChars = 80
	trimmedMark    = " [trimmed]"
	resultIndent   = "   "
)

// BuildStateGrounded renders the trace like BuildState, but shows each tool
// call's result on its line, as long as budget characters of results allow.
// Results are chosen in this order:
//
//  1. results holding a value that a later call passes as an argument, or
//     that the final answer cites: what the agent's actions rest on;
//  2. the remaining results, most recent first.
//
// A result longer than MaxResultChars keeps the parts later calls used (for
// JSON, each object holding such a value) before it is cut. Evidence items
// that came from tool results are not repeated in the evidence section.
func BuildStateGrounded(t Trace, budget int) State {
	results := chooseResults(t, budget)

	var b strings.Builder
	if t.SystemPrompt != "" {
		b.WriteString("SYSTEM PROMPT:\n")
		b.WriteString(truncateRunes(t.SystemPrompt, MaxSystemPromptChars))
		b.WriteString("\n\n")
	}
	b.WriteString("USER QUESTION:\n")
	b.WriteString(t.Question)

	b.WriteString("\n\nTOOL CALLS:\n")
	if len(t.ToolCalls) == 0 {
		b.WriteString("(none)")
	}
	for i, c := range t.ToolCalls {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteString(". ")
		b.WriteString(c.Name)
		b.WriteByte('(')
		b.WriteString(c.ArgsText())
		b.WriteString(") -> ")
		b.WriteString(c.StatusText())
		if r := results[i]; r != "" {
			if len(r) <= maxInlineChars && !strings.Contains(r, "\n") {
				b.WriteString(": ")
				b.WriteString(r)
			} else {
				b.WriteString(":\n")
				b.WriteString(resultIndent)
				b.WriteString(strings.ReplaceAll(r, "\n", "\n"+resultIndent))
			}
		}
	}

	b.WriteString("\n\nEVIDENCE:\n")
	evidence := otherEvidence(t)
	if len(evidence) == 0 {
		b.WriteString("(none)")
	}
	for i, e := range evidence {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteByte('[')
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteString("] ")
		b.WriteString(truncateRunes(e, MaxEvidenceChars))
	}

	b.WriteString("\n\nFINAL ANSWER:\n")
	b.WriteString(t.FinalAnswer)
	return State(b.String())
}

// chooseResults is the text shown for each call's result, "" for none.
func chooseResults(t Trace, budget int) []string {
	shown := make([]string, len(t.ToolCalls))
	used := laterValues(t)
	var grounding, rest []int
	for i := len(t.ToolCalls) - 1; i >= 0; i-- {
		out := strings.TrimSpace(t.ToolCalls[i].Output)
		switch {
		case out == "":
		case containsAny(out, used[i]):
			grounding = append(grounding, i)
		default:
			rest = append(rest, i)
		}
	}
	left := budget
	for _, i := range append(grounding, rest...) {
		if left < minResultChars {
			break
		}
		r := fit(strings.TrimSpace(t.ToolCalls[i].Output), used[i], min(MaxResultChars, left))
		shown[i] = r
		left -= len(r)
	}
	return shown
}

// fit returns out whole if it fits in limit bytes, else the parts of it that
// hold a used value, else its start; cut to limit.
func fit(out string, used []string, limit int) string {
	if len(out) <= limit {
		return out
	}
	if trimmed, ok := keepUsed(out, used); ok && len(trimmed)+len(trimmedMark) <= limit {
		return trimmed + trimmedMark
	}
	cut := truncateBytes(out, limit-len(trimmedMark))
	return cut + trimmedMark
}

// laterValues lists, for each call, the distinctive values that later calls
// pass as arguments or that the final answer cites.
func laterValues(t Trace) [][]string {
	out := make([][]string, len(t.ToolCalls))
	seen := map[string]bool{}
	var acc []string
	add := func(v string) {
		if !seen[v] {
			seen[v] = true
			acc = append(acc, v)
		}
	}
	for _, v := range idLike.FindAllString(t.FinalAnswer, -1) {
		if distinctive(v) {
			add(v)
		}
	}
	for i := len(t.ToolCalls) - 1; i >= 0; i-- {
		out[i] = slices.Clone(acc)
		for _, v := range argValues(t.ToolCalls[i].Args) {
			add(v)
		}
	}
	return out
}

// idLike matches the identifiers a final answer cites: a run of letters,
// digits, # or - with a digit in it (distinctive drops the short ones).
var idLike = regexp.MustCompile(`#?[A-Za-z0-9-]*[0-9][A-Za-z0-9-]*`)

// argValues is every distinctive string or number in a call's arguments.
func argValues(raw json.RawMessage) []string {
	var v any
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil {
		return nil
	}
	var out []string
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for _, c := range x {
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		case string:
			if s := strings.TrimSpace(x); distinctive(s) {
				out = append(out, s)
			}
		case float64:
			if s := strconv.FormatFloat(x, 'f', -1, 64); distinctive(s) {
				out = append(out, s)
			}
		}
	}
	walk(v)
	return out
}

// distinctive keeps values specific enough that finding one in a result
// means the result is where it came from: no short words or small numbers.
func distinctive(s string) bool {
	if len(s) > 200 {
		return false
	}
	hasDigit := strings.ContainsAny(s, "0123456789")
	return (hasDigit && len(s) >= 4) || len(s) >= 8
}

func containsAny(s string, values []string) bool {
	for _, v := range values {
		if v != "" && strings.Contains(s, v) {
			return true
		}
	}
	return false
}

// keepUsed reduces a JSON result to the objects holding a used value (each
// kept whole) and the top-level scalars, or a text result to its lines
// holding one. ok is false when nothing in it was used.
func keepUsed(out string, used []string) (string, bool) {
	var v any
	if json.Unmarshal([]byte(out), &v) == nil {
		kept, ok := keepJSON(v, used, true)
		if !ok {
			return "", false
		}
		b, err := json.Marshal(kept)
		return string(b), err == nil
	}
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if containsAny(line, used) {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n"), len(lines) > 0
}

// keepJSON keeps an object whole if one of its own scalar fields is a used
// value; otherwise it keeps only the children that hold one (and, at the top
// level, the scalar fields, which name what the result is about).
func keepJSON(v any, used []string, top bool) (any, bool) {
	switch x := v.(type) {
	case map[string]any:
		for _, c := range x {
			if scalarUsed(c, used) {
				return x, true
			}
		}
		kept := map[string]any{}
		found := false
		for k, c := range x {
			if sub, ok := keepJSON(c, used, false); ok {
				kept[k] = sub
				found = true
			} else if top && isScalar(c) {
				kept[k] = c
			}
		}
		return kept, found
	case []any:
		var kept []any
		for _, c := range x {
			if sub, ok := keepJSON(c, used, false); ok {
				kept = append(kept, sub)
			}
		}
		return kept, len(kept) > 0
	default:
		return x, scalarUsed(x, used)
	}
}

func scalarUsed(v any, used []string) bool {
	var s string
	switch x := v.(type) {
	case string:
		s = x
	case float64:
		s = strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return false
	}
	return slices.Contains(used, strings.TrimSpace(s))
}

func isScalar(v any) bool {
	switch v.(type) {
	case map[string]any, []any:
		return false
	}
	return true
}

// otherEvidence is the trace's evidence minus items taken from tool results,
// which the grounded slice shows on the tool-call lines instead.
func otherEvidence(t Trace) []string {
	var out []string
	for _, e := range t.Evidence {
		fromTool := false
		for _, c := range t.ToolCalls {
			if c.Output != "" && strings.Contains(c.Output, strings.TrimSpace(e)) {
				fromTool = true
				break
			}
		}
		if !fromTool {
			out = append(out, e)
		}
	}
	if len(out) > MaxEvidence {
		out = out[:MaxEvidence]
	}
	return out
}

// truncateBytes cuts s to at most n bytes without splitting a character.
func truncateBytes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }
