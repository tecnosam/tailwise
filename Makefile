BIN    := bin/tailwise
# CONFIG names a config file; empty runs on the built-in defaults.
CONFIG ?=
PYTHON ?= python3

NATS_BOX := docker run --rm --network tailwise_default natsio/nats-box:latest nats -s nats://nats:4222

# The version, from the latest v* tag: 0.1.0 on a tag, 0.1.0-3-gabc1234 after
# it, with -dirty for uncommitted changes, and 0.0.0-dev before the first tag.
VERSION  ?= $(shell git describe --tags --match 'v[0-9]*' --dirty 2>/dev/null | sed 's/^v//' | grep . || echo 0.0.0-dev)
LDFLAGS  := -X main.version=$(VERSION)
IMAGE    ?= ghcr.io/tecnosam/tailwise
GIT_CLIFF ?= npx --yes git-cliff@2.14.2
BUMP     ?= auto

.PHONY: chart-lint build test run lint fmt golden clean nats phoenix up down logs dlq samples need-key \
	version changelog release image

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/tailwise

# Print the version this checkout builds as.
version:
	@echo $(VERSION)

# Preview the next release: its version and CHANGELOG.md section, from the
# Conventional Commits since the last tag. Changes nothing.
changelog:
	@$(GIT_CLIFF) --bumped-version --bump $(BUMP)
	@$(GIT_CLIFF) --unreleased --bump $(BUMP) --strip all

# Cut a release: bump the version (BUMP=auto|patch|minor|major), write its
# CHANGELOG.md section, sync the Helm chart, commit and tag. Push yourself:
# git push --follow-tags.
release:
	GIT_CLIFF="$(GIT_CLIFF)" scripts/release.sh $(BUMP)

# Build the Docker image, tagged with the version.
image:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

# Offline: tests use httptest servers, in-memory span recorders and an
# in-process NATS server on a loopback port.
test:
	go vet ./...
	go test -race ./...

# Runs on the built-in defaults (Jev, embedded NATS), or CONFIG=path. Needs
# JEV_API_KEY, and Phoenix on localhost:6006 (`make phoenix`) to see the kept
# traces. For an external NATS: `make nats` and
# TAILWISE_NATS_URL=nats://localhost:4222 make run.
run: build need-key
	$(BIN) $(if $(CONFIG),-config $(CONFIG))

need-key:
	@test -n "$$JEV_API_KEY" || { echo "JEV_API_KEY is not set: export your Jev API key first"; exit 1; }

# Start only NATS JetStream, for `TAILWISE_NATS_URL=nats://localhost:4222 make run`.
nats:
	docker compose up -d nats

# Start only Phoenix, for running the engine on the host with `make run`.
phoenix:
	docker compose up -d phoenix

# Build and start the engine, NATS and Phoenix in Docker.
up: need-key
	TAILWISE_VERSION=$(VERSION) docker compose up --build -d

# Send the bundled sample traces and print each decision next to its label.
samples:
	$(PYTHON) scripts/send_samples.py --sync

# Stop all three. Queued traces and dead letters stay in the nats-data
# volume, Phoenix's traces in phoenix-data.
down:
	docker compose down

logs:
	docker compose logs -f observer

# Show the dead-letter stream. Needs the compose stack to be up.
dlq:
	$(NATS_BOX) stream info TAILWISE_TRACES_DLQ

lint:
	@unformatted="$$(gofmt -l .)"; if [ -n "$$unformatted" ]; then \
		echo "gofmt needed on:"; echo "$$unformatted"; exit 1; fi
	go vet ./...

fmt:
	gofmt -w .

# Rewrite testdata/golden/*.txt from BuildState after an intended change to
# what the model reads; review the diff before committing.
golden:
	go test ./internal/trace -run TestBuildStateGolden -update -count=1

clean:
	rm -rf bin

# Lint the Helm chart and render its main setups (needs helm).
CHART := deploy/helm/tailwise
CHART_MANIFEST := --set-file 'manifests.files.manifest\.json=testdata/manifest.json'
chart-lint:
	helm dependency update $(CHART)
	helm lint $(CHART)
	helm template tailwise $(CHART) > /dev/null
	helm template tailwise $(CHART) $(CHART_MANIFEST) > /dev/null
	helm template tailwise $(CHART) $(CHART_MANIFEST) --set nats.enabled=true > /dev/null
	helm template tailwise $(CHART) $(CHART_MANIFEST) --set traceRouting.gateway.enabled=true > /dev/null
	helm template tailwise $(CHART) $(CHART_MANIFEST) --set nats.enabled=true --set autoscaling.queueDepth.enabled=true > /dev/null
