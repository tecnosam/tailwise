package pipeline

// Check is one question that can keep a trace: an answer at or above its
// threshold flags the trace with the question.
type Check struct {
	Question string
	// Threshold is 0 to use the policy's keep threshold.
	Threshold Probability
	// Weight is the question's share of severity, from its priority; see
	// PriorityWeights.
	Weight float64
}

// PriorityWeights gives n questions, ranked 1 (most important) to n, their
// rank-order centroid weights: wᵢ = (1/n) Σ_{k=i..n} 1/k. They sum to 1 and
// fall with rank, steeply at the top: for 3 questions 0.61, 0.28 and 0.11.
func PriorityWeights(n int) []float64 {
	w := make([]float64, n)
	sum := 0.0
	for i := n; i >= 1; i-- {
		sum += 1 / float64(i)
		w[i-1] = sum / float64(n)
	}
	return w
}
