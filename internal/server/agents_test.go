package server

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/tecnosam/tailwise/internal/metrics"
	"github.com/tecnosam/tailwise/internal/pipeline"
	"github.com/tecnosam/tailwise/internal/trace"
)

type requests struct {
	mu  sync.Mutex
	got []metrics.Request
}

func (r *requests) RequestFinished(_ context.Context, req metrics.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, req)
}

// A trace that names its agent is scored by that agent's model; the
// response says which agent and model.
func TestScoreRoutesByAgent(t *testing.T) {
	support := &fakeScorer{score: func(context.Context, trace.State) (pipeline.Score, error) {
		return pipeline.Score{P: 0.9, Model: "kev-support"}, nil
	}}
	def := &fakeScorer{score: fixedScore(0.1)}
	reqs := &requests{}
	h := newHarness(t, harnessOpts{
		models: pipeline.NewAgentRouter(pipeline.Model{Name: "kev-latest", Scorer: def},
			map[trace.AgentName]pipeline.Model{"support-bot": {Name: "kev-support", Scorer: support}}),
		metrics: reqs,
	})

	w := h.do("POST", "/v1/traces:score", `{"agent": "support-bot", "question": "q", "final_answer": "a"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	res := decodeBody[Result](t, w)
	if res.Agent != "support-bot" || res.Model != "kev-support" || res.Decision != "keep" {
		t.Errorf("result = %+v", res)
	}
	waitUntil(t, "the kept trace to be exported", func() bool { return len(h.recorder.Ended()) > 0 })
	if a := rootAttrs(t, h.recorder); a["eval.agent"].AsString() != "support-bot" {
		t.Errorf("eval.agent = %q", a["eval.agent"].AsString())
	}

	res = decodeBody[Result](t, h.do("POST", "/v1/traces:score", `{"question": "q", "final_answer": "a"}`))
	if res.Agent != "" || res.Model != "kev-latest" || res.Decision != "drop" {
		t.Errorf("no agent: result = %+v", res)
	}

	reqs.mu.Lock()
	defer reqs.mu.Unlock()
	if len(reqs.got) != 2 || reqs.got[0].Route != "/v1/traces:score" || reqs.got[0].Status != 200 || reqs.got[0].Server != "api" {
		t.Errorf("request metrics = %+v", reqs.got)
	}
}
