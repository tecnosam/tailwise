# Tailwise benchmark

Tailwise's job is to pick, from many agent runs, the ones worth a human's
review: the runs that failed. This harness measures how well it does that.
Each labelled run is scored once, the way production would score it, and
the result is compared with keeping runs at random.

Every score goes through the real service. The harness starts
`bin/tailwise`, sends each run as OTLP spans to
`POST /v1/otlp/traces:score`, and reads back the decision, every answer and
the input tokens Jev billed.

| Dataset | Runs | A run failed when |
| --- | --- | --- |
| [τ-bench](https://github.com/sierra-research/tau-bench) (MIT) | 1,980 customer-service runs (retail and airline; gpt-4o and claude-3.5-sonnet), about 40% failed | Its reward is below 1: the database did not end in the right state, or the agent did not tell the user what it had to. |
| [SWE-agent trajectories](https://huggingface.co/datasets/nebius/SWE-agent-trajectories) (CC-BY-4.0) | A seeded sample of 1,000 coding runs on real GitHub issues, about 83% failed | Its patch did not make the issue's tests pass. |
| [TRAIL](https://huggingface.co/datasets/PatronusAI/TRAIL) (gated, evaluation only), optional | 148 GAIA and SWE-bench traces | It has a HIGH-impact error. TRAIL labels errors, not outcomes, and nearly every trace has some error, so "any error" would leave almost nothing to tell apart. |

## Setup

```sh
make build                      # in the repository root: bin/tailwise
cd bench
uv sync                         # Python 3.11+, the dependencies, the bench command
```

| Variable | Needed for |
| --- | --- |
| `JEV_API_KEY` | Scoring. By default the harness runs Tailwise under `envchain observer-ai`, so the key never appears in the environment or a file. Set `tailwise.envchain: ""` to use `JEV_API_KEY` from the environment instead. |
| `HF_TOKEN` | Only for TRAIL, which is gated. Accept its terms on Hugging Face, then set `HF_TOKEN` or run `hf auth login`. τ-bench and SWE-agent need no token. |

## Running

```sh
uv run bench run --dry-run                      # projected tokens and cost, no API calls
uv run bench run --limit 5                      # a smoke test: 5 runs per dataset
uv run bench run                                # τ-bench and SWE-agent, in full
uv run bench run --datasets tau,swe,trail       # with TRAIL, once you have access
uv run bench run --config path/to/config.yaml   # another config
```

Per-run overrides: `--manifest manifests/answer-wrong-only.json` (other
questions), `--evidence first` (Tailwise's older evidence strategy; `grounded`
is the default),
`--budget 1.00` (a hard cap for this run), and `--baseline <folder>` with
`--notes <file>` to add a before/after comparison to the report.

`uv run bench investigate` runs the offline τ-bench diagnosis. It never calls
Jev. It captures the exact requests Tailwise would send under several
settings, checks every write call's arguments against the user's messages,
the policy and earlier tool results, and compares writes with the task's gold
actions. The output lands in `results/<timestamp>_investigation/`.

`heldout.yaml` samples SWE-agent runs from a parquet file no other run has
read. Use it to confirm a change designed on the main datasets:
`uv run bench run --config heldout.yaml --datasets swe`.

The exit status is 0 on success, 3 when the run stopped at the budget cap,
and 2 for bad arguments.

`uv run pytest` runs the unit tests, and `uv run mypy src` type-checks the
code. Neither needs the network.

## What Tailwise sees

Each run becomes the spans an OpenInference-instrumented agent emits for the
parts Tailwise reads:

- an AGENT root span whose input is the user's request and whose output is
  the agent's final answer;
- an LLM span whose system message (`llm.input_messages.0`) is the agent's
  system prompt, where instrumentors record it. Tailwise shows it to the
  model only with `tailwise.system_prompt: true` in the bench config, its
  `scorer.system_prompt`; it is off by default, as in Tailwise;
- one TOOL span per tool call, with its arguments, its output, and an error
  status when the call failed.

| Dataset | System prompt | Question | Final answer | Tool calls |
| --- | --- | --- | --- | --- |
| τ-bench | The domain's policy | The user's messages, in order | The agent's last message | Each function call with its result; a result starting with "Error" is a failed call |
| SWE-agent | SWE-agent's system message | The issue text | The submitted patch | Each shell command with its output |
| TRAIL | Its own spans, unchanged except that the outermost AGENT span becomes a root, because TRAIL's top span has no input or output for Tailwise to read | | | |

## Metrics

Each dataset and each subset (domain and model for τ-bench, model for
SWE-agent) gets the metrics below. Two decision rules come from the same
call:

- **Tailwise (default):** the built-in questions and decision rule as
  shipped. A run is kept when any check reaches the threshold (0.5), and its
  score is `p_keep`.
- **answer_wrong only:** the decision question alone, read from the recorded
  answers.

**At the threshold** (no random floor):

| Metric | What it says |
| --- | --- |
| Kept | Share of runs kept |
| Kept that failed (precision) | Of the runs kept, the share that failed. Random keeping gets the failure rate. |
| Lift | Precision ÷ failure rate |
| Failures caught (recall) | Of the runs that failed, the share kept |
| Successes kept (false positive rate) | Of the runs that succeeded, the share kept |
| At production failure rates | The datasets fail far more often than most production agents. The catch rate and false positive rate do not depend on the failure rate, so the harness also reports the precision they would give if 2, 5, 10 or 20% of runs failed. |

**Ranking** (for review queues sorted by score):

- **Budget curve.** At budgets of 1, 5, 10, 20, 30 and 50% of runs, it gives
  the share of failures found, and the share of reviewed runs that failed,
  when reviewing the highest-scored runs first. Each is compared with the
  same number of random runs, averaged over 100 seeded runs and shown with
  its 95% interval. The curve also shows the best a perfect ranker could do.
- **ROC-AUC and PR-AUC.**
- **Calibration:** whether `p_keep` behaves like a probability of failure,
  shown as a reliability diagram with its ECE.

**Trace-detectable runs (τ-bench).** Some τ-bench labels can't be judged
from the trace, whatever reads it. The report adds a "trace-detectable runs"
row that leaves those runs out:

- runs on tasks whose logged gold actions disagree with at least one
  successful run: there, the gold says little about how runs were scored;
- failed runs whose only mistake is choosing the wrong variant of an item
  when the user never stated the option that tells the right one apart. The
  right item came from the simulated user's hidden instructions.

That leaves 1,304 of the 1,980 runs, 554 of them failed. Judge changes to
Tailwise on this row: label noise in the full dataset caps what any
trace-based detector can reach. Each run's labels are in `runs.csv`
(`meta_trace_detectable`, `meta_gold_consistent_task`,
`meta_mixup_not_in_trace`).

**Diagnostics:**

- for each check: how often it fires on failed runs and on successful runs;
- the reasons Jev gave, for failed and for successful runs;
- cost per run, and per failure caught.

**Runs Tailwise cannot score:** Jev can reject a trace. Tailwise cuts one
that is too long (a giant SWE-agent patch, say) to fit before sending it, so
`max_tokens_exceeded` should no longer happen, but on any rejection it falls
back to a random decision. These runs count the way production treats them
here: dropped, and last in any review order. The tables report how many
there were.

All randomness comes from `seed`: the SWE-agent sample, baselines, tie
breaks and retry jitter.

## Cost

- **Pricing.** Jev charges $0.042 per million input tokens; output is free.
- **Real runs.** Cost is billed from the `input_tokens` Jev reports for each
  call (`tokens_source: api`).
- **Dry runs.** `--dry-run` estimates with tiktoken `cl100k_base`, because
  TypeSafe does not publish Jev's tokenizer. Jev has billed about 25% more
  than these estimates.
- **Budget cap.** `cost.budget_usd` (default $3) is a hard cap.
  - Before each call, its estimated cost is reserved, multiplied by the
    worst estimate-to-billed ratio seen so far.
  - A call that could go past the cap is never sent. The run stops cleanly
    and exits with status 3.
  - Calls that time out inside Tailwise may still be billed without being
    recorded, so leave some headroom.
- **Cache.** Every score is cached in `.cache/scores.sqlite`, keyed by
  dataset, trace, model, a hash of the settings that change a score (model,
  threshold, questions, evidence, and the Tailwise sources that build Jev's
  state: `internal/trace/*.go` and `internal/otlp/convert.go`) and a hash of
  the exact request sent.
  - Rerunning, or resuming after a stop, costs nothing for runs already
    scored.
  - A change to other Tailwise code does not invalidate the cache; delete
    it after such a change if it affects what Jev is sent.
- **Rate limits.**
  - `client.concurrency` and `client.requests_per_second` limit the load.
  - A 429 or 5xx is retried with exponential backoff and jitter.
  - A fallback decision is retried once.
  - A 4xx is final, and is written to `errors.json`.

## Outputs

Each run writes `results/<UTC timestamp>_<git sha>/`:

| File | Contents |
| --- | --- |
| `runs.csv` | One row per run: the label, `p_keep`, the decision, `answer_wrong` and the decision it alone would make, flags, severity, cause, every answer, tokens, cost, dataset metadata |
| `summary.json` | Every metric, per dataset and subset, plus tokens, cost, duration, Tailwise version, Jev model and the config used |
| `plots/*.png` | Budget curve, precision of kept runs, ROC, PR, reliability, checks on failed and successful runs, cost |
| `report.md` | A summary of the results, the main tables and the plots |
| `tailwise.yaml`, `tailwise.log` | The config Tailwise ran with and its log |
| `errors.json` | Runs that could not be scored, if any |

Every results folder also holds `source.patch`, the uncommitted changes the
run used. `summary.json` records the base commit and the patch's SHA-256
under `meta.source`; `git checkout <base> && git apply source.patch` rebuilds
the tree. To reproduce a run, use the commit in the folder's name, the `config` block
in `summary.json` (it pins τ-bench's commit and the SWE-agent sample), and
the same `seed`. Delete `.cache/scores.sqlite` to score from scratch.

## Data handling

- **Where the data lives.** Datasets download into `.cache/`, which is
  git-ignored along with `results/`. Nothing from them is committed.
- **TRAIL's terms.**
  - Use it for evaluation only.
  - Never use it to train or tune anything, including questions or
    thresholds.
  - Never share it outside a gated repository.
- **Test fixtures.** The fixtures in `tests/` are invented, not dataset rows.
