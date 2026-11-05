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
questions; `manifests/tau-actions.json` adds τ-bench's action tools, for the
action checks), `--evidence first` (Tailwise's older evidence strategy; `grounded`
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

`uv run bench publish <results folders>` writes `published/benchmark.json`,
the figures the website shows: one row per dataset, slice (all runs or the
trace-detectable ones) and setup (built-in questions or a manifest, with or
without rules), at the cost section's deployment of 10,000 traces, 10%
failing and 500 reviews. It reads only `summary.json`, so it is free. The
website reads the file at the latest release's tag, so publish it from the
runs a release's figures come from before tagging:

```sh
uv run bench publish results/<built-in> results/<tau-actions> \
  results/<built-in+rules> results/<tau-actions+rules>
```

`uv run pytest` runs the unit tests, and `uv run mypy src` type-checks the
code. Neither needs the network.

## Results

The headline is the share of reviewed runs that failed: a team reviews the
5% of runs Tailwise ranks highest, and asks how many of them are real
failures. That is what a review queue gets; ROC-AUC and the threshold
metrics below are diagnostics. The datasets fail far more often than
production agents, so the columns after the first drop failed runs at random
until 20%, 10% or 5% of runs fail (100 draws, mean). Each draw also
resamples the tasks, since the datasets run each task several times, so the
intervals cover which tasks a dataset happened to contain. Random review
finds failures at the failure rate; a perfect ranker finds 100% in each
column.

Jev `jev-1.14.0`. Runs are ranked by `severity`, the deciding questions'
average answer. The bold rows and the other `grounded` rows were scored
2026-11-05 on the current code (`results/20261105T121615Z_…` and
`20261105T121625Z_…`); the `first` and held-out rows on 2026-11-04. The
folders under `results/` hold every table.

| Dataset | Runs (failed) | Questions, evidence | Own rate | 20% failing | 10% failing | 5% failing | ROC-AUC | Cost per 1,000 runs |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| τ-bench | 1,980 (40%) | built-in, `first` | 51% | 27% | 13% | 7% | 0.586 | $0.08 |
| | | built-in, `grounded` | 66% | 40% | 22% | 12% | 0.645 | $0.14 |
| | | + action checks³ | **67%** | **42%** | **23%** | **12%** | **0.668** | $0.36 |
| τ-bench, trace-detectable runs | 1,304 (42%) | built-in, `first` | 61% | 33% | 19% | 10% | 0.629 | $0.08 |
| | | built-in, `grounded` | 74% | 49% | 30% | 18% | 0.708 | $0.13 |
| | | + action checks³ | **92%** | **74%** | **47%** | **27%** | **0.732** | $0.33 |
| | | + action checks, ranked by `p_keep`⁴ | 71% | 41% | 23% | 14% | 0.706 | $0.33 |
| SWE-agent | 1,000 (77%) | built-in, `first`¹ | 100% | 59%† | 36%† | 22%† | 0.832 | $0.12 |
| | | built-in, `grounded` | 100% | 78%† | 52%† | 32%† | 0.856 | $0.25 |
| SWE-agent, held out | 1,000 (84%) | built-in, `first` | 100% | 60%† | 36%† | 18%† | 0.786 | $0.14 |
| | | built-in, `grounded` | 98% | 67%† | 41%† | 21%† | 0.801 | $0.27 |
| TRAIL (GAIA and SWE-bench) | 148 (74%) | built-in, `first` | 100% | 55%† | 28%† | 12%† | 0.631 | $0.12 |
| | | built-in, `grounded` | 100% | 53%† | 31%† | 17%† | 0.673 | $0.15 |

† Rough: these datasets have few successful runs (227, 160 and 38), so at
these rates a 5% review is 3 to 15 runs per draw. On τ-bench it is 33 to 141.

The intervals are wide everywhere. At 10% failing (95% intervals):
τ-bench trace-detectable with action checks 47% (26–66%), the same runs with
`answer_wrong` alone 30% (17–49%), all τ-bench runs with action checks 23%
(14–34%), SWE-agent 52% (24–83%), and TRAIL 31% (0–100%).

³ With `manifests/tau-actions.json`: the built-in questions plus τ-bench's
13 action tools, which turn on the [action checks](../README.md#checking-an-agents-actions).
Of failed runs on gold-consistent tasks, they keep wrong-variant exchanges
the user had described 90% of the time (33% without), other wrong arguments
73% (58%), extra actions 68% (57%), and runs that left out an action 68%
(50%).

⁴ The same answers, ranked by the highest deciding answer, as Tailwise did
before `severity` became the average. Ranked that way, the action checks
made the review queue *worse* than `answer_wrong` alone (23% against 30% at
10% failing): one confident check, usually `action_wrong`, put a third
successful runs at the top. Averaging puts first the runs several checks
doubt.

**Against `answer_wrong` alone**, on the same resampled runs (points of
reviewed runs that failed, 95% interval):

| τ-bench, with action checks | 20% failing | 10% failing | 5% failing |
| --- | --- | --- | --- |
| Trace-detectable runs | +24 (+5 to +40) | +17 (−1 to +35) | +9 (−2 to +23) |
| All runs | 0 (−15 to +13) | 0 (−11 to +9) | +1 (−9 to +10) |

The gain is shown at 20% failing on trace-detectable runs and likely at
10%; at lower rates and on all runs it is not shown. The action checks and
the averaging were designed on τ-bench, so these are in-sample until
confirmed on data they were not built on.

With the built-in questions, `answer_wrong` is the only deciding question,
so `severity` is its answer. The `first` and held-out rows predate
`severity` and were rebuilt from each run's `runs.csv` (folders named
`rebuilt-from-…`), not re-scored.

### Cost: random review against Tailwise

Per 10,000 traces with 10% failing, a team reviewing 500 of them (5%).
Random review costs nothing to choose the traces and finds 50 failures;
Tailwise pays Jev to score every trace ($0.042 per million input tokens,
output free). Built-in questions with `grounded` evidence, and the action
checks for τ-bench:

| Dataset | Jev cost | Failures found: random → Tailwise | Reviews per failure found | Random reviews for the same failures | Jev cost per extra failure found |
| --- | --- | --- | --- | --- | --- |
| τ-bench, trace-detectable, action checks | $3.32 | 50 → 237 | 10 → 2.1 | 2,366 | $0.018 |
| τ-bench, all runs, action checks | $3.58 | 50 → 115 | 10 → 4.3 | 1,150 | $0.055 |
| τ-bench, trace-detectable, built-in | $1.31 | 50 → 152 | 10 → 3.3 | 1,522 | $0.013 |
| SWE-agent† | $2.50 | 50 → 258 | 10 → 1.9 | 2,579 | $0.012 |
| SWE-agent† with the `no_submit` keep rule | $1.43 | 50 → 280 | 10 → 1.8 | 2,796 | $0.006 |
| TRAIL† | $1.54 | 50 → 156 | 10 → 3.2 | 1,558 | $0.015 |

Scoring costs cents per 10,000 traces; reviewer time is the real cost, and
Tailwise cuts the reviews needed per failure found by 2.3× to 5×. Every
report has this table (`## Cost`), from that run's own token counts. These
traces are 1,300 to 5,300 input tokens each; longer traces cost
proportionally more.

### Heuristic rules

`bench run --rules rules` adds the [heuristic rules](../README.md#heuristic-rules)
in `rules/<dataset>.yaml`, written from what each agent does before looking
at any result, and never tuned on the labels. Rules do not change what Jev is
asked, so `rulecheck` (Tailwise's own rule and span code) checks each run's
request and the harness combines the verdicts with the cached answers as the
engine does: a rule set costs nothing to measure, and is compared with Jev
alone on the very same answers. Scored 2026-11-05
(`results/20261105T121633Z_…`, `20261105T121649Z_…`), 10% failing:

| Dataset | Jev alone | Jev + rules | Gain (95% interval) | Rules alone |
| --- | --- | --- | --- | --- |
| τ-bench, with action checks | 23% | 19% | −4 (−11 to +4) | 12% |
| τ-bench trace-detectable, with action checks | 47% | 42% | −5 (−21 to +15) | 14% |
| SWE-agent | 52%† | 56%† | +4 (−35 to +43) | 55%† |
| TRAIL | 31%† | 24%† | −7 (−67 to +50) | 18%† |

| Dataset | Rule | Fired on failed | Fired on successful | Fired runs that failed |
| --- | --- | --- | --- | --- |
| SWE-agent | `no_submit` (keep) | 40% | 4% | 97% |
| τ-bench | `tool_failed` (signal) | 34% | 30% | 43% |
| τ-bench | `looping` (signal) | 2% | 0% | 75% |
| TRAIL | `code_failed` (signal) | 18% | 32% | 62% |
| TRAIL | `looping` (signal) | 11% | 5% | 86% |
| TRAIL | `no_answer` (keep) | 5% | 0% | 100% |

- **A tool error is not a failure signal.** Agents recover from them:
  `tool_failed` fires about as often on successful τ-bench runs as on failed
  ones, and TRAIL's `code_failed` more often on successful ones. As signal
  rules weighing as much as a check, they pulled good runs up the queue.
- **"Certain" needs checking.** SWE-agent's `no_submit` is right 97% of the
  time, not always: 4% of successful runs never call `submit` (the harness
  can submit for the agent when it runs out of budget). It decides 32% of
  SWE-agent runs without Jev, saving a fifth of the three-dataset run's tokens.
- None of the gains is shown. Rules earn their place by saving cost on
  certain failures, or as signals only once measured on the agent's own
  labelled runs.

### At the threshold

The keep decision itself, at 0.5: the share of successful runs kept, and of
failures caught. A team that reviews a fixed share by `severity` does not
depend on these.

| Dataset | Questions, evidence | Successes kept | Failures caught |
| --- | --- | --- | --- |
| τ-bench | built-in, `first` | 57% | 69% |
| | built-in, `grounded` | 33% | 52% |
| | + action checks³ | 50% | 72% |
| τ-bench, trace-detectable runs | built-in, `first` | 50% | 68% |
| | built-in, `grounded` | 25% | 54% |
| | + action checks³ | 40% | 70% |
| SWE-agent | built-in, `first`¹ | 30% | 82% |
| | built-in, `grounded` | 33% | 90% |
| SWE-agent, held out | built-in, `first` | 42% | 82% |
| | built-in, `grounded` | 46% | 86% |
| TRAIL | built-in, `first` | 68% | 86% |
| | built-in, `grounded` | 68% | 84% |

¹ Scored before the trimming and tool-status fixes, with
`manifests/answer-wrong-only.json` (the same deciding question): 3 runs were
unscorable.

TRAIL's runs are smolagents agents. Before Tailwise read their code steps
(each "Step" span whose model wrote code, as an `execute_code` call) and
scored runs that ended without an answer, TRAIL's SWE-bench runs ranked
below random (ROC-AUC 0.33): Tailwise saw only the task and the final
answer, and dropped the 5 runs that crashed, all of them failures. With
both, it is 0.67 (95% interval for the gain: +0.05 to +0.62), and no run is
unscorable.

**Reading them:**

- **Tailwise beats random review clearly.** With the action checks, a team
  reviewing 5% of τ-bench runs finds a real failure in about half its reviews
  when 10% of runs fail (47%, 95% interval 26–66%, against 10% at random),
  and in a quarter when 5% fail.
- **How checks are combined matters as much as which checks run.** Ranked by
  the highest answer, the action checks lowered the review queue's yield
  (30% to 23%); averaged, they raise it to 47%. That gain over `answer_wrong`
  alone is likely but not yet shown at 10% failing (interval −1 to +35
  points), and it is in-sample.
- **On all τ-bench runs there is no gain** (23% either way): a third of its
  labels rest on things no trace shows, which caps any trace-based ranking.
- `grounded` evidence improves the queue on every dataset. It costs about
  twice as much.
- TRAIL's label is "has a HIGH-impact error", not "failed the task", so it
  is noisier than the other two. Its figures are a final check, never a
  target: its terms forbid tuning on it.

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
  shipped. A run is kept when any check reaches the threshold (0.5), and it
  is ranked by `severity`, the deciding questions' average answer.
- **answer_wrong only:** the decision question alone, read from the recorded
  answers.

A third row ranks Tailwise's answers by `p_keep`, the highest deciding
answer, to show what averaging adds; it is left out where it ranks the same.

**The headline: reviewed runs that failed.** A team reviews the
`review_budget` share of runs (5%) that Tailwise ranks highest. The report
gives the share of those that failed at the dataset's own failure rate, and
at each production rate in `reference_failure_rates` (20, 10, 5 and 2%). For
a production rate, failed runs (or, above the dataset's rate, successful
ones) are dropped at random until that share fails, 100 times; the report
gives the mean, its 95% interval in `summary.json`, and marks a cell † when
fewer than 20 runs are reviewed per draw. Ties in score are broken at random.
Random review finds failures at the failure rate; a perfect ranker finds
them in every reviewed run, or failure rate ÷ budget when failures are fewer
than the budget.

`severity` is computed from the recorded answers, as Tailwise computes it,
so changing how answers combine needs no re-scoring. For each run scored
fresh, the harness checks it against the `severity` Tailwise returned.

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
- **Calibration:** whether `p_keep` (not `severity`, an average) behaves
  like a probability of failure,
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

`results/` is not committed. `published/benchmark.json` is: the figures of the
latest published runs, with the `run` folder each row came from and the
`source` commit.

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
