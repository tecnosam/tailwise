package otlp

import (
	"cmp"
	"encoding/json"
	"regexp"
	"slices"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/tecnosam/tailwise/internal/trace"
)

// stepToolName names a code step as a tool call.
const stepToolName = "execute_code"

const llmOutputMessageContent = "llm.output_messages.0.message.content"

// A fenced code block (smolagents' "Code:\n```py ... ```<end_code>") or a
// <code> block (newer smolagents).
var codeBlock = regexp.MustCompile("(?s)```[^\\n]*\\n(.*?)```|<code>(.*?)</code>")

// toolCallsOf is every tool call in start order: the TOOL spans, and the code
// steps of an AGENT root (see codeSteps).
func toolCallsOf(root *tracepb.Span, spans []*tracepb.Span) []trace.ToolCall {
	children := map[string][]*tracepb.Span{}
	for _, s := range spans {
		children[string(s.GetParentSpanId())] = append(children[string(s.GetParentSpanId())], s)
	}
	steps := codeSteps(root, spans, children)
	ordered := append(spansOfKind(spans, kindTool), steps...)
	slices.SortStableFunc(ordered, func(a, b *tracepb.Span) int {
		return cmp.Compare(a.GetStartTimeUnixNano(), b.GetStartTimeUnixNano())
	})
	calls := make([]trace.ToolCall, len(ordered))
	for i, s := range ordered {
		if slices.Contains(steps, s) {
			calls[i] = stepCall(s, children)
		} else {
			calls[i] = toolCall(s)
		}
	}
	return calls
}

// codeSteps are the steps of a code-writing agent, such as a smolagents
// CodeAgent, which runs the code its model writes instead of calling tools:
// each step is a CHAIN span under the AGENT root, its LLM child wrote the
// code, and its output is what running it printed. No TOOL span records the
// action, so without these the model would see only the question and the
// answer. A step with a TOOL span under it is left out: its tools are already
// calls. So is one whose model wrote no code, which keeps other frameworks'
// CHAIN spans out.
func codeSteps(root *tracepb.Span, spans []*tracepb.Span, children map[string][]*tracepb.Span) []*tracepb.Span {
	if attr(root, attrSpanKind) != kindAgent {
		return nil
	}
	byID := make(map[string]*tracepb.Span, len(spans))
	for _, s := range spans {
		byID[string(s.GetSpanId())] = s
	}
	withTools := map[string]bool{} // spans with a TOOL span under them
	for _, s := range spans {
		if attr(s, attrSpanKind) != kindTool {
			continue
		}
		for p := byID[string(s.GetParentSpanId())]; p != nil && !withTools[string(p.GetSpanId())]; p = byID[string(p.GetParentSpanId())] {
			withTools[string(p.GetSpanId())] = true
		}
	}
	var steps []*tracepb.Span
	for _, s := range children[string(root.GetSpanId())] {
		if attr(s, attrSpanKind) == kindChain && !withTools[string(s.GetSpanId())] && stepCode(s, children) != "" {
			steps = append(steps, s)
		}
	}
	return steps
}

// stepCode is the last code block the step's LLM span wrote, "" if none.
func stepCode(step *tracepb.Span, children map[string][]*tracepb.Span) string {
	for _, c := range children[string(step.GetSpanId())] {
		if attr(c, attrSpanKind) != kindLLM {
			continue
		}
		text, ok := lookup(c, llmOutputMessageContent)
		if !ok {
			text = messageText(attr(c, attrOutputValue), outputKeys)
		}
		if blocks := codeBlock.FindAllStringSubmatch(text, -1); len(blocks) > 0 {
			last := blocks[len(blocks)-1]
			return cmp.Or(last[1], last[2])
		}
	}
	return ""
}

// stepCall is a code step as a tool call: the code as its arguments, what it
// printed as its output, or the error that stopped it.
func stepCall(step *tracepb.Span, children map[string][]*tracepb.Span) trace.ToolCall {
	args, _ := json.Marshal(map[string]string{"code": stepCode(step, children)})
	output := messageText(attr(step, attrOutputValue), outputKeys)
	status := toolStatus(step)
	if output == "" && status == "error" {
		output = step.GetStatus().GetMessage()
	}
	call := trace.ToolCall{Name: stepToolName, Args: args, Output: output}
	call.Status, _ = json.Marshal(status)
	return call
}
