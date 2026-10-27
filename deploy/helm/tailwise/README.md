# Tailwise Helm chart

Installs the inference engine on Kubernetes: the engine Deployment and its
Service (JSON API on 8080, OTLP on 4317/4318), its config and model
manifests, and optionally a NATS JetStream cluster and a trace-routing
gateway. Traces are scored by TypeSafe's hosted Jev by default; any other
System One model speaking `/v1/systemone` can be set in `config.scorer`.

## Install

```sh
kubectl create namespace tailwise
kubectl -n tailwise create secret generic tailwise-secrets --from-literal=jev-api-key="$JEV_API_KEY"
helm dependency update deploy/helm/tailwise       # fetches the optional NATS chart once
helm install tailwise deploy/helm/tailwise --namespace tailwise
```

That is all it needs: your Jev API key, in the Secret `jev.apiKeySecret`
names. Without a manifest each model is asked the built-in questions, and
the engine logs a warning; give your own in `manifests.files`. Then point your agents' OTLP exporters at
`tailwise.tailwise.svc:4317` (gRPC) or `:4318` (HTTP). `helm install` prints
the exact address.

The image is `ghcr.io/tecnosam/tailwise:<appVersion>`. To use your own
build, push `Dockerfile` to your registry and set
`image.repository` and `image.tag`.

## Choosing where the queues live

| Setup | Values | Survives a pod restart | Notes |
| --- | --- | --- | --- |
| Embedded NATS (default) | nothing | no | Each pod runs its own NATS server inside the engine; nothing else to run. With several replicas, keep a trace's spans on one pod (below) |
| Bundled NATS | `nats.enabled=true` | yes | A 3-server JetStream cluster in the release, each server with its own volume |
| Your NATS | `queue.url=nats://...` | yes | Auth, TLS and `replicas` under `config.queue` |

**Bundled NATS.** The official NATS chart, run as a StatefulSet of three
servers that clients reach through one Service. Each server has its own
persistent volume (`nats.config.jetstream.fileStore.pvc.size`, 10Gi); there
is no shared volume. Every stream the engine creates is replicated to all
three (`queue.replicas` follows `nats.config.cluster.replicas`), so losing a
pod or a node loses nothing. The servers are spread across nodes, and a
NetworkPolicy lets only the engine's pods connect. Any value of the
[NATS chart](https://github.com/nats-io/k8s/tree/main/helm/charts/nats) can be
set under `nats:`, e.g. a storage class, node selector or tolerations.

**Your NATS.** For a NATS you already run, or a hosted one:

```yaml
queue:
  url: tls://nats.messaging:4222
config:
  queue:
    replicas: 3
    auth: {credentials_file: /etc/nats/tailwise.creds}
extraVolumes:
  - {name: nats-creds, secret: {secretName: tailwise-nats-creds}}
extraVolumeMounts:
  - {name: nats-creds, mountPath: /etc/nats, readOnly: true}
```

## Several replicas with queues on embedded NATS

An OTLP trace arrives in several batches, and the pod that assembles it must
receive all of them. With shared NATS any pod can; with embedded NATS, spans must stick to
one pod. The chart offers two ways:

- **Client-IP affinity** (the default with embedded NATS). The Service sends
  everything from one agent pod to the same engine pod. This works when a
  whole trace is produced by one process, as for most agents. It does not
  for a trace spanning several services, and many agents behind one NAT
  address all land on one pod.
- **Trace-routing gateway** (`traceRouting.gateway.enabled=true`). An
  OpenTelemetry Collector with the `loadbalancing` exporter splits every
  batch by trace ID and sends each trace to one engine pod by consistent
  hashing over the pods behind a headless Service. Point agents at the
  `-gateway` Service. Works for any trace, at the cost of one more hop.

Either way, queued work in a pod is lost if that pod dies; use NATS where
that matters.

## Autoscaling

The engine spends most of its time waiting on its model, so CPU is a poor
signal. Scale on the backlog instead: the traces waiting to be scored.
With the queues in NATS, `autoscaling.queueDepth.enabled=true` creates a
[KEDA](https://keda.sh) ScaledObject whose `nats-jetstream` trigger reads
the workers' consumer lag straight from NATS, with no metrics pipeline in
between:

```yaml
nats: {enabled: true}
autoscaling:
  queueDepth:
    enabled: true
    minReplicas: 1
    maxReplicas: 20
    lagThreshold: 50            # waiting traces per pod
```

It needs KEDA installed in the cluster (`kedaNamespace` lets it through
the NATS NetworkPolicy), and replaces the CPU autoscaler
(`autoscaling.enabled`). For your own NATS, set `natsMonitoringEndpoint` to
its monitoring address (port 8222; in a cluster, its headless Service). The
same backlog is exported as the `tailwise.queue.depth` metric, for
dashboards and alerts. With embedded NATS each pod's queue is its own, so there is
no shared backlog to scale on.

## Secrets

The engine's config names environment variables, never secret values
(`scorer.api_key_env`, `exporters[].headers_env`, `queue.auth.token_env`,
`observability.metrics.headers_env`). Feed them from your Secrets:

```yaml
config:
  exporters:
    - {name: langfuse, endpoint: https://cloud.langfuse.com/api/public/otel/v1/traces, headers_env: LANGFUSE_OTLP_HEADERS}
env:                                 # JEV_API_KEY comes from jev.apiKeySecret
  - name: LANGFUSE_OTLP_HEADERS
    valueFrom: {secretKeyRef: {name: tailwise-secrets, key: langfuse-headers}}
```

## One model per agent

List the agent under `config.agents`, and put its manifest, if it has one,
under `manifests.files` (or in your own ConfigMap, `manifests.existingConfigMap`):

```yaml
config:
  agents:
    models:
      support-bot:                  # complete on its own: nothing comes from scorer
        endpoint: https://api.typesafe.ai/v1/systemone
        model: jev-latest
        manifest: /etc/tailwise/manifests/support-bot.json
manifests:
  files:
    support-bot.json: |
      ...
```

## Values

| Value | Default | Meaning |
| --- | --- | --- |
| `jev.apiKeySecret` | `{name: tailwise-secrets, key: jev-api-key}` | The Secret holding your Jev API key, passed as `JEV_API_KEY`; `name: ""` to pass it another way |
| `config` | no exporters; Jev as the scorer | The engine's config; see `tailwise.schema.yaml` for every key. The chart sets the listen addresses, and `queue.url` with bundled NATS |
| `manifests.files` / `manifests.existingConfigMap` | none | Optional model manifests, mounted in `/etc/tailwise/manifests` |
| `env`, `envFrom` | `[]` | Environment for the engine, e.g. secrets the config names |
| `extraVolumes`, `extraVolumeMounts` | `[]` | Files the config points at: NATS credentials, certificates |
| `queue.url` | `""` | An external NATS; ignored with `nats.enabled` |
| `nats.enabled` | `false` | Run a NATS JetStream cluster with the release |
| `traceRouting.gateway.enabled` | `false` | Run the trace-routing collector in front of the engine |
| `replicaCount` | `2` | Engine pods, unless `autoscaling.enabled` |
| `autoscaling.*` | off, 2 to 10 at 70% CPU | HorizontalPodAutoscaler on CPU |
| `autoscaling.queueDepth.*` | off, 1 to 10, 50 traces per pod | KEDA scaling on the trace backlog; needs NATS and KEDA |
| `service.sessionAffinity` | `ClientIP` with embedded NATS and no gateway, else `None` | Override the Service's affinity |
| `probes.readinessPath` | `/healthz` | `/readyz` also takes a pod out of service while the queue or the default model is down |
| `podDisruptionBudget.*` | on, `minAvailable: 1` | Keeps one pod up through node drains |
| `serviceAccount.annotations` | `{}` | e.g. an IAM role (IRSA) or a GKE workload identity |
| `networkPolicy.enabled` | `true` | Restrict the bundled NATS to the engine |
| `resources`, `nodeSelector`, `tolerations`, `affinity`, `topologySpreadConstraints` | requests 250m/256Mi, spread over nodes | Scheduling |
| `podSecurityContext`, `securityContext` | non-root, read-only root filesystem, no capabilities | Pod security; the error file sink needs a writable volume |

## Kustomize

Kustomize can install the chart and apply your overlays on top:

```yaml
# kustomization.yaml
helmCharts:
  - name: tailwise
    repo: oci://ghcr.io/tecnosam/charts   # or a local copy: helmGlobals.chartHome
    version: 0.1.0
    releaseName: tailwise
    namespace: tailwise
    valuesFile: tailwise-values.yaml
patches:
  - path: my-patch.yaml
```

Build with `kustomize build --enable-helm`. Or render once and keep the
output as a plain base: `helm template tailwise deploy/helm/tailwise -f
tailwise-values.yaml > base/tailwise.yaml`.

## Checking a config before a rollout

The engine validates a config and its manifests without starting:

```sh
docker run --rm -v $PWD:/cfg ghcr.io/tecnosam/tailwise:0.1.0 -check -config /cfg/tailwise.yaml
```
