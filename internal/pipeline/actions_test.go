package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/tecnosam/tailwise/internal/fault"
	"github.com/tecnosam/tailwise/internal/trace"
)

// askScorer answers the decision question with decide, each action check
// from actionWrong (by tool name), and completeness from requested and
// handled (by tool name). err, when set, fails every action check.
type askScorer struct {
	decide      Probability
	actionWrong map[string]Probability
	requested   map[string]Probability
	handled     map[string]Probability
	err         error

	mu    sync.Mutex
	asked int
}

func (s *askScorer) Score(context.Context, trace.State) (Score, error) {
	p := map[string]Probability{"answer_wrong": s.decide}
	return Score{P: s.decide, Answers: p, Deciding: p, Cause: "correct", Explanation: "model reason", InputTokens: 100}, nil
}

func (s *askScorer) Ready(context.Context) error { return nil }

func (s *askScorer) Ask(_ context.Context, state trace.State, _ json.RawMessage) (Answers, error) {
	s.mu.Lock()
	s.asked++
	s.mu.Unlock()
	if s.err != nil {
		return Answers{}, s.err
	}
	p := map[string]Probability{}
	if strings.HasPrefix(string(state), trace.HandledNote) {
		for tool, r := range s.requested {
			p[RequestedQuestion(tool)], p[HandledQuestion(tool)] = r, s.handled[tool]
		}
		return Answers{P: p, InputTokens: 10}, nil
	}
	action := strings.SplitN(strings.SplitN(string(state), "ACTION:\n", 2)[1], "(", 2)[0]
	p[ActionWrong] = s.actionWrong[action]
	return Answers{P: p, InputTokens: 10}, nil
}

var retailActions = &Actions{Tools: map[string]string{"exchange": "exchange an item", "cancel": "cancel an order"}}

func actionModel(s Scorer) Model {
	return Model{Name: "m", Scorer: s, Actions: retailActions,
		Checks: []Check{{Question: "answer_wrong"}, {Question: ActionWrong}, {Question: UnhandledRequest}}}
}

func actionTrace() trace.Trace {
	return trace.Trace{TraceID: "t", Question: "Exchange my keyboard for size L, and cancel #W2.", FinalAnswer: "Done.",
		ToolCalls: []trace.ToolCall{
			{Name: "get_product", Args: json.RawMessage(`{"id":"77"}`), Output: `{"2222":"M","3333":"L"}`},
			{Name: "exchange", Args: json.RawMessage(`{"new_item_ids":["2222"]}`), Output: "ok"},
		}}
}

// A wrong action keeps a trace the decision question passed, with a reason
// naming the action.
func TestActionCheckKeepsAWrongAction(t *testing.T) {
	s := &askScorer{decide: 0.2, actionWrong: map[string]Probability{"exchange": 0.8}}
	e := NewEngine(Deps{Models: SingleModel(actionModel(s)), Policy: keepAtHalf(), Exporter: nowhere{}, Logger: quiet})
	rec := e.Process(context.Background(), actionTrace()).Record
	d := rec.Decision
	if !d.Keep || d.PKeep != 0.8 || d.Cause != CauseWrongAction || !strings.Contains(d.Reason, `exchange({"new_item_ids":["2222"]})`) {
		t.Errorf("decision = %+v", d)
	}
	if len(rec.Flags) != 1 || rec.Flags[0] != ActionWrong || rec.Answers[ActionWrong] != 0.8 {
		t.Errorf("flags %q, answers %v", rec.Flags, rec.Answers)
	}
	// One action call and the completeness check, billed on top of the score.
	if s.asked != 2 || rec.InputTokens != 120 {
		t.Errorf("asked %d, input tokens %d", s.asked, rec.InputTokens)
	}
}

// A request neither done nor declined keeps the trace; one the agent handled
// does not.
func TestCompletenessCheckKeepsAnUnhandledRequest(t *testing.T) {
	s := &askScorer{decide: 0.2,
		requested: map[string]Probability{"exchange": 0.9, "cancel": 0.9},
		handled:   map[string]Probability{"exchange": 0.9, "cancel": 0.1}}
	e := NewEngine(Deps{Models: SingleModel(actionModel(s)), Policy: keepAtHalf(), Exporter: nowhere{}, Logger: quiet})
	d := e.Process(context.Background(), actionTrace()).Record.Decision
	if !d.Keep || d.Cause != CauseUnhandledRequest || !strings.Contains(d.Reason, "cancel an order") {
		t.Errorf("decision = %+v", d)
	}

	s.handled["cancel"] = 0.9
	if d := e.Process(context.Background(), actionTrace()).Record.Decision; d.Keep {
		t.Errorf("all requests handled: decision = %+v", d)
	}
}

// When the decision question keeps the trace, its reason stands.
func TestDecisionQuestionReasonWins(t *testing.T) {
	s := &askScorer{decide: 0.9, actionWrong: map[string]Probability{"exchange": 0.8}}
	e := NewEngine(Deps{Models: SingleModel(actionModel(s)), Policy: keepAtHalf(), Exporter: nowhere{}, Logger: quiet})
	if d := e.Process(context.Background(), actionTrace()).Record.Decision; d.Reason != "model reason" || d.Cause != "correct" {
		t.Errorf("decision = %+v", d)
	}
}

// A failed check is left out: the synchronous path decides without it, the
// queued path redelivers on a system error.
func TestActionCheckFailure(t *testing.T) {
	s := &askScorer{decide: 0.2, err: fault.Unavailable(fault.Scorer, "score trace", errors.New("down"))}
	e := NewEngine(Deps{Models: SingleModel(actionModel(s)), Policy: keepAtHalf(), Exporter: nowhere{}, Logger: quiet})
	rec := e.Process(context.Background(), actionTrace()).Record
	if !rec.Decision.Scored || rec.Decision.Keep || rec.Answers[ActionWrong] != 0 {
		t.Errorf("sync: record = %+v", rec)
	}
	if _, err := e.ProcessQueued(context.Background(), actionTrace()); err == nil {
		t.Error("queued: a system error in a check did not redeliver")
	}
}

// Severity ranks a trace two checks doubt above one a single check is sure
// of, though p_keep, the highest answer, says the opposite.
func TestSeverityRanksAgreementFirst(t *testing.T) {
	severity := func(s *askScorer) (Probability, float64) {
		e := NewEngine(Deps{Models: SingleModel(actionModel(s)), Policy: keepAtHalf(), Exporter: nowhere{}, Logger: quiet})
		d := e.Process(context.Background(), actionTrace()).Record.Decision
		return d.PKeep, d.Severity
	}
	sureP, sure := severity(&askScorer{decide: 0.1, actionWrong: map[string]Probability{"exchange": 0.95}})
	agreeP, agree := severity(&askScorer{decide: 0.7, actionWrong: map[string]Probability{"exchange": 0.7}})
	if !(agree > sure && agreeP < sureP) {
		t.Errorf("two checks at 0.7: p_keep %v, severity %v; one at 0.95: p_keep %v, severity %v", agreeP, agree, sureP, sure)
	}
}

// A failed check is left out of severity, not counted as 0; a trace with no
// action call has action_wrong 0, which does count.
func TestSeverityLeavesOutFailedChecks(t *testing.T) {
	process := func(s *askScorer, tr trace.Trace) Record {
		e := NewEngine(Deps{Models: SingleModel(actionModel(s)), Policy: keepAtHalf(), Exporter: nowhere{}, Logger: quiet})
		return e.Process(context.Background(), tr).Record
	}
	failed := &askScorer{decide: 0.6, err: fault.Unavailable(fault.Scorer, "score trace", errors.New("down"))}
	if got := process(failed, actionTrace()).Decision.Severity; math.Abs(got-0.6) > 1e-9 {
		t.Errorf("checks failed: severity %v, want 0.6", got)
	}
	noAction := actionTrace()
	noAction.ToolCalls = noAction.ToolCalls[:1]
	s := &askScorer{decide: 0.6, requested: map[string]Probability{"exchange": 0.9}, handled: map[string]Probability{"exchange": 0}}
	rec := process(s, noAction)
	if a, ok := rec.Answers[ActionWrong]; !ok || a != 0 || math.Abs(rec.Decision.Severity-0.5) > 1e-9 {
		t.Errorf("no action call: action_wrong %v (present %v), severity %v, want 0 and 0.5", a, ok, rec.Decision.Severity)
	}
}

// Without an actions block, no check runs.
func TestNoActionsNoChecks(t *testing.T) {
	s := &askScorer{decide: 0.2}
	m := actionModel(s)
	m.Actions = nil
	e := NewEngine(Deps{Models: SingleModel(m), Policy: keepAtHalf(), Exporter: nowhere{}, Logger: quiet})
	e.Process(context.Background(), actionTrace())
	if s.asked != 0 {
		t.Errorf("asked %d", s.asked)
	}
}
