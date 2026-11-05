package trace

import (
	"strconv"
	"strings"
)

// The states of the two action checks, which look closer than the main
// question can at agents that act for a user (cancel an order, book a flight,
// send money): an action call is one to a tool the manifest lists as an
// action.
const (
	// MaxLookups is how many earlier results an action check shows: the most
	// recent ones holding a value the action passes.
	MaxLookups = 3
	// MaxLookupChars caps one lookup result, cut to the parts holding a value
	// the action passes.
	MaxLookupChars = 2500
	// maxStatusChars caps an error message shown with an action's status.
	maxStatusChars = 200
	// maxAgentTurnChars caps one agent turn in the completeness check: what
	// it said it did matters more than its long listings.
	maxAgentTurnChars = 600
	// maxConversationChars bounds the conversation shown; the earliest turns
	// go first.
	maxConversationChars = 24000
)

// HandledNote defines HANDLED for the completeness check's questions.
const HandledNote = "NOTES: A user request is HANDLED when the agent either carried it out with an " +
	"AGENT ACTION, or explicitly declined it and told the user why. It is not handled if it was left " +
	"unaddressed, done only in part, or the agent said it would act but no AGENT ACTION did it. " +
	"Lookups are not shown."

// BuildActionState is the state for checking one action call, the i-th tool
// call: does it do what the user asked? It holds the user's request, the
// earlier results the action's argument values came from (where the right
// item, order or account is identified), the action, and the actions after
// it, so a request handled in a later step is not blamed on this one.
func BuildActionState(t Trace, i int, isAction func(name string) bool) State {
	call := t.ToolCalls[i]
	values := argValues(call.Args)

	var lookups []int
	for j := i - 1; j >= 0 && len(lookups) < MaxLookups; j-- {
		c := t.ToolCalls[j]
		if !isAction(c.Name) && containsAny(c.Output, values) {
			lookups = append(lookups, j)
		}
	}

	var b strings.Builder
	b.WriteString("USER REQUEST:\n")
	b.WriteString(t.Question)
	b.WriteString("\n\nLOOKUP RESULTS THE ACTION'S VALUES CAME FROM:\n")
	if len(lookups) == 0 {
		b.WriteString("(none)")
	}
	for k := len(lookups) - 1; k >= 0; k-- {
		c := t.ToolCalls[lookups[k]]
		if k < len(lookups)-1 {
			b.WriteByte('\n')
		}
		b.WriteByte('[')
		b.WriteString(strconv.Itoa(len(lookups) - k))
		b.WriteString("] ")
		writeCall(&b, c)
		b.WriteString(": ")
		b.WriteString(fit(strings.TrimSpace(c.Output), values, MaxLookupChars))
	}
	b.WriteString("\n\nACTION:\n")
	writeCall(&b, call)
	b.WriteString("\n\nLATER ACTIONS IN THIS RUN:\n")
	later := false
	for _, c := range t.ToolCalls[i+1:] {
		if isAction(c.Name) {
			if later {
				b.WriteByte('\n')
			}
			b.WriteString("- ")
			writeCall(&b, c)
			later = true
		}
	}
	if !later {
		b.WriteString("(none)")
	}
	return State(b.String())
}

// BuildCompletenessState is the state for checking that the agent handled
// every request the user made: the conversation when the trace has one (else
// the user's request), the agent's actions in order (lookups left out), and
// its final answer. The agent's turns show what it declined, and requests the
// user withdrew.
func BuildCompletenessState(t Trace, isAction func(name string) bool) State {
	var b strings.Builder
	b.WriteString(HandledNote)
	if turns := conversationText(t.Conversation); turns != "" {
		b.WriteString("\n\nCONVERSATION:\n")
		b.WriteString(turns)
	} else {
		b.WriteString("\n\nUSER REQUEST:\n")
		b.WriteString(t.Question)
	}
	b.WriteString("\n\nAGENT ACTIONS:\n")
	n := 0
	for _, c := range t.ToolCalls {
		if !isAction(c.Name) {
			continue
		}
		if n > 0 {
			b.WriteByte('\n')
		}
		n++
		b.WriteString(strconv.Itoa(n))
		b.WriteString(". ")
		writeCall(&b, c)
		if c.StatusText() != "ok" && strings.TrimSpace(c.Output) != "" {
			b.WriteString(": ")
			b.WriteString(truncateRunes(strings.TrimSpace(c.Output), maxStatusChars))
		}
	}
	if n == 0 {
		b.WriteString("(none)")
	}
	b.WriteString("\n\nFINAL ANSWER:\n")
	b.WriteString(t.FinalAnswer)
	return State(b.String())
}

// conversationText is the turns as USER: and AGENT: lines, agent turns cut to
// maxAgentTurnChars, the earliest turns dropped beyond maxConversationChars.
func conversationText(turns []Message) string {
	lines := make([]string, 0, len(turns))
	for _, m := range turns {
		text := strings.TrimSpace(m.Content)
		if m.Role == "assistant" {
			if len(text) > maxAgentTurnChars {
				text = truncateRunes(text, maxAgentTurnChars) + " [...]"
			}
			lines = append(lines, "AGENT: "+text)
		} else {
			lines = append(lines, "USER: "+text)
		}
	}
	total := 0
	start := len(lines)
	for start > 0 && total+len(lines[start-1]) <= maxConversationChars {
		start--
		total += len(lines[start]) + 2
	}
	return strings.Join(lines[start:], "\n\n")
}

func writeCall(b *strings.Builder, c ToolCall) {
	b.WriteString(c.Name)
	b.WriteByte('(')
	b.WriteString(c.ArgsText())
	b.WriteString(") -> ")
	b.WriteString(c.StatusText())
}
