package pipeline

import (
	"fmt"
	"maps"
	"math"
	"slices"
)

// Pool combines the model's answers into p_keep as a weighted logarithmic
// opinion pool:
//
//	p_keep = sigmoid(Intercept + Σ Weights[q] · logit(answer q))
//
// With weights summing to 1 and no intercept, answers that agree keep their
// probability and answers that disagree meet in between. A logistic
// regression fitted on the answers' logits has the same form, so its
// coefficients can be used as the weights.
type Pool struct {
	Intercept float64
	Weights   map[string]float64
}

// poolEpsilon keeps the logit of an answer of exactly 0 or 1 finite.
const poolEpsilon = 1e-6

// Combine needs an answer for every weighted question. Questions are summed
// in name order, so the same answers always give the same p_keep.
func (p *Pool) Combine(answers map[string]Probability) (Probability, error) {
	z := p.Intercept
	for _, q := range slices.Sorted(maps.Keys(p.Weights)) {
		a, ok := answers[q]
		if !ok {
			return 0, fmt.Errorf("model response has no answer for %q", q)
		}
		x := min(max(float64(a), poolEpsilon), 1-poolEpsilon)
		z += p.Weights[q] * math.Log(x/(1-x))
	}
	return Probability(1 / (1 + math.Exp(-z))), nil
}
