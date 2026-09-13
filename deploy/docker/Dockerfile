# syntax=docker/dockerfile:1.9
#
# AgentGate — one Dockerfile for every binary in the module.
#
#   docker build -f deploy/docker/Dockerfile \
#     --build-arg SERVICE=gateway \
#     --build-arg VERSION=$(git describe --tags --always --dirty) \
#     --build-arg COMMIT=$(git rev-parse --short HEAD) \
#     --build-arg BUILD_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ) \
#     -t ghcr.io/agentgate/gateway:dev .
#
# Build context is the REPO ROOT (the Go module root), not this directory.

ARG GO_VERSION=1.24
# Which runtime flavour to finish on. `plain` is pure distroless and is what
# ships to Kubernetes, where the kubelet performs HTTP probes and the image
# therefore needs no client tooling at all. `healthcheck` adds a single static
# busybox purely so `docker compose` (which executes probes *inside* the
# container) can run an HTTP healthcheck locally. Keeping them separate means
# production images carry no shell and no network client.
ARG RUNTIME_VARIANT=plain

# ---------------------------------------------------------------------------
# Stage 1 — build
# ---------------------------------------------------------------------------
# --platform=$BUILDPLATFORM keeps the toolchain native and cross-compiles via
# GOOS/GOARCH; that is far faster than emulating the target under QEMU and is
# safe because CGO is off, so there is nothing platform-specific to link.
FROM --platform=${BUILDPLATFORM} golang:${GO_VERSION}-bookworm AS build

ARG SERVICE
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=1970-01-01T00:00:00Z
ARG TARGETOS=linux
ARG TARGETARCH=amd64

WORKDIR /src

# Copy the module graph first so dependency download is cached independently of
# source edits. `go.su[m]` is a glob so a module with no external deps (and thus
# no go.sum) still builds instead of failing the COPY.
COPY go.mod go.su[m] ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# CGO_ENABLED=0 gives a fully static binary, which is what makes the distroless
# *static* base (no libc at all) viable and removes glibc CVE churn from the
# runtime image.
# -trimpath strips build-host paths so builds are reproducible and no local
# filesystem layout leaks into panics or the binary.
# -tags timetzdata embeds the tzdata database, because distroless static has no
# /usr/share/zoneinfo and cost/chargeback rollups are computed on wall-clock
# boundaries in a named timezone.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    test -n "${SERVICE}" || (echo "build-arg SERVICE is required" >&2; exit 1) && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build \
      -trimpath \
      -tags timetzdata \
      -ldflags "-s -w \
        -X github.com/agentgate/agentgate/internal/version.Version=${VERSION} \
        -X github.com/agentgate/agentgate/internal/version.Commit=${COMMIT} \
        -X github.com/agentgate/agentgate/internal/version.BuildDate=${BUILD_DATE}" \
      -o /out/agentgate-service \
      ./cmd/${SERVICE}

# ---------------------------------------------------------------------------
# Stage 2 — runtime bases
# ---------------------------------------------------------------------------
# distroless static: no shell, no package manager, no libc. `:nonroot` bakes in
# uid/gid 65532 and a writable-nothing filesystem, which is what lets the
# Kubernetes manifests set readOnlyRootFilesystem + runAsNonRoot without any
# further image work.
FROM gcr.io/distroless/static-debian12:nonroot AS runtime-plain

FROM busybox:1.37.0-uclibc AS busybox

FROM runtime-plain AS runtime-healthcheck
# Single static binary, no shell registered as ENTRYPOINT, used only by the
# compose healthcheck. Deliberately parked outside PATH.
COPY --from=busybox /bin/busybox /busybox/busybox

# ---------------------------------------------------------------------------
# Stage 3 — final
# ---------------------------------------------------------------------------
FROM runtime-${RUNTIME_VARIANT} AS final

ARG SERVICE
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=1970-01-01T00:00:00Z

LABEL org.opencontainers.image.title="agentgate-${SERVICE}" \
      org.opencontainers.image.description="AgentGate ${SERVICE} — enterprise agentic AI control plane" \
      org.opencontainers.image.vendor="AgentGate" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.source="https://github.com/agentgate/agentgate" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      io.agentgate.service="${SERVICE}"

COPY --from=build /out/agentgate-service /usr/local/bin/agentgate-service

# Explicit numeric uid:gid (not the name) so Kubernetes can assert
# runAsNonRoot/runAsUser without resolving /etc/passwd inside the image.
USER 65532:65532

# Documentation only — the actual listeners come from config/env.
# gateway 8080/9090 · controlplane 8081/9091 · fleetview 8082/9092
# guardrails 8083 · mockprovider 8090
EXPOSE 8080 8081 8082 8083 8090 9090 9091 9092

ENV AGENTGATE_ENV=dev \
    AGENTGATE_LOG_LEVEL=info

ENTRYPOINT ["/usr/local/bin/agentgate-service"]
