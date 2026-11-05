package pipeline

import (
	"math/rand/v2"
	"slices"
)

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
	d := Decision{Scored: true, PKeep: pKeep, Severity: float64(pKeep)}
	d.Keep, d.Rule = p.keepIf(pKeep >= p.cfg.KeepThreshold)
	return d
}

// DecideChecks keeps a trace that any check flags. Flags are in the checks'
// order. Severity is the average answer of the checks that answered, flagged
// or not: a check that failed is left out rather than counted as 0, so an
// outage does not sink a failed trace down the review order. Signals, the
// signal rules' 1 or 0, join the average and decide nothing. With no checks,
// p_keep alone decides, as in Decide.
func (p *Policy) DecideChecks(pKeep Probability, checks []Check, answers map[string]Probability, signals ...Probability) (Decision, []string) {
	if len(checks) == 0 {
		d := p.Decide(pKeep)
		d.Severity = average(append([]Probability{pKeep}, signals...))
		return d, nil
	}
	d := Decision{Scored: true, PKeep: pKeep}
	flags := []string{}
	counted := slices.Clone(signals)
	for _, c := range checks {
		threshold := c.Threshold
		if threshold == 0 {
			threshold = p.cfg.KeepThreshold
		}
		a, ok := answers[c.Question]
		if !ok {
			continue
		}
		if a >= threshold {
			flags = append(flags, c.Question)
		}
		counted = append(counted, a)
	}
	d.Severity = average(counted)
	d.Keep, d.Rule = p.keepIf(len(flags) > 0)
	return d, flags
}

func average(ps []Probability) float64 {
	if len(ps) == 0 {
		return 0
	}
	var sum float64
	for _, p := range ps {
		sum += float64(p)
	}
	return sum / float64(len(ps))
}

func (p *Policy) keepIf(above bool) (bool, Rule) {
	switch {
	case above:
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
