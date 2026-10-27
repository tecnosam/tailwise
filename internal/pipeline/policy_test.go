package pipeline

import (
	"testing"
)

var testConfig = PolicyConfig{KeepThreshold: 0.5, RandomFloor: 0.02}

// fixed returns a random source that yields the given values in order and
// fails the test if it is asked for more.
func fixed(t *testing.T, values ...float64) (rnd func() float64, draws *int) {
	t.Helper()
	n := 0
	return func() float64 {
		if n >= len(values) {
			t.Fatalf("random source drawn %d times, only %d values provided", n+1, len(values))
		}
		v := values[n]
		n++
		return v
	}, &n
}

func TestDecide(t *testing.T) {
	tests := []struct {
		name      string
		pKeep     Probability
		draw      []float64 // values the random source will return
		wantKeep  bool
		wantWhy   Rule
		wantDraws int
	}{
		{"kev keep", 0.93, nil, true, RuleAboveThreshold, 0},
		{"kev keep, well above", 0.8, nil, true, RuleAboveThreshold, 0},
		{"kev keep, just above", 0.65, nil, true, RuleAboveThreshold, 0},
		{"kev keep at threshold", 0.5, nil, true, RuleAboveThreshold, 0},
		{"random-floor keep", 0.1, []float64{0.01}, true, RuleRandomFloor, 1},
		{"drop just below threshold", 0.4999, []float64{0.5}, false, RuleBelowThreshold, 1},
		{"drop at floor boundary", 0.1, []float64{0.02}, false, RuleBelowThreshold, 1},
		{"drop", 0.1, []float64{0.9}, false, RuleBelowThreshold, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rnd, draws := fixed(t, tt.draw...)
			d := NewPolicy(testConfig, rnd).Decide(tt.pKeep)
			if d.Keep != tt.wantKeep || d.Rule != tt.wantWhy {
				t.Errorf("decision = keep:%v rule:%s, want keep:%v rule:%s", d.Keep, d.Rule, tt.wantKeep, tt.wantWhy)
			}
			if !d.Scored || d.PKeep != tt.pKeep {
				t.Errorf("probabilities not recorded: %+v", d)
			}
			if *draws != tt.wantDraws {
				t.Errorf("random draws = %d, want %d", *draws, tt.wantDraws)
			}
		})
	}
}

func TestFallback(t *testing.T) {
	rnd, _ := fixed(t, 0.01, 0.5)
	p := NewPolicy(testConfig, rnd)

	keep := p.Fallback()
	if !keep.Keep || keep.Rule != RuleFallback || keep.Scored {
		t.Errorf("fallback keep = %+v", keep)
	}
	drop := p.Fallback()
	if drop.Keep || drop.Rule != RuleFallback || drop.Scored {
		t.Errorf("fallback drop = %+v", drop)
	}
}

func TestRandomFloorExtremes(t *testing.T) {
	never := NewPolicy(PolicyConfig{KeepThreshold: 0.5, RandomFloor: 0}, func() float64 { return 0 })
	if d := never.Decide(0.1); d.Keep {
		t.Errorf("random_floor 0 kept a trace: %+v", d)
	}
	if d := never.Fallback(); d.Keep {
		t.Errorf("random_floor 0 kept a fallback trace: %+v", d)
	}

	always := NewPolicy(PolicyConfig{KeepThreshold: 0.5, RandomFloor: 1}, func() float64 { return 0.999999 })
	if d := always.Decide(0.1); !d.Keep || d.Rule != RuleRandomFloor {
		t.Errorf("random_floor 1 dropped a trace: %+v", d)
	}
}

func TestDefaultRandomSourceRate(t *testing.T) {
	p := NewPolicy(PolicyConfig{KeepThreshold: 0.5, RandomFloor: 0.2}, nil)
	kept := 0
	const n = 20000
	for i := 0; i < n; i++ {
		if p.Decide(0.1).Keep {
			kept++
		}
	}
	if rate := float64(kept) / n; rate < 0.17 || rate > 0.23 {
		t.Errorf("random-floor keep rate = %.3f, want about 0.20", rate)
	}
}
