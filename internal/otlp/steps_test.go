package otlp

import (
	"slices"
	"strings"
	"testing"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/tecnosam/tailwise/internal/trace"
)

// codeAgentRun is the shape of a smolagents CodeAgent run: each Step is a
// CHAIN span whose LLM child wrote code, and whose output is what the code
// printed. The last step calls final_answer, a TOOL span.
func codeAgentRun() []*tracepb.Span {
	failed := span(5, 1, "Step 2", 300, attrSpanKind, kindChain)
	failed.Status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: "AgentExecutionError: SyntaxError"}
	return []*tracepb.Span{
		span(1, 0, "CodeAgent.run", 100, attrSpanKind, kindAgent, attrInputValue, `{"task": "Fix the bug."}`, attrOutputValue, "Done."),
		span(2, 1, "Step 1", 200, attrSpanKind, kindChain, attrOutputValue, "Execution logs:\nsrc/app.py"),
		span(3, 2, "LiteLLMModel.__call__", 210, attrSpanKind, kindLLM,
			llmOutputMessageContent, "Thought: list the files.\nCode:\n```py\nprint(ls())\n```<end_code>"),
		failed,
		span(6, 5, "LiteLLMModel.__call__", 310, attrSpanKind, kindLLM, llmOutputMessageContent, "Code:\n<code>print(f'{x\\n}')</code>"),
		// A step whose model wrote no code is not an action.
		span(7, 1, "Step 3", 400, attrSpanKind, kindChain, attrOutputValue, "thinking"),
		span(8, 7, "LiteLLMModel.__call__", 410, attrSpanKind, kindLLM, llmOutputMessageContent, "Let me think."),
		// The last step's code called a tool: the tool is the call.
		span(9, 1, "Step 4", 500, attrSpanKind, kindChain, attrOutputValue, "Done."),
		span(10, 9, "LiteLLMModel.__call__", 510, attrSpanKind, kindLLM, llmOutputMessageContent, "```py\nfinal_answer('Done.')\n```"),
		span(11, 9, "FinalAnswerTool", 520, attrSpanKind, kindTool, attrToolName, "final_answer",
			attrInputValue, `{"answer": "Done."}`, attrOutputValue, "Done."),
	}
}

func TestAssembleCodeAgentSteps(t *testing.T) {
	got, err := assemble(t, codeAgentRun())
	if err != nil {
		t.Fatal(err)
	}
	if got.Question != "Fix the bug." {
		t.Errorf("question = %q", got.Question)
	}
	want := "TOOL CALLS:\n" +
		"1. execute_code({\"code\":\"print(ls())\\n\"}) -> ok:\n   Execution logs:\n   src/app.py\n" +
		"2. execute_code({\"code\":\"print(f'{x\\\\n}')\"}) -> error: AgentExecutionError: SyntaxError\n" +
		"3. final_answer({\"answer\":\"Done.\"}) -> ok: Done.\n\n"
	if state := string(trace.BuildStateGrounded(got, 12000)); !strings.Contains(state, want) {
		t.Errorf("state =\n%s\nwant it to contain\n%s", state, want)
	}
	// The default evidence holds the steps' output too.
	if len(got.Evidence) == 0 || got.Evidence[0] != "Execution logs:\nsrc/app.py" {
		t.Errorf("evidence = %q", got.Evidence)
	}
}

// Under a root that is not an AGENT span, CHAIN spans are left alone.
func TestAssembleStepsOnlyUnderAnAgent(t *testing.T) {
	spans := codeAgentRun()
	spans[0] = span(1, 0, "pipeline", 100, attrSpanKind, kindChain, attrInputValue, "q", attrOutputValue, "a")
	got, err := assemble(t, spans)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].Name != "final_answer" {
		t.Errorf("tool calls = %+v, want only final_answer", got.ToolCalls)
	}
}

// The conversation comes from the LLM call with the longest history: its
// user and assistant messages, then its reply.
func TestAssembleConversation(t *testing.T) {
	spans := []*tracepb.Span{
		span(1, 0, "run", 100, attrSpanKind, kindAgent, attrInputValue, "Cancel #W2.", attrOutputValue, "Cancelled."),
		span(2, 1, "llm", 200, attrSpanKind, kindLLM,
			"llm.input_messages.0.message.role", "system", "llm.input_messages.0.message.content", "Be kind.",
			"llm.input_messages.1.message.role", "user", "llm.input_messages.1.message.content", "Cancel #W2."),
		span(3, 1, "llm", 300, attrSpanKind, kindLLM,
			"llm.input_messages.0.message.role", "system", "llm.input_messages.0.message.content", "Be kind.",
			"llm.input_messages.1.message.role", "user", "llm.input_messages.1.message.content", "Cancel #W2.",
			"llm.input_messages.2.message.role", "assistant", "llm.input_messages.2.message.content", "Are you sure?",
			"llm.input_messages.3.message.role", "tool", "llm.input_messages.3.message.content", "{}",
			"llm.input_messages.4.message.role", "user", "llm.input_messages.4.message.content", "Yes.",
			outputMessageContent, "Cancelled."),
	}
	got, err := assemble(t, spans)
	if err != nil {
		t.Fatal(err)
	}
	want := []trace.Message{{Role: "user", Content: "Cancel #W2."}, {Role: "assistant", Content: "Are you sure?"},
		{Role: "user", Content: "Yes."}, {Role: "assistant", Content: "Cancelled."}}
	if !slices.Equal(got.Conversation, want) {
		t.Errorf("conversation = %+v", got.Conversation)
	}
}
