# Export kept traces in order of severity

**Labels:** enhancement, export

## Problem

Kept traces are exported in the order they are decided. When more traces are
kept than reviewers can look at, the ones that reach them first should be the
ones with the worst problems, not the ones that happened to finish first.

Take two kept traces:

- Trace 1 is flagged `answer_wrong` and `unsupported_answer`.
- Trace 2 is flagged `answer_wrong` and `contradicts_knowledge`.

Both are worth reviewing, but if `unsupported_answer` ranks above
`contradicts_knowledge` for this agent, trace 1 should go first.

## What exists today

- Each manifest's `decision` block ranks the questions that can keep a trace:
  the decision question first, then `any_of` in the order written. Each
  question can have its own threshold (`decision.thresholds`).
- Ranks become rank-order centroid weights (`pipeline.PriorityWeights`).
- Every kept trace gets a **severity**: the sum of weight × answer over its
  flags (`pipeline.Policy.DecideChecks`). It is in the decision log
  (`severity`) and on the exported root span (`eval.severity`).
- Nothing orders by severity yet. The export queues
  (`internal/export/natsq`) are JetStream work queues and deliver first in,
  first out.

## Proposal

Have each exporter send higher-severity traces first.

JetStream streams are strictly ordered, so a priority queue needs one of these
designs:

1. **Priority bands.** Publish each record to one of a few subjects by
   severity (for example `exports.<name>.high`, `.medium` and `.low`), with
   one consumer per band. Exporter workers pull from the highest band that
   has messages. This is simple and durable, and the ordering inside a band
   stays first in, first out.
2. **A reordering window.** Pull a batch, sort it by severity and send in
   that order. This is exact within the window, and adds latency equal to
   the window.
3. **Budgeted keeping.** Instead of keeping everything above threshold, keep
   the top N by severity per period (for example per hour, per agent). This
   matters most when review capacity is a fixed number rather than a share.

Option 1 fits the current queue design best.

## Things to decide

- **Calibrating severity across questions.** Severity uses the raw answers.
  A question with a high threshold (say 0.9) that just passes contributes
  almost as much as one that passes a 0.5 threshold with a 0.95 answer.
  Consider using how far each answer is past its threshold,
  `(answer − threshold) / (1 − threshold)`, instead of the raw answer. Once
  labelled traces exist, calibrate each question's answers against the
  labels.
- **Starvation.** If high-severity traces keep arriving, low ones may never
  be sent. Age records upward, or cap how long a record can wait.
- **Random-floor samples.** Traces kept by `random_floor` have no flags and a
  severity of 0. They are the unbiased sample used to measure what the
  checks miss, so they must not wait behind everything else forever. Give
  them their own band, or a guaranteed share.
- **Is a queue needed at all?** If the back end can sort (Phoenix can filter
  and sort on `eval.severity`), reviewers may get most of the value without
  changing export. A priority queue matters when export itself is the
  bottleneck, or when only part of the kept traces should be sent.

## Related

- Sampling per flag: a separate random floor for each question, so every
  kind of problem gets its own sample for review. It would use the same
  bands.

## Done when

- Exporters send higher-severity traces before lower ones, by one of the
  designs above.
- Random-floor samples still reach every back end within a bounded delay.
- The README documents the ordering and how to tune it, and the tests cover
  ordering, starvation and restarts (queued records keep their band).
