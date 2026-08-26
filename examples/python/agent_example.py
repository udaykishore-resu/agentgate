#!/usr/bin/env python3
"""Calling the AgentGate gateway from Python with the stock OpenAI SDK.

===============================================================================
Why no SDK change is needed
===============================================================================

The AgentGate gateway is wire-compatible with the OpenAI Chat Completions API.
It speaks the same request and response JSON, the same `text/event-stream`
framing, and the same `data: [DONE]` terminator. That is a deliberate design
choice rather than a coincidence: every agent framework the client's teams use
already speaks that shape, so adopting it removes an SDK migration from the
critical path and lets the gateway be dropped in front of code that already
works.

The practical consequence is that adopting AgentGate is two lines:

    client = OpenAI(
        base_url="https://gateway.agentgate.internal/v1",
        api_key=agentgate_access_token,
    )

Everything the SDK does - streaming, tool calls, retries, typed responses -
continues to work. The `api_key` parameter carries the AgentGate access token;
the SDK sends it as `Authorization: Bearer <value>`, which is exactly what the
gateway wants. Nothing in the SDK needs patching, subclassing or monkeypatching.

What the SDK does *not* know about is the AgentGate-specific surface:

  * the `x-agentgate-*` response headers, reached through `.with_raw_response`;
  * the `agentgate.usage` and `error` named SSE events, which the SDK's
    streaming iterator skips because a strict OpenAI client ignores named
    frames;
  * `POST /v1/token-count`, and the control plane's `/oauth2/token`.

This file shows both halves: the SDK for the model calls, and plain `requests`
for the AgentGate-specific parts.

===============================================================================
Dependencies
===============================================================================

    pip install "openai>=1.0" requests

Only these two. Neither is required by the platform itself - AgentGate's own Go
services have no Python in them - they are what a consuming team is most likely
to already have.

===============================================================================
Running it
===============================================================================

    export AGENTGATE_GATEWAY_URL=http://localhost:8080
    export AGENTGATE_CONTROLPLANE_URL=http://localhost:8081
    export AGENTGATE_CLIENT_ID=cli_...
    export AGENTGATE_CLIENT_SECRET=ags_...
    export AGENTGATE_ENV=dev
    python3 agent_example.py

Or, on a runtime with federated workload identity (preferred - no secret ever
exists, and it is what the production promotion gate requires):

    export AGENTGATE_SUBJECT_TOKEN_FILE=/var/run/secrets/tokens/agentgate
    export AGENTGATE_AGENT_IDENTITY=agent://fsclient/payments-risk/dispute-triage
"""

from __future__ import annotations

import json
import os
import random
import secrets
import sys
import time
from dataclasses import dataclass, field
from typing import Any, Callable, Iterator, Mapping

import requests
from openai import OpenAI
from openai import APIStatusError

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

GATEWAY_URL = os.environ.get("AGENTGATE_GATEWAY_URL", "http://localhost:8080")
CONTROLPLANE_URL = os.environ.get("AGENTGATE_CONTROLPLANE_URL", "http://localhost:8081")
ENV = os.environ.get("AGENTGATE_ENV", "dev")
MODEL = os.environ.get("AGENTGATE_MODEL", "general-chat")
AGENT_VERSION = os.environ.get("AGENTGATE_AGENT_VERSION", "")

# The frozen v1 error codes this example reasons about. The complete set is in
# api/openapi/gateway.v1.yaml. Branch on the code, never on the title or the
# detail: the code is frozen, the prose is not.
CODE_INVALID_REQUEST = "invalid_request"
CODE_UNAUTHENTICATED = "unauthenticated"
CODE_FORBIDDEN_POOL = "forbidden_pool"
CODE_AGENT_NOT_PROMOTED = "agent_not_promoted"
CODE_GUARDRAIL_BLOCKED = "guardrail_blocked"
CODE_UNKNOWN_MODEL = "unknown_model"
CODE_CLIENT_TIMEOUT = "client_timeout"
CODE_CONTEXT_TOO_LARGE = "context_too_large"
CODE_RATE_LIMITED = "rate_limited"
CODE_QUOTA_EXCEEDED = "quota_exceeded"
CODE_PROVIDER_ERROR = "provider_error"
CODE_NO_HEALTHY_BACKEND = "no_healthy_backend"
CODE_PROVIDER_TIMEOUT = "provider_timeout"
CODE_INTERNAL_ERROR = "internal_error"

# Only these are worth another attempt. Retrying anything else spends the
# caller's deadline on a request that has already been definitively answered.
RETRYABLE_CODES = frozenset(
    {
        CODE_RATE_LIMITED,
        CODE_QUOTA_EXCEEDED,
        CODE_PROVIDER_ERROR,
        CODE_NO_HEALTHY_BACKEND,
        CODE_PROVIDER_TIMEOUT,
        CODE_INTERNAL_ERROR,
    }
)


# ---------------------------------------------------------------------------
# The AgentGate error document
# ---------------------------------------------------------------------------


@dataclass
class Problem(Exception):
    """An RFC 9457 problem document, as returned by every AgentGate failure."""

    code: str
    status: int
    title: str = ""
    detail: str = ""
    request_id: str = ""
    trace_id: str = ""
    retry_after_seconds: int = 0
    param: str = ""

    def __str__(self) -> str:
        base = f"{self.code} ({self.status}): {self.detail}"
        return f"{base} [param={self.param}]" if self.param else base

    @property
    def retryable(self) -> bool:
        return self.code in RETRYABLE_CODES

    @classmethod
    def from_body(cls, status: int, body: Any) -> "Problem":
        """Build a Problem from a response body, synthesising one if needed.

        A proxy that returns an HTML error page is not a contract violation the
        caller can do anything about, but it must not crash the client either.
        Falling back to the status class keeps every failure a Problem.
        """
        if isinstance(body, (bytes, str)):
            try:
                body = json.loads(body)
            except (ValueError, TypeError):
                body = None
        if isinstance(body, Mapping) and body.get("code"):
            return cls(
                code=str(body.get("code")),
                status=int(body.get("status") or status),
                title=str(body.get("title") or ""),
                detail=str(body.get("detail") or ""),
                request_id=str(body.get("request_id") or ""),
                trace_id=str(body.get("trace_id") or ""),
                retry_after_seconds=int(body.get("retry_after_seconds") or 0),
                param=str(body.get("param") or ""),
            )
        if status == 429:
            code = CODE_RATE_LIMITED
        elif status >= 500:
            code = CODE_PROVIDER_ERROR
        else:
            code = CODE_INVALID_REQUEST
        return cls(code=code, status=status, detail=str(body or ""))

    @classmethod
    def from_openai_error(cls, exc: APIStatusError) -> "Problem":
        """Extract the AgentGate Problem from an OpenAI SDK exception.

        The SDK raises its own exception type on a non-2xx response and keeps
        the parsed body on `.body`, so the problem document survives intact.
        """
        problem = cls.from_body(exc.status_code, getattr(exc, "body", None))
        # Retry-After on the response is authoritative when the body's hint is
        # absent: the gateway knows when the bucket refills and we do not.
        header = _header(getattr(exc, "response", None), "retry-after")
        if header and not problem.retry_after_seconds:
            try:
                problem.retry_after_seconds = int(header)
            except ValueError:
                pass
        if not problem.request_id:
            problem.request_id = _header(getattr(exc, "response", None), "x-agentgate-request-id")
        if not problem.trace_id:
            problem.trace_id = _header(getattr(exc, "response", None), "x-agentgate-trace-id")
        return problem


def _header(response: Any, name: str) -> str:
    try:
        return response.headers.get(name, "") or ""
    except AttributeError:
        return ""


# ---------------------------------------------------------------------------
# The x-agentgate-* response headers
# ---------------------------------------------------------------------------


@dataclass
class GatewayHeaders:
    """The AgentGate metadata carried on every response, success or failure.

    Reading it is half the value of routing through the gateway: it tells the
    application what a call actually cost and where it went, without waiting for
    a monthly report or opening a dashboard.
    """

    request_id: str = ""
    trace_id: str = ""
    provider: str = ""
    backend_model: str = ""
    pool: str = ""
    attempts: int = 0
    cache: str = ""
    tokens_input: int = 0
    tokens_output: int = 0
    cost_usd: float = 0.0
    ratelimit_limit: int = 0
    ratelimit_remaining: int = 0
    ratelimit_reset_seconds: int = 0
    guardrail: str = ""
    degraded: bool = False
    retry_after_seconds: int = 0

    @classmethod
    def from_headers(cls, headers: Mapping[str, str]) -> "GatewayHeaders":
        def get(name: str) -> str:
            return headers.get(name, "") or ""

        def as_int(name: str) -> int:
            try:
                return int(get(name))
            except ValueError:
                return 0

        def as_float(name: str) -> float:
            try:
                return float(get(name))
            except ValueError:
                return 0.0

        return cls(
            request_id=get("x-agentgate-request-id"),
            trace_id=get("x-agentgate-trace-id"),
            provider=get("x-agentgate-provider"),
            backend_model=get("x-agentgate-model"),
            pool=get("x-agentgate-pool"),
            attempts=as_int("x-agentgate-attempts"),
            cache=get("x-agentgate-cache"),
            tokens_input=as_int("x-agentgate-tokens-input"),
            tokens_output=as_int("x-agentgate-tokens-output"),
            cost_usd=as_float("x-agentgate-cost-usd"),
            ratelimit_limit=as_int("x-agentgate-ratelimit-limit-tokens"),
            ratelimit_remaining=as_int("x-agentgate-ratelimit-remaining-tokens"),
            ratelimit_reset_seconds=as_int("x-agentgate-ratelimit-reset"),
            guardrail=get("x-agentgate-guardrail"),
            degraded=get("x-agentgate-degraded") == "true",
            retry_after_seconds=as_int("retry-after"),
        )

    def __str__(self) -> str:
        parts = [
            f"request={self.request_id}",
            f"trace={self.trace_id}",
            f"provider={self.provider}",
            f"backend={self.backend_model}",
            f"pool={self.pool}",
            f"attempts={self.attempts}",
            f"cache={self.cache}",
            f"in={self.tokens_input}",
            f"out={self.tokens_output}",
            f"cost=${self.cost_usd:.6f}",
        ]
        if self.ratelimit_limit:
            parts.append(
                f"quota={self.ratelimit_remaining}/{self.ratelimit_limit}"
                f" reset={self.ratelimit_reset_seconds}s"
            )
        if self.guardrail:
            parts.append(f"guardrail={self.guardrail}")
        if self.degraded:
            # Not an error. The platform is saying a dependency was degraded
            # for this call, so the application can decide whether to retry or
            # fall back instead of guessing.
            parts.append("DEGRADED")
        return " ".join(parts)


# ---------------------------------------------------------------------------
# W3C trace context
# ---------------------------------------------------------------------------


@dataclass
class TraceContext:
    """A W3C traceparent.

    Propagating it makes the agent's own work and the gateway's work it caused
    one trace instead of two unrelated ones - the difference between "the call
    was slow" and "the call was slow in the guardrail stage of the third of five
    steps".

    A real agent takes this from its OpenTelemetry SDK. The shape is identical;
    this is here so the example has no OTel dependency.
    """

    trace_id: str = field(default_factory=lambda: secrets.token_hex(16))
    span_id: str = field(default_factory=lambda: secrets.token_hex(8))
    sampled: bool = True

    def child(self) -> "TraceContext":
        return TraceContext(self.trace_id, secrets.token_hex(8), self.sampled)

    def header(self) -> str:
        return f"00-{self.trace_id}-{self.span_id}-{'01' if self.sampled else '00'}"


# ---------------------------------------------------------------------------
# Token acquisition
# ---------------------------------------------------------------------------


def fetch_token_exchange(
    subject_token: str, agent_identity: str, env: str = ENV, agent_version: str = ""
) -> dict[str, Any]:
    """Obtain an access token by RFC 8693 token exchange.

    The preferred path. The runtime has already proved who the workload is - a
    Kubernetes projected service account token, an Azure managed identity token,
    a SPIFFE JWT-SVID - and the control plane converts that proof into an
    AgentGate identity. No shared secret ever exists, and the resulting token
    carries attestation=workload-identity, which the production promotion gate
    requires.
    """
    form = {
        "grant_type": "urn:ietf:params:oauth:grant-type:token-exchange",
        "subject_token": subject_token,
        "subject_token_type": "urn:ietf:params:oauth:token-type:jwt",
        "agent_identity": agent_identity,
        "env": env,
    }
    if agent_version:
        form["agent_version"] = agent_version
    return _post_token(form)


def fetch_token_client_credentials(
    client_id: str, client_secret: str, env: str = ENV, agent_version: str = ""
) -> dict[str, Any]:
    """Obtain an access token with a client secret.

    The fallback, for runtimes that cannot do federated workload identity. The
    resulting token carries attestation=client-secret, which the production
    promotion gate refuses - an agent on this path reaches staging, not
    production.
    """
    form = {
        "grant_type": "client_credentials",
        "client_id": client_id,
        "client_secret": client_secret,
        "env": env,
    }
    if agent_version:
        form["agent_version"] = agent_version
    return _post_token(form)


def _post_token(form: dict[str, str]) -> dict[str, Any]:
    # The token endpoint is form-encoded and returns OAuth2-shaped errors
    # ({"error", "error_description"}), not problem+json: it speaks the dialect
    # an OAuth2 client library expects.
    response = requests.post(
        CONTROLPLANE_URL.rstrip("/") + "/oauth2/token",
        data=form,
        headers={"Content-Type": "application/x-www-form-urlencoded"},
        timeout=30,
    )
    if response.status_code != 200:
        try:
            body = response.json()
            raise RuntimeError(
                f"token request refused: {body.get('error')}: {body.get('error_description')}"
            )
        except ValueError:
            raise RuntimeError(
                f"token request refused: {response.status_code}: {response.text.strip()}"
            ) from None
    payload = response.json()
    if not payload.get("access_token"):
        raise RuntimeError("token endpoint returned an empty access token")
    return payload


def obtain_token() -> dict[str, Any]:
    """Pick a grant from what the environment provides, preferring workload identity."""
    subject_token_file = os.environ.get("AGENTGATE_SUBJECT_TOKEN_FILE", "")
    if subject_token_file:
        agent_identity = os.environ.get("AGENTGATE_AGENT_IDENTITY", "")
        if not agent_identity:
            raise RuntimeError(
                "AGENTGATE_AGENT_IDENTITY is required alongside AGENTGATE_SUBJECT_TOKEN_FILE"
            )
        # The projected token is refreshed in place by the kubelet, so it is
        # read at each use and never cached.
        with open(subject_token_file, "r", encoding="utf-8") as handle:
            subject_token = handle.read().strip()
        print("obtaining a token by RFC 8693 token exchange (federated workload identity)")
        return fetch_token_exchange(subject_token, agent_identity, ENV, AGENT_VERSION)

    client_id = os.environ.get("AGENTGATE_CLIENT_ID", "")
    client_secret = os.environ.get("AGENTGATE_CLIENT_SECRET", "")
    if not client_id or not client_secret:
        raise RuntimeError(
            "set AGENTGATE_SUBJECT_TOKEN_FILE and AGENTGATE_AGENT_IDENTITY, "
            "or AGENTGATE_CLIENT_ID and AGENTGATE_CLIENT_SECRET"
        )
    print("obtaining a token by client credentials (prefer workload identity where available)")
    return fetch_token_client_credentials(client_id, client_secret, ENV, AGENT_VERSION)


# ---------------------------------------------------------------------------
# Retry policy
# ---------------------------------------------------------------------------


def retry_delay(attempt: int, problem: Problem) -> float:
    """How long to wait before another attempt, in seconds.

    Retry-After is authoritative when the platform sends it: the gateway knows
    when the bucket refills and the caller does not. Only when it is absent do
    we fall back to exponential backoff with *full* jitter - full, not equal,
    because a fleet of agents backing off in lockstep reproduces the thundering
    herd the backoff was meant to prevent.

    Ignoring Retry-After does not get you served sooner. The fleet-wide retry
    budget is capped at 10% of request volume and will shed the excess.
    """
    if problem.retry_after_seconds > 0:
        return float(problem.retry_after_seconds)
    ceiling = min(0.25 * (2**attempt), 20.0)
    return random.uniform(0.0, ceiling)


def call_with_retry(
    operation: Callable[[], Any], *, max_attempts: int = 3, label: str = "call"
) -> Any:
    """Run an operation, retrying only what the contract says is transient."""
    last: Problem | None = None
    for attempt in range(max_attempts):
        if attempt > 0 and last is not None:
            delay = retry_delay(attempt - 1, last)
            print(
                f"  retrying {label} after {delay:.2f}s "
                f"(attempt {attempt + 1}/{max_attempts}, last={last.code})",
                file=sys.stderr,
            )
            time.sleep(delay)
        try:
            return operation()
        except APIStatusError as exc:
            problem = Problem.from_openai_error(exc)
            if not problem.retryable:
                # Permanent. Returning immediately is not giving up early; it is
                # declining to spend the deadline on a request that has already
                # been definitively answered.
                raise problem from None
            last = problem
        except Problem as problem:
            if not problem.retryable:
                raise
            last = problem
    assert last is not None
    raise last


# ---------------------------------------------------------------------------
# Unary and streaming calls through the OpenAI SDK
# ---------------------------------------------------------------------------


def unary_call(client: OpenAI, trace: TraceContext, session_id: str) -> None:
    """A unary completion, reading the AgentGate response headers.

    `.with_raw_response` is the SDK's supported way to reach the HTTP response
    alongside the parsed body. Without it the x-agentgate-* headers - cost,
    provider, cache result, quota headroom - are simply discarded, which is the
    most common way a team ends up unable to explain its own model spend.
    """
    print("=== unary ===")

    def do_call() -> Any:
        return client.chat.completions.with_raw_response.create(
            model=MODEL,
            messages=[
                {
                    "role": "system",
                    "content": "You are a dispute triage assistant. Answer in two sentences.",
                },
                {
                    "role": "user",
                    "content": "Cardholder disputes a 42.10 GBP contactless transaction from 3 March.",
                },
            ],
            max_tokens=256,
            temperature=0.2,
            # Extra headers the SDK passes through untouched. `metadata` in the
            # body would work too; the header takes precedence for session id.
            extra_headers={
                "traceparent": trace.child().header(),
                "x-agentgate-session-id": session_id,
                "x-agentgate-request-priority": "interactive",
            },
            # Caller context joined onto the trace. The gateway strips it before
            # the request reaches a provider, so it costs no tokens and leaks no
            # internal identifiers.
            extra_body={
                "metadata": {
                    "session_id": session_id,
                    "step": "classify",
                    "tags": ["dispute", "tier2"],
                }
            },
        )

    try:
        raw = call_with_retry(do_call, label="chat.completions")
    except Problem as problem:
        report_failure(problem)
        return

    headers = GatewayHeaders.from_headers(raw.headers)
    print(f"headers: {headers}")

    completion = raw.parse()
    if completion.choices:
        print(f"answer:  {completion.choices[0].message.content}")
    if completion.usage:
        print(
            f"usage:   in={completion.usage.prompt_tokens} "
            f"out={completion.usage.completion_tokens} "
            f"total={completion.usage.total_tokens}"
        )
    print()


def streaming_call_via_sdk(client: OpenAI, trace: TraceContext, session_id: str) -> None:
    """A streaming completion through the SDK.

    The SDK handles the SSE framing, the heartbeat comments and the [DONE]
    sentinel. What it does not surface is the `agentgate.usage` and `error`
    named frames: it skips named events, because a strict OpenAI client is
    supposed to. See `streaming_call_raw` for those.

    A streaming call is NOT retried here. Before the first content byte the
    gateway retries and fails over on the caller's behalf; after it, re-issuing
    the request would bill for and regenerate a completion the caller has
    already partly consumed.
    """
    print("=== streaming (SDK) ===")
    try:
        stream = client.chat.completions.create(
            model=MODEL,
            messages=[
                {
                    "role": "user",
                    "content": "Summarise the common chargeback reason codes for card-present fraud.",
                }
            ],
            max_tokens=512,
            temperature=0.2,
            stream=True,
            stream_options={"include_usage": True},
            extra_headers={
                "traceparent": trace.child().header(),
                "x-agentgate-session-id": session_id,
            },
        )
        for chunk in stream:
            if chunk.choices and chunk.choices[0].delta.content:
                print(chunk.choices[0].delta.content, end="", flush=True)
        print("\n")
    except APIStatusError as exc:
        # A failure BEFORE the first byte still has a status line and a problem
        # body, so it arrives as a normal SDK exception.
        report_failure(Problem.from_openai_error(exc))
    except Exception as exc:  # noqa: BLE001 - the SDK raises several types mid-stream
        # A failure AFTER the first byte cannot change the status line, so it
        # surfaces as a transport error here. Whatever was printed above is an
        # INCOMPLETE answer and must not be presented as the answer.
        print(f"\nstream failed mid-flight: {exc}", file=sys.stderr)
        print("the partial output above is NOT the answer", file=sys.stderr)


def streaming_call_raw(token: str, trace: TraceContext, session_id: str) -> None:
    """The same stream read directly, so the AgentGate frames are visible.

    Use this shape when the application needs per-call cost, the backend that
    served it, or a mid-stream guardrail block - none of which reach the SDK's
    iterator.

    The rule that matters: **a stream that ends without `data: [DONE]` failed.**
    Once the first content byte is written the HTTP status is long gone and
    cannot be changed, so the contract carries the failure in band as an `error`
    frame. A client that treats a truncated stream as a short answer silently
    shows a user half a response, which in a regulated context is worse than
    showing them an error.
    """
    print("=== streaming (raw, showing AgentGate frames) ===")
    body = {
        "model": MODEL,
        "messages": [{"role": "user", "content": "Name three PSD2 strong-authentication exemptions."}],
        "max_tokens": 256,
        "temperature": 0.2,
        "stream": True,
        "stream_options": {"include_usage": True},
    }
    response = requests.post(
        GATEWAY_URL.rstrip("/") + "/v1/chat/completions",
        json=body,
        headers={
            "Authorization": f"Bearer {token}",
            "Accept": "text/event-stream",
            "traceparent": trace.child().header(),
            "x-agentgate-session-id": session_id,
        },
        stream=True,
        timeout=(10, 600),
    )
    headers = GatewayHeaders.from_headers(response.headers)

    if response.status_code != 200:
        report_failure(Problem.from_body(response.status_code, response.content))
        return

    completed = False
    received = 0
    for event, data in _iter_sse(response):
        if data.strip() == "[DONE]":
            completed = True
            break
        if event == "":
            try:
                chunk = json.loads(data)
            except ValueError:
                continue  # an unparseable frame is ignorable, not fatal
            choices = chunk.get("choices") or []
            if choices:
                text = (choices[0].get("delta") or {}).get("content") or ""
                if text:
                    received += len(text)
                    print(text, end="", flush=True)
        elif event == "agentgate.usage":
            usage = json.loads(data)
            print(
                f"\n\nusage frame: backend={usage.get('backend')} "
                f"attempts={usage.get('attempts')} "
                f"cost=${usage.get('cost_usd', 0):.6f} "
                f"estimated={usage.get('estimated')} "
                f"stalls={usage.get('stalls')}"
            )
        elif event == "error":
            problem = Problem.from_body(0, data)
            print(f"\n\nstream error frame: {problem}", file=sys.stderr)
            report_failure(problem)
        # An unknown named event is ignorable by design: the contract adds
        # frames, and a strict client must not choke on one it has not met.

    print(f"\nheaders: {headers}")
    if not completed:
        print(
            f"stream ended without [DONE] after {received} characters; "
            "the partial answer is NOT the answer",
            file=sys.stderr,
        )
    print()


def _iter_sse(response: requests.Response) -> Iterator[tuple[str, str]]:
    """Decode an SSE stream into (event_name, data) pairs.

    Lines beginning with `:` are comments. The gateway sends `: heartbeat` every
    15 seconds so that idle enterprise proxies do not drop a long generation;
    receiving one is evidence the connection is alive and the model is still
    thinking, not that anything is wrong.
    """
    event = ""
    data: list[str] = []
    for raw_line in response.iter_lines(decode_unicode=True):
        line = raw_line if raw_line is not None else ""
        if line == "":
            if data or event:
                yield event, "\n".join(data)
                event, data = "", []
            continue
        if line.startswith(":"):
            continue
        if line.startswith("event:"):
            event = line[len("event:") :].strip()
        elif line.startswith("data:"):
            chunk = line[len("data:") :]
            data.append(chunk[1:] if chunk.startswith(" ") else chunk)
    if data or event:
        yield event, "\n".join(data)


# ---------------------------------------------------------------------------
# The AgentGate-specific endpoints
# ---------------------------------------------------------------------------


def token_count(token: str, trace: TraceContext) -> None:
    """Pre-flight estimate. Cheaper than paying for a `context_too_large` refusal."""
    print("=== token-count ===")
    response = requests.post(
        GATEWAY_URL.rstrip("/") + "/v1/token-count",
        json={
            "model": MODEL,
            "messages": [{"role": "user", "content": "How much context does this consume?"}],
            "max_tokens": 2048,
        },
        headers={
            "Authorization": f"Bearer {token}",
            "traceparent": trace.child().header(),
        },
        timeout=30,
    )
    if response.status_code != 200:
        report_failure(Problem.from_body(response.status_code, response.content))
        return
    payload = response.json()
    print(
        f"model={payload['model']} "
        f"estimated_input={payload['estimated_input_tokens']} "
        f"window={payload['context_window']} "
        f"max_output={payload['max_output_tokens']}"
    )
    print(
        f"monthly budget: {payload['monthly_tokens_used']}/{payload['monthly_token_budget']} used, "
        f"{payload['tokens_per_minute_limit']} tokens/minute limit"
    )
    print()


def list_models(client: OpenAI) -> None:
    """The models this token may actually use.

    A model that exists on the platform but that this agent has not been granted
    is absent from the list rather than forbidden. When a call fails with
    unknown_model, this is where to look.
    """
    print("=== models ===")
    for model in client.models.list():
        print(f"  {model.id}")
    print()


# ---------------------------------------------------------------------------
# Error handling
# ---------------------------------------------------------------------------


def report_failure(problem: Problem) -> None:
    """Print the response the contract attaches to each error code.

    A real agent branches here rather than printing. The point is that each of
    these has a different correct response, and treating them uniformly as "the
    call failed" throws that away.
    """
    print(f"failed: {problem}", file=sys.stderr)
    if problem.request_id:
        print(
            f"  quote request_id={problem.request_id} trace_id={problem.trace_id} in any ticket",
            file=sys.stderr,
        )

    guidance = {
        CODE_INVALID_REQUEST: (
            "permanent. Fix the request; retrying it unchanged fails identically."
        ),
        CODE_UNAUTHENTICATED: (
            "obtain a fresh token and retry once. A second failure with a fresh token is "
            "configuration, not transience."
        ),
        CODE_FORBIDDEN_POOL: (
            "this token is not entitled to that pool, or lacks the scope. Ask the platform "
            "team for a grant."
        ),
        CODE_AGENT_NOT_PROMOTED: (
            "this token's environment does not match this gateway. Promote the version, or "
            "call the right gateway."
        ),
        CODE_GUARDRAIL_BLOCKED: (
            "content safety refused. Do NOT retry the same content. If this is a false "
            "positive, raise it with the guardrail owner - the x-agentgate-guardrail header "
            "names the category that fired."
        ),
        CODE_UNKNOWN_MODEL: "call GET /v1/models for the list this token may actually use.",
        CODE_CONTEXT_TOO_LARGE: (
            "trim the context or lower max_tokens. POST /v1/token-count answers this before "
            "a request is spent."
        ),
        CODE_CLIENT_TIMEOUT: (
            "the deadline expired. Retry with a longer deadline or a smaller request, not "
            "with the same one."
        ),
        CODE_NO_HEALTHY_BACKEND: (
            "every backend in the pool is unavailable. This is a platform failure and has "
            "already paged someone."
        ),
        CODE_PROVIDER_ERROR: (
            "upstream failure after the gateway's own retries and failover. Back off, then retry."
        ),
        CODE_PROVIDER_TIMEOUT: (
            "upstream failure after the gateway's own retries and failover. Back off, then retry."
        ),
        CODE_INTERNAL_ERROR: (
            "retry once with backoff. If it persists, open a ticket quoting the request id."
        ),
    }

    if problem.code in (CODE_RATE_LIMITED, CODE_QUOTA_EXCEEDED):
        print(
            f"  honour Retry-After ({problem.retry_after_seconds}s). If the monthly budget is "
            "exhausted, backoff will not help; the quota must change.",
            file=sys.stderr,
        )
    elif problem.code in guidance:
        print(f"  {guidance[problem.code]}", file=sys.stderr)
        if problem.code == CODE_INVALID_REQUEST and problem.param:
            print(f"  the offending field is {problem.param!r}", file=sys.stderr)
    else:
        # An unrecognised code is not a client error. The contract permits new
        # codes; fall back to the status class.
        if problem.status >= 500 or problem.status == 429:
            print("  unrecognised code, transient status class: back off and retry.", file=sys.stderr)
        else:
            print("  unrecognised code, permanent status class: do not retry.", file=sys.stderr)


# ---------------------------------------------------------------------------
# Walkthrough
# ---------------------------------------------------------------------------


def main() -> int:
    try:
        token_response = obtain_token()
    except (RuntimeError, OSError) as exc:
        print(f"could not obtain an access token: {exc}", file=sys.stderr)
        return 1

    print(
        f"token: agent_id={token_response['agent_id']} "
        f"env={token_response['env']} "
        f"version={token_response['agent_version']} "
        f"expires_in={token_response['expires_in']}s "
        f"scope={token_response['scope']!r}\n"
    )

    token = token_response["access_token"]

    # The two lines that make an existing OpenAI-SDK agent an AgentGate agent.
    #
    # max_retries=0 turns off the SDK's own retry logic. That is deliberate: the
    # SDK does not know which AgentGate codes are permanent, and it will happily
    # retry a guardrail block. Retry policy belongs where the error codes are
    # understood - in call_with_retry above.
    client = OpenAI(
        base_url=GATEWAY_URL.rstrip("/") + "/v1",
        api_key=token,
        max_retries=0,
        timeout=600.0,
    )

    # A long-running agent must refresh the token: lifetimes are short (15
    # minutes by default) so that a quarantine takes effect quickly. Refresh at
    # roughly half the lifetime, not at expiry - a token that expires mid-request
    # produces a 401 the caller cannot distinguish from a real authentication
    # failure.

    trace = TraceContext()
    session_id = "sess_" + secrets.token_hex(8)
    print(f"trace: {trace.trace_id} (session {session_id})\n")

    list_models(client)
    unary_call(client, trace, session_id)
    streaming_call_via_sdk(client, trace, session_id)
    streaming_call_raw(token, trace, session_id)
    token_count(token, trace)
    return 0


if __name__ == "__main__":
    sys.exit(main())
