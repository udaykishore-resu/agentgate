# ADR 0014 — A deliberately minimal dependency set

**Status:** Accepted
**Date:** 2026-08-26
**Deciders:** Platform lead, client security architecture, client third-party risk function
**Consulted:** Two consuming teams, SRE, the client's open-source review board
**Affects:** `go.mod`, `internal/telemetry`, `internal/yamlite`, `internal/provider` (SigV4 and
event-stream), build and supply-chain controls, `docs/10-network-security.md`

---

## Context

AgentGate runs inside a regulated financial-services estate. In that estate, adding a third-party
Go module is not a `go get`. It is:

- an entry on the software bill of materials, reviewed by the open-source review board;
- a licence assessment;
- a transitive dependency tree that inherits all of the above;
- a continuing obligation: every CVE against any node in that tree becomes a ticket with an SLA,
  regardless of whether the affected code path is reachable from this application.

The client's review board has a measured median turnaround of six weeks per new direct dependency
and its transitives. That number is the constraint. It is given, not chosen, and it does not move
because a platform team finds it inconvenient.

Against that, the platform needs five capabilities that are conventionally supplied by well-known
libraries: OpenTelemetry tracing, Prometheus metrics exposition, YAML configuration parsing, AWS
SigV4 request signing for Bedrock, and the AWS event-stream binary framing that Bedrock uses for
streaming responses. The obvious modules for those — `go.opentelemetry.io/otel` and its exporters,
`prometheus/client_golang`, `gopkg.in/yaml.v3`, `aws/aws-sdk-go-v2` and its `smithy-go` framing —
between them pull in a large number of transitive modules.

A decision is forced because the alternative is not "use the libraries and move on". It is "spend
the first several months of the engagement in review board queues, or ship something".

## Decision Drivers

| # | Driver | Weight |
|---|---|---|
| 1 | Every third-party module is a security-review line item and a supply-chain surface, with a measured six-week median cost | Decisive |
| 2 | The whole dependency set must be auditable end to end by a reviewer who is not a Go specialist | High |
| 3 | The platform must not carry maintenance the ecosystem would otherwise carry, unless the cost above justifies it | High |
| 4 | The capabilities needed are narrow subsets of what the libraries provide | High |
| 5 | Interoperability must be exact: an AgentGate span must be indistinguishable, at the collector, from an SDK span | High |
| 6 | The decision must be reversible without a rewrite | Medium |

Driver 1 alone would decide this if the others were neutral. Driver 3 is the honest counterweight
and is the reason the Consequences section below is as long as it is.

## Options Considered

### Option A — Use the standard libraries for everything

Adopt `go.opentelemetry.io/otel` with the OTLP exporters, `prometheus/client_golang`,
`gopkg.in/yaml.v3` and `aws-sdk-go-v2`.

| Pros | Cons |
|---|---|
| Zero bespoke code for any of the five capabilities | Roughly thirty additional modules on the SBOM once transitives are counted |
| Full protocol coverage: OTLP metrics and logs, exemplars, all AWS services | At six weeks median per review, the platform ships months late for capabilities it uses a fraction of |
| Maintained by the communities that own the specifications | Every CVE in any transitive module becomes an SLA-bound ticket, whether or not the code path is reachable |
| Ecosystem instrumentation works out of the box | The client's review board would reasonably ask why an AI gateway needs a full AWS SDK to call one API |
| Upgrades track specification changes automatically | `aws-sdk-go-v2` alone is a large tree for a single signed POST |

Not rejected on technical grounds. Rejected on driver 1, and that rejection has a cost which is
recorded honestly below.

### Option B — Vendor a minimal set and implement the narrow subsets in-tree

Keep three direct dependencies and write the five capabilities against their specifications.

| Pros | Cons |
|---|---|
| Five modules total on the SBOM, all reviewable in an afternoon | **The project now owns code the OpenTelemetry and Prometheus communities would otherwise maintain** |
| Each in-tree implementation is a few hundred lines a reviewer can read end to end | The OTLP exporter implements a subset, and the boundaries of that subset must be documented and remembered |
| The subset is exactly what the platform uses, so there is no unreachable attack surface | A specification change is the platform's problem, not an upgrade |
| Fully testable against the real collector, which is the only conformance test that matters | Reviewers unfamiliar with the codebase will ask "why did you write your own OTLP exporter", every time |
| CVE exposure is limited to code the project can actually read | Bus factor: five small bespoke components is five things a new engineer must learn |

### Option C — Vendor the libraries' source into the repository

Copy the upstream code in rather than depending on the modules.

| Pros | Cons |
|---|---|
| No module resolution at build time | The SBOM obligation is unchanged; the code is still third-party, just harder to attribute |
| Pinned by construction | Upgrades become manual merges, which means they will not happen |
| | Combines the maintenance burden of Option B with the review surface of Option A |

Rejected quickly. It is the worst of both options and it makes the licence position harder to
state, not easier.

### Option D — Split the difference: libraries for telemetry, bespoke for the rest

Take `go.opentelemetry.io/otel` and `prometheus/client_golang`, write the YAML parser and the AWS
pieces.

| Pros | Cons |
|---|---|
| The two components with the strongest maintenance argument come from upstream | The OTel and Prometheus trees are the largest of the four; taking them keeps most of the review cost |
| Full OTLP coverage including metrics and logs | The saving relative to Option A is small; the saving relative to Option B is nearly all of it given up |
| Standard instrumentation interoperates | Still ships months late |

Rejected on cost/benefit. It is the option to revisit first if the review-board constraint changes,
which is why it is recorded here rather than dismissed.

## Decision

**AgentGate depends on three third-party Go modules and the standard library. The five capabilities
listed above are implemented in-tree as documented subsets, behind interfaces that permit the
upstream implementations to be substituted without changing calling code.**

### The dependency set

| Module | Why it is not written in-tree |
|---|---|
| `github.com/golang-jwt/jwt/v5` | **Cryptographic verification.** Writing a JWT verifier by hand is how `alg: none` and algorithm-confusion vulnerabilities get shipped. This is the exact case where a widely reviewed implementation is the safer choice, and the security architecture function said so first. |
| `github.com/redis/go-redis/v9` | The distributed rate limiter needs correct RESP protocol handling, connection pooling, cluster topology and Lua script management. Reimplementing that is a project, not a file, and getting the pooling subtly wrong is an outage under load. |
| `github.com/google/uuid` | Small, stable, universally reviewed, and the alternative is hand-rolled identifier generation, which nobody should write again. |

Their transitive closure is two further modules — `cespare/xxhash/v2` and `dgryski/go-rendezvous`,
both pulled in by `go-redis`. **Five modules in total.** The complete `go.sum` fits on one screen,
which is the property driver 2 asked for.

### What is implemented in-tree, and exactly what is and is not covered

**`internal/telemetry` — OTLP/HTTP JSON trace exporter (`otlp.go`)**

| Implemented | Not implemented |
|---|---|
| OTLP/HTTP with the **JSON** encoding, which the collector's `otlp` receiver accepts natively | OTLP over **gRPC** |
| **Traces only** | **OTLP metrics** and **OTLP logs** |
| Resource attributes; instrumentation scope; span name, kind, parent, trace state | Span **links** |
| Span attributes: string, bool, int, double, and homogeneous arrays | Other array and nested `kvlist` value types |
| Span **events**, used for `gen_ai.content.prompt` / `.completion` capture | |
| Span **status** code and message | |
| Batching with a bounded queue, batch size and flush timeout | Persistent (on-disk) queueing across a restart |
| Retry with quadratic backoff on 429 and 5xx; no retry on 4xx | **gzip request compression** |
| Configurable endpoint, headers and timeout | Partial-success response handling — a partial success is treated as a success |

Metrics deliberately do **not** travel over OTLP. Prometheus scrapes each service directly (ADR
0012), so a collector outage cannot silently lose the signals the SLOs are computed from. Logs go
to the collector via the standard `log/slog` handler and the collector's own receivers, not through
this exporter.

JSON rather than protobuf is a second deliberate choice: it keeps the binary free of a protobuf
dependency, and it makes the payloads readable during a network security review — which, in this
estate, is a review artefact rather than a curiosity.

**`internal/telemetry` — Prometheus text exposition (`metrics.go`)**

| Implemented | Not implemented |
|---|---|
| Counter, gauge, histogram | Summary with quantiles |
| Prometheus **text exposition format** on a dedicated operator listener | **OpenMetrics** format, and therefore **exemplars** |
| Label names fixed at registration, values supplied positionally | Dynamic label sets, `MustCurryWith`, vector partial matching |
| Bounded label-value normalisation so a caller mistake degrades to `unknown` rather than panicking in the request path | Native histograms |
| Fixed bucket boundaries tuned per instrument to the SLO thresholds | Go runtime and process collectors |

The absence of exemplars is the one omission with real cost: it means a latency bucket cannot be
clicked through to an example trace. The trace id is on every log line and in every response header,
so the join is available; it is one step less convenient.

**`internal/yamlite` — YAML subset parser**

Supports comments, block mappings by indentation, block sequences, flow sequences and mappings,
literal and folded block scalars, quoted scalars and document separators. Explicitly rejects, with a
line-numbered error rather than a silent mis-parse: anchors and aliases, tags, multiple documents,
complex keys, and tab indentation.

It parses to a generic tree and round-trips through `encoding/json`, so struct tags, custom
unmarshalers and type conversion behave exactly as they do for JSON configuration — one set of
semantics, not two.

The configuration format is fixed and small, and every file the platform ships parses with it. A
consuming team's `agent.yaml` also parses with it, which is why `examples/agent.yaml` documents the
dialect prominently.

**`internal/provider/sigv4.go` — AWS Signature Version 4**

The canonical request, credential scope, string-to-sign and signing-key derivation, producing the
`Authorization` and `x-amz-*` headers. It signs one operation against one service. It does not do
credential resolution chains, region discovery, endpoint resolution, retries or any other AWS
service — the deployed environment supplies credentials from the instance role, and the static
fields exist for local testing against the mock provider.

**`internal/provider/eventstream.go` — AWS event-stream decoder**

The binary framing Bedrock uses for streaming responses instead of SSE: prelude, header block, and
payload, with length arithmetic that rejects malformed frames. **The CRC32 checksums are not
verified.** The transport is TLS over a single connection, so a corrupted frame means a broken
connection rather than a bit flip, and the length arithmetic already rejects a malformed frame.
That is a deliberate, documented simplification and it is stated in the source at the point where a
reader would otherwise assume the CRCs were checked.

### The escape hatch

`telemetry.Exporter` is a two-method interface — `ExportSpans` and `Shutdown`. The in-tree OTLP
exporter is one implementation of it. Substituting the upstream OpenTelemetry SDK means writing an
adapter that satisfies that interface and passing it to `telemetry.New`; **no calling code changes**,
because no calling code references the exporter type.

This is not a theoretical property. It is the specific thing that makes this decision reversible,
and it is why the interface exists at all rather than the exporter being called directly.

The same shape does not exist for the other four. `yamlite` is swapped by changing one `Unmarshal`
call site. The SigV4 signer and the event-stream decoder are internal to the Bedrock adapter and
are swapped by rewriting that adapter, which is bounded work in one package.

### Supply-chain controls this decision assumes

A small dependency set is not on its own a supply-chain control. The following are conditions of
this decision, not optional extras:

- Every module pinned by exact version and hash in `go.sum`, verified against the checksum database
  at build time.
- `GOFLAGS=-mod=readonly` in CI: a build that would change `go.mod` fails rather than resolving.
- `govulncheck` in CI, gating the build.
- A new direct dependency is an ADR, not a pull request. There is no lightweight path.

## Consequences

### Positive

- **Five modules on the SBOM.** The whole dependency set can be reviewed by one person in an
  afternoon, which is the difference between a security review that happens and one that is
  perpetually scheduled.
- **The bespoke components are readable end to end.** The OTLP exporter is a few hundred lines of
  JSON marshalling; the SigV4 signer is a page. A reviewer can satisfy themselves about what they
  do without trusting a transitive tree.
- **No unreachable attack surface.** The platform does not carry an OTLP gRPC stack it never opens
  or an AWS SDK for services it never calls.
- **CVE exposure is bounded and mostly actionable.** A finding lands in code the team can read and
  fix the same day, rather than in a transitive module awaiting an upstream release.
- **Interoperability is exact and continuously verified.** The exporter POSTs to a real
  OpenTelemetry Collector in the local stack and in integration tests. Wire conformance against the
  collector is the only conformance test that matters, and it runs on every change.
- **Build times and image sizes are small**, which matters more than it sounds when the deployment
  pipeline runs per-environment.

### Negative

- **The project owns code the OpenTelemetry and Prometheus communities would otherwise maintain.**
  This is the real cost and it should not be softened. When the OTLP specification changes, when a
  new span field becomes conventional, when the exposition format gains a feature — that is now the
  platform team's work, competing with everything else on the roadmap.
- **The exporter implements a subset of OTLP, not all of it.** No gRPC, no metrics, no logs, no span
  links, no compression, no persistent queue, and partial-success responses are treated as success.
  Each of those is a gap someone will eventually hit, and the person who hits it will not have read
  this ADR. The table above exists so that the answer takes minutes to find rather than an
  afternoon of source reading.
- **No exemplars.** A Prometheus latency bucket cannot be clicked through to an example trace. The
  join is available via the trace id in logs and response headers, but it is a manual step.
- **The event-stream decoder does not verify CRCs.** The reasoning is sound and documented, but it
  is a deviation from the specification and a reviewer is entitled to flag it.
- **The YAML subset will eventually reject a file someone expects to work.** An anchor is a
  perfectly ordinary YAML feature and a team that uses one will get an error rather than a parse.
  The error names the line and says what is unsupported, which is the best that can be done, but it
  is still friction the ecosystem parser would not have caused.
- **Bus factor and onboarding cost.** Five bespoke components are five things a new engineer must
  learn that would otherwise have been transferable knowledge. An engineer who knows the
  OpenTelemetry SDK arrives knowing nothing about `internal/telemetry`.
- **The decision will be questioned by every new reviewer**, and answering takes this document plus
  a conversation. That is a recurring cost with no end date.

### Neutral

- Ecosystem auto-instrumentation packages do not apply. The platform's own instrumentation is
  explicit and hand-placed throughout the policy chain, which was going to be true regardless: the
  spans that matter here are `gateway.policy.<stage>`, and no auto-instrumentation would have
  produced those.
- The dependency count is not itself a virtue and this ADR does not claim it is. A dependency that
  earns its review cost should be taken — `golang-jwt`, `go-redis` and `uuid` are exactly that.
- This policy applies to the Go services only. The example clients, the load generator's
  reporting, and any operational tooling are free to depend on whatever they like, because they are
  not in the deployed artefact.

### What this forecloses

- **OTLP metrics and logs from the services themselves.** Getting them means either implementing
  more of OTLP or taking the SDK. Metrics currently reach Prometheus by scrape (ADR 0012) and logs
  reach the collector through its own receivers, so nothing is missing today — but a future
  requirement for OTLP-native metrics reopens this.
- **Ecosystem instrumentation libraries** for any Go component the platform might later embed. A
  library that expects a `go.opentelemetry.io/otel` `TracerProvider` will not find one.
- **Any other AWS service** without either writing more signing and protocol code or taking the
  SDK. Bedrock is the only AWS API on the request path, and adding a second is the point at which
  the SDK's cost/benefit changes.

Getting any of these back costs one adapter behind `telemetry.Exporter` plus the review-board
process for the modules involved. It is bounded work, deliberately.

## Revisit when

Any one of these is a signal, not a suggestion:

1. **The review-board constraint changes.** If the client establishes a pre-approved module
   allow-list that includes the OpenTelemetry and Prometheus Go libraries, driver 1 loses its
   weight and Option D becomes the obvious answer. Ask at each annual architecture review whether
   this has happened.
2. **A required capability is outside the implemented subset.** A concrete need for OTLP metrics,
   OTLP logs, span links, gRPC transport, or exemplars is the point at which the adapter gets
   written. Do not implement a sixth subset to avoid the conversation.
3. **A second AWS API appears on the request path.** One signed operation justifies a page of
   signing code. Two or more do not.
4. **A specification change breaks wire compatibility with the collector**, and keeping up costs
   more than a sprint per year. Maintenance load is the cost this decision accepted; when it stops
   being small, the decision stops being right.
5. **The bespoke components accumulate defects.** Two or more production incidents traced to
   `internal/telemetry`, `internal/yamlite`, or the AWS pieces is evidence that the in-house
   implementations are not as cheap to own as this ADR assumed. That is the strongest possible
   signal and it should supersede this ADR rather than amend it.
