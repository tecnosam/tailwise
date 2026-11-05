package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/tecnosam/tailwise/internal/trace"
)

// The action checks' answers, recorded and deciding like the manifest's
// questions.
const (
	// ActionWrong is the highest probability, over the trace's action calls,
	// that a call's arguments are not what the user asked for.
	ActionWrong = "action_wrong"
	// UnhandledRequest is the highest probability, over the kinds of action,
	// that the user asked for one and the agent neither did it nor declined:
	// min(requested, 1 − handled).
	UnhandledRequest = "unhandled_request"

	// The causes the action checks give a trace they keep.
	CauseWrongAction      = "wrong_action"
	CauseUnhandledRequest = "unhandled_request"

	// MaxActionChecks bounds the action calls checked in one trace, the most
	// recent kept; actionConcurrency bounds the calls in flight.
	MaxActionChecks   = 10
	actionConcurrency = 4
)

// Actions configures the action checks for a model: which tools act for the
// user, and the questions asked about them.
type Actions struct {
	// Tools maps each action tool to what it does, worded as a request
	// ("cancel a pending order").
	Tools map[string]string
	// ActionQuestions is asked about one action call; ActionWrong is among
	// them.
	ActionQuestions json.RawMessage
	// CompletenessQuestions asks, for each tool, RequestedQuestion(tool) and
	// HandledQuestion(tool).
	CompletenessQuestions json.RawMessage
}

func RequestedQuestion(tool string) string { return "requested_" + tool }
func HandledQuestion(tool string) string   { return "handled_" + tool }

func (a *Actions) isAction(name string) bool {
	_, ok := a.Tools[name]
	return ok
}

// Asker asks a model questions of the caller's own about a state of its own;
// a Scorer that implements it can run the action checks.
type Asker interface {
	Ask(ctx context.Context, state trace.State, questions json.RawMessage) (Answers, error)
}

type Answers struct {
	P           map[string]Probability
	InputTokens int
}

// actionFindings is what the action checks found, for the reason.
type actionFindings struct {
	actionWrong Probability
	action      string // the call with the highest ActionWrong, as name(args)
	unhandled   Probability
	request     string // the kind of request most likely unhandled
	inputTokens int
	checked     bool // at least one check answered
}

// checkActions asks about each action call (the last MaxActionChecks) and
// about completeness, concurrently. A check that fails is reported and left
// out, unless on the queued path a system error should redeliver the trace.
func (e *Engine) checkActions(ctx context.Context, t trace.Trace, model Model, queued bool) (actionFindings, error) {
	asker, ok := model.Scorer.(Asker)
	if !ok || model.Actions == nil {
		return actionFindings{}, nil
	}
	a := model.Actions
	var calls []int
	for i, c := range t.ToolCalls {
		if a.isAction(c.Name) {
			calls = append(calls, i)
		}
	}
	if len(calls) > MaxActionChecks {
		calls = calls[len(calls)-MaxActionChecks:]
	}

	type job struct {
		state     trace.State
		questions json.RawMessage
		call      int // -1 for completeness
	}
	jobs := []job{{trace.BuildCompletenessState(t, a.isAction), a.CompletenessQuestions, -1}}
	for _, i := range calls {
		jobs = append(jobs, job{trace.BuildActionState(t, i, a.isAction), a.ActionQuestions, i})
	}

	var (
		mu    sync.Mutex
		f     actionFindings
		fatal error
		wg    sync.WaitGroup
		slots = make(chan struct{}, actionConcurrency)
	)
	for _, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			var ans Answers
			err := e.withRetry(ctx, t, model, queued, "ask action check", func() error {
				var err error
				ans, err = asker.Ask(ctx, j.state, j.questions)
				return err
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if queued && isSystemError(err) {
					fatal = err
				}
				e.reportCheckFailure(ctx, t, model, err)
				return
			}
			f.inputTokens += ans.InputTokens
			f.checked = true
			if j.call >= 0 {
				if p, ok := ans.P[ActionWrong]; ok && (p > f.actionWrong || f.action == "") {
					f.actionWrong, f.action = p, callText(t.ToolCalls[j.call])
				}
				return
			}
			for _, tool := range slices.Sorted(maps.Keys(a.Tools)) {
				req, okR := ans.P[RequestedQuestion(tool)]
				handled, okH := ans.P[HandledQuestion(tool)]
				if !okR || !okH {
					continue
				}
				if p := min(req, 1-handled); p > f.unhandled || f.request == "" {
					f.unhandled, f.request = p, a.Tools[tool]
				}
			}
		}()
	}
	wg.Wait()
	return f, fatal
}

// apply adds the findings to the score: as answers, as deciding answers
// (p_keep is the highest), and in the input billed. A trace with no action
// call has no wrong action: its action_wrong is 0, not missing, since a
// missing answer means the check failed.
func (f actionFindings) apply(score *Score, hasActionCalls bool) {
	if score.Answers == nil {
		score.Answers = map[string]Probability{}
	}
	if score.Deciding == nil {
		score.Deciding = map[string]Probability{}
	}
	if !hasActionCalls {
		score.Answers[ActionWrong], score.Deciding[ActionWrong] = 0, 0
	}
	if !f.checked {
		return
	}
	if hasActionCalls && f.action != "" {
		score.Answers[ActionWrong], score.Deciding[ActionWrong] = f.actionWrong, f.actionWrong
		score.P = max(score.P, f.actionWrong)
	}
	if f.request != "" {
		score.Answers[UnhandledRequest], score.Deciding[UnhandledRequest] = f.unhandled, f.unhandled
		score.P = max(score.P, f.unhandled)
	}
	score.InputTokens += f.inputTokens
}

// reason is the plain-English reason when an action check, not the decision
// question, kept the trace.
func (f actionFindings) reason(flag string) (cause, reason string) {
	switch flag {
	case ActionWrong:
		return CauseWrongAction, fmt.Sprintf("The agent's action %s does not match what the user asked for.", f.action)
	case UnhandledRequest:
		return CauseUnhandledRequest, fmt.Sprintf("The user asked the agent to %s, and it neither did it nor declined.", f.request)
	}
	return "", ""
}

func callText(c trace.ToolCall) string {
	return c.Name + "(" + c.ArgsText() + ")"
}
