# Engine config examples

The engine needs no config file: without `-config` it runs on its built-in
defaults, scoring every trace with TypeSafe's hosted Jev, and needs only
`JEV_API_KEY`. These are ready-to-run variations for when you want to change
something. Each sets only what differs from the built-in defaults; every key
it leaves out keeps its default.
[`../tailwise.schema.yaml`](../tailwise.schema.yaml) documents every key and
its default, and a test (`internal/config/examples_test.go`) checks that each
example here loads.

| File | Use it to |
|---|---|
| [`jev.yaml`](jev.yaml) | Tune Jev for production: your own questions, workers, timeouts |
| [`json-api-only.yaml`](json-api-only.yaml) | Take traces over the JSON API only, with the OTLP receivers off |
| [`review-more.yaml`](review-more.yaml) | Keep more traces for review: a lower threshold and a larger random sample |
| [`remote-services.yaml`](remote-services.yaml) | Run against a NATS cluster and a hosted Phoenix on other machines |
| [`langfuse.yaml`](langfuse.yaml) | Send kept traces to Langfuse as well as Phoenix |
| [`agents.yaml`](agents.yaml) | Give some agents their own questions, with a default for the rest |

## Running one

From the repository root, with Phoenix up (`make phoenix`). The queues
stay on an embedded NATS server unless the config or `TAILWISE_NATS_URL` sets `queue.url`
(then start NATS with `make nats`):

```sh
export JEV_API_KEY=...
make run CONFIG=examples/review-more.yaml
```

A relative path inside a config (such as `scorer.manifest`) resolves against
the config file's own directory, so the examples work from wherever you start
the engine.

## Jev and its key

The engine sends the key as a bearer token with every request. It reads it
from `JEV_API_KEY`, or the environment variable `scorer.api_key_env` names,
and refuses to start if that variable is empty. The key itself never goes in
a file. `GET /readyz` reports the engine unready if Jev refuses the key.

## Manifests

A manifest holds the questions the model is asked. It is optional. Without
one, the engine asks the built-in questions and logs a warning at start-up.

[`manifests/my-agent.json`](manifests/my-agent.json) is a starting point: the
built-in questions, ready to reword for your agent. Point `scorer.manifest`
at the file, as [`jev.yaml`](jev.yaml) does.

Two things to know before pointing Jev at real traffic:

- **Every trace leaves your network.** It is sent to `api.typesafe.ai`
  and billed to the key. Only use it for traces you are allowed to send to a
  third party.
- **Rate limits.** Jev answers 429 when busy. A queued trace goes back to the
  queue and is redelivered later with `queue.redelivery_backoff`. If that
  happens often, lower `server.workers`.

## Sending traces to Langfuse, or any OTLP back end

`exporters` is a list: every kept trace goes to each back end in it, each
with its own queue, so a slow one never holds up the others.
[`langfuse.yaml`](langfuse.yaml) keeps the local Phoenix and adds Langfuse,
which takes Basic auth built from your public and secret keys. Keep the
header in an environment variable and name it in `headers_env`:

```sh
export LANGFUSE_OTLP_HEADERS="Authorization=Basic%20$(printf '%s' "pk-lf-...:sk-lf-..." | base64)"
make run CONFIG=examples/langfuse.yaml
```

The variable holds headers in the OpenTelemetry format, `key=value` pairs
separated by commas, with `%20` for a space. The engine refuses to start if
it is unset. Any other back end that takes OTLP/HTTP traces (Honeycomb, Grafana
Tempo, an OpenTelemetry Collector) is one more entry: a `name`, its traces
`endpoint` with the path, and its auth header. The engine's README lists the
attributes each back end receives.

## Docker Compose

The image has no config baked in, and the compose file runs it on the
built-in defaults with `JEV_API_KEY` from your shell. To run an example in
Docker, mount it and pass `-config`, in `docker-compose.yml`:

```yaml
  observer:
    command: ["-config", "/etc/tailwise/config.yaml"]
    volumes:
      - ./examples/jev.yaml:/etc/tailwise/config.yaml:ro
      - ./examples/manifests:/etc/tailwise/manifests:ro
```

A relative `manifest` in the mounted config then resolves against
`/etc/tailwise`, so `manifests/my-agent.json` is found.
