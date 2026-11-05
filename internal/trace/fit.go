package trace

import (
	"encoding/json"
	"slices"
	"unicode/utf8"
)

// MaxStateChars bounds the state sent to the model. Jev rejects longer
// requests (max_tokens_exceeded), and a rejected trace falls back to a random
// decision. In the benchmark it billed up to 26.5k tokens without rejecting,
// and rejected a 64,000-character patch dense enough to be about 35k: at that
// density this bound is about 22k.
const MaxStateChars = 40000

const (
	cutMark = "\n[... cut to fit ...]\n"
	// minCutChars is the shortest a field is cut to: below it, Fit gives up
	// rather than cut every field to nothing.
	minCutChars = 200
)

// Fit renders t with build and, while the state is longer than limit
// characters, cuts the middle out of the longest field: the question, the
// final answer, the system prompt, an evidence item, or a tool call's
// arguments or result. It keeps the start and end of each, where a patch's
// header and a log's outcome usually are. trimmed reports whether anything was
// cut; the state can still be over limit when every field is already short.
func Fit(t Trace, limit int, build func(Trace) State) (state State, trimmed bool) {
	state = build(t)
	if len(state) <= limit {
		return state, false
	}
	t.ToolCalls = slices.Clone(t.ToolCalls)
	t.Evidence = slices.Clone(t.Evidence)
	fields := textFields(&t)
	spent := make([]bool, len(fields)) // cut without shortening the state: not shown, or escaping ate the cut
	for len(state) > limit {
		i := longest(fields, spent)
		if i < 0 {
			break
		}
		f := fields[i]
		keep := max(f.len()-(len(state)-limit)-len(cutMark), minCutChars)
		f.set(cutMiddle(f.get(), keep))
		trimmed = true
		before := len(state)
		if state = build(t); len(state) >= before {
			spent[i] = true
		}
	}
	return state, trimmed
}

type textField struct {
	get func() string
	set func(string)
}

func (f textField) len() int { return len(f.get()) }

func textFields(t *Trace) []textField {
	fields := []textField{
		{func() string { return t.Question }, func(s string) { t.Question = s }},
		{func() string { return t.FinalAnswer }, func(s string) { t.FinalAnswer = s }},
		{func() string { return t.SystemPrompt }, func(s string) { t.SystemPrompt = s }},
	}
	for i := range t.Evidence {
		fields = append(fields, textField{func() string { return t.Evidence[i] }, func(s string) { t.Evidence[i] = s }})
	}
	for i := range t.ToolCalls {
		c := &t.ToolCalls[i]
		fields = append(fields,
			textField{func() string { return c.Output }, func(s string) { c.Output = s }},
			// Cut arguments become a JSON string of the cut text.
			textField{func() string { return argsText(c.Args) }, func(s string) { c.Args, _ = json.Marshal(s) }})
	}
	return fields
}

// longest is the index of the longest field still worth cutting, -1 when
// none is longer than minCutChars.
func longest(fields []textField, spent []bool) int {
	best := -1
	for i, f := range fields {
		if !spent[i] && f.len() > minCutChars && (best < 0 || f.len() > fields[best].len()) {
			best = i
		}
	}
	return best
}

// argsText is the arguments as text: a JSON string's value, so cutting it
// again does not escape it twice, else the raw JSON.
func argsText(args json.RawMessage) string {
	var s string
	if json.Unmarshal(args, &s) == nil {
		return s
	}
	return string(args)
}

// cutMiddle keeps about keep bytes of s, half from each end, on rune
// boundaries, with cutMark between them.
func cutMiddle(s string, keep int) string {
	head, tail := keep/2, keep-keep/2
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	start := len(s) - tail
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[:head] + cutMark + s[start:]
}
