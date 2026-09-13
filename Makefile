# AgentGate — build, test and deploy.
#
# `make` with no target prints the self-documenting help below. Every target
# carrying a `##` comment appears there; targets without one are internal.

SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

# ---------------------------------------------------------------------------
# Version stamping
# ---------------------------------------------------------------------------
# Every value falls back safely. A container build from a tarball, a shallow
# CI clone, or a machine without git all still produce a working binary — they
# just produce one that says `dev`. A build that fails because `git describe`
# returned nothing is a build that fails at the worst possible moment.
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo 1970-01-01T00:00:00Z)

MODULE     := github.com/agentgate/agentgate
VERSION_PKG := $(MODULE)/internal/version

# -s -w strips the symbol table and DWARF: smaller image, and nothing useful is
# lost because panics still carry file and line.
LDFLAGS := -s -w \
	-X $(VERSION_PKG).Version=$(VERSION) \
	-X $(VERSION_PKG).Commit=$(COMMIT) \
	-X $(VERSION_PKG).BuildDate=$(BUILD_DATE)

GO         ?= go
GOFLAGS    ?= -trimpath
BIN_DIR    := bin
SERVICES   := gateway controlplane fleetview guardrails mockprovider agentctl loadgen
IMAGES     := gateway controlplane fleetview guardrails mockprovider

REGISTRY   ?= ghcr.io/agentgate
PLATFORMS  ?= linux/amd64,linux/arm64

COMPOSE    ?= docker compose
DOCKER     ?= docker

GATEWAY_URL ?= http://localhost:8080
CP_URL      ?= http://localhost:8081
FLEET_URL   ?= http://localhost:8082

# Kustomize must read the collector configs from deploy/otel, which is outside
# the kustomization root. See deploy/k8s/base/kustomization.yaml for why one
# copy beats two.
KUSTOMIZE       ?= kustomize
KUSTOMIZE_FLAGS := --load-restrictor=LoadRestrictionsNone
KUBECTL         ?= kubectl

ENV ?= dev

.PHONY: help
help: ## Show this help
	@printf '\nAgentGate — %s (%s)\n\n' '$(VERSION)' '$(COMMIT)'
	@grep -hE '^[a-zA-Z0-9_/-]+:.*?## ' $(MAKEFILE_LIST) \
		| sort \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'
	@printf '\nCommon variables: ENV=%s SVC=<service> REGISTRY=%s VERSION=%s\n\n' '$(ENV)' '$(REGISTRY)' '$(VERSION)'

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------

.PHONY: build
build: ## Build one binary: make build SVC=gateway
	@test -n "$(SVC)" || { echo "SVC is required, e.g. make build SVC=gateway"; exit 2; }
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(SVC) ./cmd/$(SVC)
	@echo "built $(BIN_DIR)/$(SVC) $(VERSION)"

.PHONY: build-all
build-all: ## Build every binary into bin/
	@mkdir -p $(BIN_DIR)
	@for svc in $(SERVICES); do \
		echo "building $$svc"; \
		CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$$svc ./cmd/$$svc || exit 1; \
	done
	@echo "built $(words $(SERVICES)) binaries at $(VERSION)"

# ---------------------------------------------------------------------------
# Test and static analysis
# ---------------------------------------------------------------------------

.PHONY: test
test: ## Run unit tests with race detection and coverage
	# -race is not optional here: the gateway's hot path is concurrent by
	# construction (rate-limit buckets, breaker state, streaming windows) and a
	# data race in any of them is a correctness bug in billing.
	$(GO) test -race -covermode=atomic -coverprofile=coverage.out -timeout=5m ./...
	@$(GO) tool cover -func=coverage.out | tail -n 1

.PHONY: test-integration
test-integration: ## Run integration tests against the local stack
	# Requires the stack to be up; the suite talks to real Redis, Postgres and
	# the mock providers rather than to fakes, because the interesting failures
	# (Lua atomicity, breaker transitions, SSE framing) do not reproduce
	# against a fake.
	$(GO) test -race -tags=integration -timeout=15m ./test/integration/...

.PHONY: lint
lint: ## Run golangci-lint
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint not found: https://golangci-lint.run/welcome/install/"; exit 127; }
	golangci-lint run --timeout=5m ./...

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: fmt
fmt: ## Format Go, Terraform and YAML
	$(GO) fmt ./...
	@command -v gofumpt >/dev/null 2>&1 && gofumpt -l -w . || echo "gofumpt not installed, skipping"
	@command -v terraform >/dev/null 2>&1 \
		&& terraform fmt -recursive deploy/terraform \
		|| echo "terraform not installed, skipping tf fmt"

.PHONY: validate
validate: ## Validate compose, collector, Prometheus rules and kustomize output
	$(COMPOSE) config --quiet
	@echo "compose ok"
	$(DOCKER) run --rm -v "$(PWD)/deploy/otel:/etc/otel:ro" \
		-e AGENTGATE_ENV=dev -e OTEL_GATEWAY_HOST=localhost \
		-e TRACES_OTLP_ENDPOINT=localhost:4317 -e CONTENT_OTLP_ENDPOINT=localhost:4317 \
		-e LOKI_ENDPOINT=http://localhost:3100/otlp -e TAIL_SAMPLING_BASELINE_PERCENT=5 \
		otel/opentelemetry-collector-contrib:0.115.1 validate --config=/etc/otel/collector-gateway.yaml
	@echo "collector ok"
	$(DOCKER) run --rm -v "$(PWD)/deploy/prometheus:/p:ro" \
		prom/prometheus:v3.1.0 promtool check rules /p/rules/slo.rules.yml /p/rules/operational.rules.yml
	@echo "prometheus rules ok"
	@for env in dev staging prod; do \
		$(KUSTOMIZE) build $(KUSTOMIZE_FLAGS) deploy/k8s/overlays/$$env > /dev/null || exit 1; \
		echo "kustomize $$env ok"; \
	done

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
# `make run` is the thirty-second version of this repo: two Go processes, no
# Docker, no Postgres, no Redis, no identity provider, no key material. It is
# the shipped config/gateway.yaml, which turns every dependency off on purpose
# (in-memory rate limiter, in-memory cache, builtin guardrails, and
# identity.allow_unverified so callers need no token). `make run-stack` below
# is the same gateway wired to the real thing.

.PHONY: run
run: ## Clean clone to a live gateway on :8080 — no Docker, no config, no keys
	@command -v $(GO) >/dev/null 2>&1 || { echo "go is required: https://go.dev/dl/"; exit 1; }
	@command -v curl >/dev/null 2>&1 || { echo "curl is required"; exit 1; }
	@mkdir -p $(BIN_DIR)
	@echo "building gateway and mockprovider (vendored deps, no network needed)"
	@CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/mockprovider ./cmd/mockprovider
	@CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/gateway ./cmd/gateway
	@$(BIN_DIR)/mockprovider > $(BIN_DIR)/mockprovider.log 2>&1 & \
		mock=$$!; \
		$(BIN_DIR)/gateway -config config/gateway.yaml > $(BIN_DIR)/gateway.log 2>&1 & \
		gw=$$!; \
		trap 'kill $$mock $$gw 2>/dev/null; exit 0' INT TERM; \
		trap 'kill $$mock $$gw 2>/dev/null' EXIT; \
		ready=""; \
		for _ in $$(seq 1 60); do \
			if curl -sf -o /dev/null $(GATEWAY_URL)/healthz 2>/dev/null; then ready=yes; break; fi; \
			sleep 0.25; \
		done; \
		echo; \
		if [ -z "$$ready" ]; then echo "  gateway did not come up on $(GATEWAY_URL) — see $(BIN_DIR)/gateway.log"; exit 1; fi; \
		echo "  gateway ready on $(GATEWAY_URL)   mock model backend on http://localhost:8090"; \
		echo; \
		echo "  in another terminal:"; \
		echo; \
		echo "    make demo          # runs the three requests below for you"; \
		echo; \
		echo "    curl -s $(GATEWAY_URL)/healthz"; \
		echo "    curl -s $(GATEWAY_URL)/v1/models"; \
		echo "    curl -s $(GATEWAY_URL)/v1/chat/completions -H 'content-type: application/json' \\"; \
		echo "      -d '{\"model\":\"general-chat\",\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}],\"max_tokens\":32}'"; \
		echo; \
		echo "  no Authorization header is needed: identity.allow_unverified is on in config/gateway.yaml."; \
		echo "  logs: $(BIN_DIR)/gateway.log and $(BIN_DIR)/mockprovider.log"; \
		echo "  ctrl-c stops both processes. make run-stack brings up the full wired stack instead."; \
		echo; \
		wait $$gw

.PHONY: demo
demo: ## Drive a gateway started by `make run`: a normal call, a cache hit, a blocked secret
	@command -v curl >/dev/null 2>&1 || { echo "curl is required"; exit 1; }
	@curl -sf -o /dev/null $(GATEWAY_URL)/healthz 2>/dev/null || { \
		echo "no gateway on $(GATEWAY_URL) — run 'make run' in another terminal first"; exit 1; }
	@echo
	@echo "1. a normal completion — note the x-agentgate-* headers describing the decision"
	@curl -s -D - -o /dev/null $(GATEWAY_URL)/v1/chat/completions \
		-H 'content-type: application/json' \
		-d '{"model":"general-chat","messages":[{"role":"user","content":"what happened to this payment"}],"max_tokens":32}' \
		| grep -iE '^(HTTP/|x-agentgate-)' || true
	@echo
	@echo "2. the same request again — served from cache this time"
	@curl -s -D - -o /dev/null $(GATEWAY_URL)/v1/chat/completions \
		-H 'content-type: application/json' \
		-d '{"model":"general-chat","messages":[{"role":"user","content":"what happened to this payment"}],"max_tokens":32}' \
		| grep -iE '^(HTTP/|x-agentgate-cache)' || true
	@echo
	@echo "3. a prompt carrying an AWS access key — the builtin guardrail refuses it"
	@curl -s -o /dev/null -w '   HTTP %{http_code}\n' $(GATEWAY_URL)/v1/chat/completions \
		-H 'content-type: application/json' \
		-d '{"model":"general-chat","messages":[{"role":"user","content":"use key AKIAIOSFODNN7EXAMPLE"}],"max_tokens":32}'
	@echo

# ---------------------------------------------------------------------------
# Local stack
# ---------------------------------------------------------------------------

# The key is 644, not 600: the controlplane container runs as uid 65532 and
# bind-mounts it read-only, so an owner-only mode is unreadable in-container.
# It is dev-only, gitignored and regenerated on demand.
.PHONY: dev-keys
dev-keys:
	@mkdir -p deploy/compose/keys
	@test -f deploy/compose/keys/signing.pem || { \
		echo "generating local token signing key (never leaves this machine, never committed)"; \
		openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 \
			-out deploy/compose/keys/signing.pem 2>/dev/null; \
		chmod 644 deploy/compose/keys/signing.pem; }

.PHONY: run-stack
run-stack: dev-keys ## Build and start the full local stack
	VERSION=$(VERSION) COMMIT=$(COMMIT) BUILD_DATE=$(BUILD_DATE) \
		$(COMPOSE) up -d --build --wait
	@echo
	@echo "  gateway      $(GATEWAY_URL)          /v1, /healthz, /readyz"
	@echo "  controlplane $(CP_URL)"
	@echo "  fleetview    http://localhost:8082"
	@echo "  grafana      http://localhost:3000   (anonymous viewer)"
	@echo "  prometheus   http://localhost:9099"
	@echo "  traces       http://localhost:16686"
	@echo
	@echo "  next: make smoke"

.PHONY: stop-stack
stop-stack: ## Stop the local stack, keeping volumes
	$(COMPOSE) down --remove-orphans

.PHONY: reset-stack
reset-stack: ## Stop the local stack and delete its volumes
	$(COMPOSE) down --remove-orphans --volumes

.PHONY: logs
logs: ## Follow stack logs: make logs [SVC=gateway]
	@if [ -n "$(SVC)" ]; then $(COMPOSE) logs -f --tail=200 $(SVC); \
	else $(COMPOSE) logs -f --tail=100; fi

.PHONY: smoke
smoke: ## End-to-end request through the gateway, asserting the frozen contract
	@echo "==> health"
	@curl -fsS $(GATEWAY_URL)/healthz > /dev/null && echo "    gateway healthz ok"
	@curl -fsS $(GATEWAY_URL)/readyz  > /dev/null && echo "    gateway readyz ok"
	@curl -fsS $(CP_URL)/healthz      > /dev/null && echo "    controlplane healthz ok"
	@curl -fsS $(FLEET_URL)/healthz   > /dev/null && echo "    fleetview healthz ok"
	@echo "==> registering the example agent and issuing a credential"
	@REG=$$(AGENTGATE_CONTROLPLANE_URL=$(CP_URL) AGENTGATE_ACTOR=smoke@local \
		$(GO) run ./cmd/agentctl register -f examples/agent.yaml -env dev -credential 2>&1); \
	echo "$$REG" | sed 's/^/    /'; \
	CLIENT_ID=$$(echo "$$REG" | awk '/^client_id:/{print $$2}'); \
	CLIENT_SECRET=$$(echo "$$REG" | awk '/^client_secret:/{print $$2}'); \
	test -n "$$CLIENT_ID" || { echo "    registration did not return a credential"; exit 1; }; \
	echo "==> exchanging it for an access token"; \
	TOKEN=$$(AGENTGATE_CONTROLPLANE_URL=$(CP_URL) $(GO) run ./cmd/agentctl token \
		-client-id "$$CLIENT_ID" -client-secret "$$CLIENT_SECRET" -env dev -q); \
	test -n "$$TOKEN" || { echo "    could not mint a token"; exit 1; }; \
	echo "    token acquired"; \
	echo "==> POST /v1/chat/completions"; \
	HDRS=$$(mktemp); \
	BODY=$$(curl -fsS -D $$HDRS -X POST $(GATEWAY_URL)/v1/chat/completions \
		-H "authorization: Bearer $$TOKEN" \
		-H 'content-type: application/json' \
		-H 'x-agentgate-session-id: smoke-1' \
		-d '{"model":"general-chat","messages":[{"role":"user","content":"reply with the single word: ok"}],"max_tokens":16,"temperature":0}'); \
	echo "$$BODY" | jq -e '.choices[0].message.content' > /dev/null || { echo "    unexpected body: $$BODY"; rm -f $$HDRS; exit 1; }; \
	echo "    content:  $$(echo "$$BODY" | jq -r '.choices[0].message.content' | head -c 60)"; \
	echo "==> contract headers (SPEC 2.3)"; \
	for h in x-agentgate-request-id x-agentgate-trace-id x-agentgate-provider \
	         x-agentgate-model x-agentgate-pool x-agentgate-attempts \
	         x-agentgate-cache x-agentgate-tokens-input x-agentgate-tokens-output \
	         x-agentgate-cost-usd x-agentgate-guardrail; do \
		v=$$(grep -i "^$$h:" $$HDRS | tr -d '\r' | cut -d' ' -f2-); \
		test -n "$$v" || { echo "    MISSING $$h"; rm -f $$HDRS; exit 1; }; \
		printf '    %-30s %s\n' "$$h" "$$v"; \
	done; \
	rm -f $$HDRS; \
	echo "==> the same request again should be served from cache"; \
	CACHE=$$(curl -fsS -o /dev/null -D - -X POST $(GATEWAY_URL)/v1/chat/completions \
		-H "authorization: Bearer $$TOKEN" -H 'content-type: application/json' \
		-d '{"model":"general-chat","messages":[{"role":"user","content":"reply with the single word: ok"}],"max_tokens":16,"temperature":0}' \
		| grep -i '^x-agentgate-cache:' | tr -d '\r' | cut -d' ' -f2-); \
	echo "    x-agentgate-cache              $$CACHE"; \
	echo "==> a credential in the prompt must be refused"; \
	STATUS=$$(curl -s -o /dev/null -w '%{http_code}' -X POST $(GATEWAY_URL)/v1/chat/completions \
		-H "authorization: Bearer $$TOKEN" -H 'content-type: application/json' \
		-d '{"model":"general-chat","messages":[{"role":"user","content":"use AKIAIOSFODNN7EXAMPLE"}],"max_tokens":8}'); \
	test "$$STATUS" = "403" || { echo "    guardrail did not block, got $$STATUS"; exit 1; }; \
	echo "    blocked with 403 as expected"; \
	echo "==> streaming"; \
	curl -fsS -N -X POST $(GATEWAY_URL)/v1/chat/completions \
		-H "authorization: Bearer $$TOKEN" -H 'content-type: application/json' \
		-d '{"model":"general-chat","messages":[{"role":"user","content":"count to three"}],"max_tokens":32,"stream":true,"stream_options":{"include_usage":true}}' \
		| grep -q 'data: \[DONE\]' && echo "    stream terminated with [DONE]"; \
	echo "==> fleet view"; \
	sleep 2; \
	curl -fsS $(FLEET_URL)/api/v1/agents | jq -r '.agents[] | "    " + .identity + "  health=" + .health'; \
	curl -fsS "$(FLEET_URL)/api/v1/chargeback?period=day" | jq -r '"    spend today: $$" + (.total_usd|tostring)'; \
	echo "==> smoke passed"


.PHONY: dev-token
dev-token: ## Print a fresh access token for the example agent (local stack only)
	@REG=$$(AGENTGATE_CONTROLPLANE_URL=$(CP_URL) AGENTGATE_ACTOR=loadgen@local \
		$(GO) run ./cmd/agentctl register -f examples/agent.yaml -env dev -credential 2>&1); \
	CLIENT_ID=$$(echo "$$REG" | awk '/^client_id:/{print $$2}'); \
	CLIENT_SECRET=$$(echo "$$REG" | awk '/^client_secret:/{print $$2}'); \
	test -n "$$CLIENT_ID" || { echo "$$REG" >&2; echo "registration did not return a credential" >&2; exit 1; }; \
	AGENTGATE_CONTROLPLANE_URL=$(CP_URL) $(GO) run ./cmd/agentctl token \
		-client-id "$$CLIENT_ID" -client-secret "$$CLIENT_SECRET" -env dev -q

.PHONY: load
load: ## Run the load test against the local stack (mints a token unless AGENTGATE_TOKEN is set)
	# The mockprovider profiles in docker-compose.yml are deliberately uneven
	# (1% / 8% / 0% failure) so a load run exercises retry, failover and the
	# breaker rather than only measuring throughput.
	@TOKEN=$${AGENTGATE_TOKEN:-}; \
	if [ -z "$$TOKEN" ]; then TOKEN=$$($(MAKE) -s dev-token); fi; \
	test -n "$$TOKEN" || { echo "could not obtain a token; set AGENTGATE_TOKEN or start the stack (make run-stack)" >&2; exit 1; }; \
	$(GO) run ./cmd/loadgen \
		-url=$(GATEWAY_URL) \
		-token="$$TOKEN" \
		-d=$${DURATION:-60s} \
		-rate=$${RATE:-50} \
		-c=$${CONCURRENCY:-25} \
		-model=$${MODEL:-general-chat} \
		-stream=$${STREAM:-true}

# ---------------------------------------------------------------------------
# Images
# ---------------------------------------------------------------------------

.PHONY: docker
docker: ## Build container images for every service
	@for svc in $(IMAGES); do \
		echo "==> $(REGISTRY)/$$svc:$(VERSION)"; \
		DOCKER_BUILDKIT=1 $(DOCKER) build \
			-f deploy/docker/Dockerfile \
			--build-arg SERVICE=$$svc \
			--build-arg VERSION=$(VERSION) \
			--build-arg COMMIT=$(COMMIT) \
			--build-arg BUILD_DATE=$(BUILD_DATE) \
			-t $(REGISTRY)/$$svc:$(VERSION) \
			-t $(REGISTRY)/$$svc:latest \
			. || exit 1; \
	done

.PHONY: docker-push
docker-push: ## Build and push multi-arch images
	# Refuses to publish a dirty tree: an image tagged with a version that does
	# not exist in git is an image nobody can reproduce or roll back to.
	@case "$(VERSION)" in *-dirty) echo "refusing to push a dirty build ($(VERSION)) — commit first"; exit 1;; esac
	@$(DOCKER) buildx inspect agentgate >/dev/null 2>&1 || $(DOCKER) buildx create --name agentgate --use
	@for svc in $(IMAGES); do \
		echo "==> pushing $(REGISTRY)/$$svc:$(VERSION)"; \
		$(DOCKER) buildx build \
			-f deploy/docker/Dockerfile \
			--platform=$(PLATFORMS) \
			--build-arg SERVICE=$$svc \
			--build-arg VERSION=$(VERSION) \
			--build-arg COMMIT=$(COMMIT) \
			--build-arg BUILD_DATE=$(BUILD_DATE) \
			--provenance=true --sbom=true \
			-t $(REGISTRY)/$$svc:$(VERSION) \
			--push . || exit 1; \
	done

# ---------------------------------------------------------------------------
# Kubernetes
# ---------------------------------------------------------------------------

.PHONY: k8s-render
k8s-render: ## Render an overlay to stdout: make k8s-render ENV=prod
	@$(KUSTOMIZE) build $(KUSTOMIZE_FLAGS) deploy/k8s/overlays/$(ENV)

.PHONY: k8s-diff
k8s-diff: ## Diff an overlay against the live cluster: make k8s-diff ENV=prod
	@$(KUSTOMIZE) build $(KUSTOMIZE_FLAGS) deploy/k8s/overlays/$(ENV) | $(KUBECTL) diff -f - || true

.PHONY: k8s-dev
k8s-dev: ## Apply the dev overlay
	@$(MAKE) --no-print-directory k8s-apply ENV=dev

.PHONY: k8s-staging
k8s-staging: ## Apply the staging overlay
	@$(MAKE) --no-print-directory k8s-apply ENV=staging

.PHONY: k8s-prod
k8s-prod: ## Apply the prod overlay (asks for confirmation)
	@echo "About to apply to PRODUCTION using context: $$($(KUBECTL) config current-context)"
	@read -r -p "Type the context name to continue: " confirm; \
		test "$$confirm" = "$$($(KUBECTL) config current-context)" || { echo "aborted"; exit 1; }
	@$(MAKE) --no-print-directory k8s-apply ENV=prod

.PHONY: k8s-apply
k8s-apply:
	$(KUSTOMIZE) build $(KUSTOMIZE_FLAGS) deploy/k8s/overlays/$(ENV) | $(KUBECTL) apply -f -
	$(KUBECTL) -n agentgate rollout status deployment/gateway --timeout=5m
	$(KUBECTL) -n agentgate rollout status deployment/controlplane --timeout=5m

# ---------------------------------------------------------------------------
# Terraform
# ---------------------------------------------------------------------------

.PHONY: tf-azure-plan
tf-azure-plan: ## Plan the Azure stack: make tf-azure-plan ENV=prod
	terraform -chdir=deploy/terraform/azure init -reconfigure \
		-backend-config=backends/$(ENV).hcl
	terraform -chdir=deploy/terraform/azure validate
	terraform -chdir=deploy/terraform/azure plan \
		-var-file=$(abspath env/$(ENV).azure.tfvars) \
		-out=$(ENV).tfplan

.PHONY: tf-azure-apply
tf-azure-apply: ## Apply a previously-generated Azure plan: make tf-azure-apply ENV=prod
	# Applies the saved plan file, never a fresh one. Applying without a plan
	# means approving changes nobody reviewed.
	terraform -chdir=deploy/terraform/azure apply $(ENV).tfplan

.PHONY: tf-aws-plan
tf-aws-plan: ## Plan the AWS stack: make tf-aws-plan ENV=prod
	terraform -chdir=deploy/terraform/aws init -reconfigure \
		-backend-config=backends/$(ENV).hcl
	terraform -chdir=deploy/terraform/aws validate
	terraform -chdir=deploy/terraform/aws plan \
		-var-file=$(abspath env/$(ENV).aws.tfvars) \
		-out=$(ENV).tfplan

.PHONY: tf-aws-apply
tf-aws-apply: ## Apply a previously-generated AWS plan: make tf-aws-apply ENV=prod
	terraform -chdir=deploy/terraform/aws apply $(ENV).tfplan

# ---------------------------------------------------------------------------
# Housekeeping
# ---------------------------------------------------------------------------

.PHONY: clean
clean: ## Remove build output and local test artefacts
	rm -rf $(BIN_DIR) coverage.out coverage.html
	rm -f deploy/terraform/azure/*.tfplan deploy/terraform/aws/*.tfplan
	$(GO) clean -testcache

.PHONY: version
version: ## Print the version stamp that would be built
	@echo "version    $(VERSION)"
	@echo "commit     $(COMMIT)"
	@echo "build_date $(BUILD_DATE)"
