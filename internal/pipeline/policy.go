package pipeline

import "math/rand/v2"

type PolicyConfig struct {
	KeepThreshold Probability
	// RandomFloor is the share of below-threshold traces kept at random.
	RandomFloor Probability
}

// Policy is safe for concurrent use as long as its random source is.
type Policy struct {
	cfg  PolicyConfig
	rand func() float64
}

// rnd must return uniform values in [0, 1); nil uses math/rand.
func NewPolicy(cfg PolicyConfig, rnd func() float64) *Policy {
	if rnd == nil {
		rnd = rand.Float64
	}
	return &Policy{cfg: cfg, rand: rnd}
}

func (p *Policy) Decide(pKeep Probability) Decision {
	d := Decision{Scored: true, PKeep: pKeep}
	d.Keep, d.Rule = p.keep(pKeep)
	return d
}

func (p *Policy) keep(pKeep Probability) (bool, Rule) {
	switch {
	case pKeep >= p.cfg.KeepThreshold:
		return true, RuleAboveThreshold
	case p.sampled():
		return true, RuleRandomFloor
	}
	return false, RuleBelowThreshold
}

// Fallback keeps a random_floor share, so a scorer outage still yields a
// sample for review.
func (p *Policy) Fallback() Decision {
	if p.sampled() {
		return Decision{Keep: true, Rule: RuleFallback}
	}
	return Decision{Rule: RuleFallback}
}

func (p *Policy) sampled() bool {
	return Probability(p.rand()) < p.cfg.RandomFloor
}
