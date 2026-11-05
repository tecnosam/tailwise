package trace

import (
	"strconv"
	"strings"
)

// Changing these changes what the model sees.
const (
	MaxEvidence          = 3
	MaxEvidenceChars     = 1500
	MaxSystemPromptChars = 8000
)

type State string

// BuildState renders the trace into the model's state string. The golden
// tests pin its exact output. The system prompt section is left out when
// there is none, so a trace without one renders exactly as before.
func BuildState(t Trace) State {
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
	}

	b.WriteString("\n\nEVIDENCE:\n")
	evidence := t.Evidence
	if len(evidence) > MaxEvidence {
		evidence = evidence[:MaxEvidence]
	}
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

func (c ToolCall) ArgsText() string {
	if len(c.Args) == 0 {
		return ""
	}
	return compact(c.Args)
}

// truncateRunes keeps the first n code points, like Python's s[:n].
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}
