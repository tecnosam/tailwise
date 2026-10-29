# Contributing to Tailwise

Thanks for helping. This guide covers working on the engine itself; to run
it, see the [README](README.md).

## Getting set up

You need:

- Go 1.26 or newer
- Docker, for `make up` and the other Compose targets
- Python 3, only for `make samples`
- Node, for `npx git-cliff` when previewing or cutting a release
- helm, only for `make chart-lint`

```sh
make build    # bin/tailwise
make test     # go vet + go test -race, fully offline
```

`make run` and `make up` need a Jev API key in `JEV_API_KEY`; the tests do not.

For how the pieces fit together, start with
[architecture/architecture.md](architecture/architecture.md); the same folder
has class, sequence and dependency diagrams.

## Layout

```
cmd/tailwise/      flags, config load, wiring, graceful shutdown
internal/          every package, none importable from another module:
  pipeline/        the core: domain types, interfaces, model routing, keep/drop policy, the Engine
  trace/           trace types (with the agent) and BuildState
  fault/           the error type: kind, component, operation, trace
  observe/         the error pipeline and its sinks (stdout, file, otlp)
  metrics/         the engine's OpenTelemetry metrics
  retry/           the one retry policy
  config/          the YAML file (File), the engine's settings (Engine), validation
  scorer/          System One model client (Jev, ...) and the per-agent model builder
  broker/          the JetStream connection: an external NATS, or one embedded
  queue/           the trace queue and span store: the contract and settings
    natsq/         on JetStream
  export/          exporters: the dispatcher, the back-end and queue contracts
    otlpbackend/   the OTLP/HTTP back end
    natsq/         export queues on JetStream
  server/          HTTP handlers and the OpenAPI spec
  otlp/            OTLP gRPC and HTTP receivers, span-to-trace mapping
  jsq/             the generic JetStream work queue both NATS queues are built on
  common/          shared helpers: NATS connection and pull loop, worker pool, request metrics
```

## Tests

`make test` needs no network and no Docker. The queue tests run a real NATS
server in-process on a loopback port (`internal/queue/queuetest`), so
redelivery, dead-lettering and stream limits are tested against JetStream
itself, not a fake.

## The slice and its golden files

`trace.BuildState` renders a trace into the text the model reads. Tool
arguments, and any status that is not a string, are written as compact JSON
(`search({"query":"capital of France","limit":3}) -> ok`), and evidence is
cut to 1500 Unicode code points rather than bytes.

- `testdata/traces/*.json`: input fixtures
- `testdata/golden/*.txt`: the slice for each, as `BuildState` writes it

`TestBuildStateGolden` fails if `BuildState` stops producing exactly these
files, so what the model reads never changes by accident. A second test
sends every fixture through the queue and checks the worker builds the same
slice.

**After an intended change** to the slice, or a new fixture, rewrite the
golden files and review their diff before committing:

```sh
make golden
make test
```

## Before opening a pull request

- `make lint` and `make test` pass. CI ([.github/workflows/ci.yml](.github/workflows/ci.yml))
  runs both, `make build` and `make chart-lint` on every pull request.
- Changes to the slice come with regenerated golden files (`make golden`)
  whose diff you have reviewed.
- Commit messages follow Conventional Commits (below): the changelog and the
  next version are computed from them.
- User-facing behaviour changes are reflected in the README, and changes to
  configuration in `tailwise.schema.yaml` and the examples.

## Issues

Planned work is written as Markdown in [issues/](issues/), one file per issue,
and reviewed like code. On every push to main that changes the folder,
[.github/workflows/issues.yml](.github/workflows/issues.yml) runs
[scripts/sync-issues.sh](scripts/sync-issues.sh), which keeps the GitHub
issues in step:

- A new file creates an issue, and an edited file updates its issue's title
  and body. Edit the file, not the issue: the next sync overwrites the issue.
- Deleting a file closes its issue with a comment.
- The first `# ` line is the title, an optional `**Labels:** a, b` line sets
  labels (created if missing; labels are only ever added), and the rest is
  the body.
- A hidden marker at the end of the body names the file, so renaming a file
  creates a new issue and closes the old one.

`DRY_RUN=1 scripts/sync-issues.sh` prints what a sync would do (it needs `gh`
signed in to the repository).

## Commits, versioning and releases

Tailwise follows [Semantic Versioning](https://semver.org), starting at
0.1.0, and records every release in [CHANGELOG.md](CHANGELOG.md). Releases are
git tags (`v0.1.0`), and the changelog is built from commit messages, so
commits follow [Conventional Commits](https://www.conventionalcommits.org):

| Commit | Example | Bumps |
| --- | --- | --- |
| `fix:` | `fix(scorer): retry a 529 from Jev` | patch: 0.1.0 to 0.1.1 |
| `feat:` | `feat(api): add a cause field` | minor: 0.1.1 to 0.2.0 |
| `feat!:`, or `BREAKING CHANGE:` in the body | `feat!: rename the reason field` | minor before 1.0.0 (0.2.0 to 0.3.0), major after |
| `docs:`, `refactor:`, `perf:` | `docs: explain manifests` | listed in the changelog, patch |
| `chore:`, `test:`, `ci:`, `build:`, `style:` | `test: cover the embedded broker` | not listed |

[git-cliff](https://git-cliff.org), configured in [cliff.toml](cliff.toml),
computes the next version and writes its changelog section. The Makefile
runs it through `npx`, so there is nothing to install beyond Node; set
`GIT_CLIFF=git-cliff` to use a local one.

```sh
make version                 # what this checkout builds as, e.g. 0.1.0-3-gabc1234
make changelog               # preview the next version and its changelog section
make release                 # bump from the commits, write CHANGELOG.md, sync the Helm chart, commit and tag
make release BUMP=minor      # or force patch, minor or major
git push --follow-tags       # publish the release commit and its tag
make image                   # build the Docker image tagged with the version
```

`make release` refuses to run with uncommitted changes, and when nothing new
is releasable. The binary reports its version with `tailwise -version` and in
its start-up log line; `make build`, `make image` and `make up` stamp it from
git, and a plain `go build` reports `dev`.

Pushing the tag publishes the release.
[.github/workflows/release.yml](.github/workflows/release.yml) runs the tests
again, then [GoReleaser](https://goreleaser.com) ([.goreleaser.yaml](.goreleaser.yaml))
builds `tailwise` for Linux, macOS and Windows on amd64 and arm64, stamped
with the tag's version, and creates a GitHub release with the archives,
`checksums.txt`, and the tag's CHANGELOG.md section as its notes
(`scripts/release-notes.sh 0.1.0` prints them). The release fails if
CHANGELOG.md has no section for the tag. A tag with a pre-release suffix,
such as `v0.2.0-rc.1`, is published as a pre-release.

## Make targets

| Target | Does |
| --- | --- |
| `make build` | Build `bin/tailwise`, stamped with the version |
| `make version` | Print the version this checkout builds as |
| `make changelog` | Preview the next version and its changelog section |
| `make release` | Cut a release: version, CHANGELOG.md, Helm chart, commit and tag (`BUMP=auto\|patch\|minor\|major`) |
| `make image` | Build the Docker image tagged with the version |
| `make test` | `go vet ./...` and `go test -race ./...` |
| `make run` | Build and run on the host on the built-in defaults (`CONFIG=path` for a config file); needs `JEV_API_KEY` |
| `make phoenix` | Start only Phoenix in Docker |
| `make nats` | Start only NATS in Docker, for `TAILWISE_NATS_URL=nats://localhost:4222 make run` |
| `make up` / `make down` | Start or stop the engine, NATS and Phoenix in Docker; `up` needs `JEV_API_KEY` |
| `make samples` | Send the bundled sample traces and print each decision |
| `make logs` | Follow the engine container's log |
| `make dlq` | Show the dead-letter stream's state |
| `make lint` | gofmt check and `go vet` |
| `make fmt` | Format every Go file with gofmt |
| `make chart-lint` | Lint the Helm chart and render its main setups (needs helm) |
| `make golden` | Rewrite the golden slice files after an intended change to the slice |
| `make clean` | Remove `bin/` |
