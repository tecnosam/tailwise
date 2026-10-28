package pipeline

import (
	"math"
	"strings"
	"testing"
)

func TestPoolCombines(t *testing.T) {
	half := &Pool{Weights: map[string]float64{"a": 0.5, "b": 0.5}}
	for _, tt := range []struct {
		name    string
		pool    *Pool
		answers map[string]Probability
		want    float64
	}{
		{"agreeing answers keep their probability", half, map[string]Probability{"a": 0.9, "b": 0.9}, 0.9},
		{"opposite answers meet in the middle", half, map[string]Probability{"a": 0.9, "b": 0.1}, 0.5},
		{"one question with weight 1 is its answer", &Pool{Weights: map[string]float64{"a": 1}}, map[string]Probability{"a": 0.3}, 0.3},
		{"the intercept shifts the log-odds", &Pool{Intercept: math.Log(9), Weights: map[string]float64{"a": 1}}, map[string]Probability{"a": 0.5}, 0.9},
		{"unweighted answers are ignored", half, map[string]Probability{"a": 0.2, "b": 0.2, "c": 1}, 0.2},
		{"an answer of exactly 1 stays finite", half, map[string]Probability{"a": 1, "b": 0.5}, 0.999},
	} {
		got, err := tt.pool.Combine(tt.answers)
		if err != nil || math.Abs(float64(got)-tt.want) > 1e-3 {
			t.Errorf("%s: got %v, %v; want %v", tt.name, got, err, tt.want)
		}
	}
}

func TestPoolNeedsEveryWeightedAnswer(t *testing.T) {
	p := &Pool{Weights: map[string]float64{"a": 0.5, "b": 0.5}}
	if _, err := p.Combine(map[string]Probability{"a": 0.9}); err == nil || !strings.Contains(err.Error(), `no answer for "b"`) {
		t.Errorf("error = %v", err)
	}
}
