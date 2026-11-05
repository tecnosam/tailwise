package pipeline

// Check is one question that can keep a trace: an answer at or above its
// threshold flags the trace with the question.
type Check struct {
	Question string
	// Threshold is 0 to use the policy's keep threshold.
	Threshold Probability
}
