// AgentGate load-test shared helpers.
//
// Everything every scenario needs: configuration, token minting against the control
// plane, request builders, response-header assertions against SPEC 2.3, an SSE body
// parser, and the custom metrics the thresholds are expressed on.
//
// Nothing in here is scenario-specific. If a helper only one script uses, it lives in
// that script.

import http from 'k6/http';
import { check, fail } from 'k6';
import { Trend, Counter, Rate, Gauge } from 'k6/metrics';
import encoding from 'k6/encoding';

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

export const CFG = {
  gateway: __ENV.AGENTGATE_GATEWAY || 'https://gateway.agentgate.internal',
  controlplane: __ENV.AGENTGATE_CONTROLPLANE || 'https://controlplane.agentgate.internal',

  // Token acquisition. Two modes:
  //   client_credentials - exchange a load-test client id/secret for an access token
  //   static             - use AGENTGATE_TOKEN as-is (short runs, smoke, local)
  authMode: __ENV.AGENTGATE_AUTH_MODE || 'client_credentials',
  staticToken: __ENV.AGENTGATE_TOKEN || '',
  clientId: __ENV.AGENTGATE_CLIENT_ID || '',
  clientSecret: __ENV.AGENTGATE_CLIENT_SECRET || '',

  tenant: __ENV.AGENTGATE_TENANT || 'fsclient',
  team: __ENV.AGENTGATE_TEAM || 'payments-risk',
  agent: __ENV.AGENTGATE_AGENT || 'loadgen',
  env: __ENV.AGENTGATE_ENV || 'staging',

  pool: __ENV.AGENTGATE_POOL || 'general-chat',
  model: __ENV.AGENTGATE_MODEL || 'general-chat',
  embedModel: __ENV.AGENTGATE_EMBED_MODEL || 'general-embed',

  // Mix and shape
  streamRatio: parseFloat(__ENV.AGENTGATE_STREAM_RATIO || '0.6'),
  maxTokens: parseInt(__ENV.AGENTGATE_MAX_TOKENS || '256', 10),
  promptTokens: parseInt(__ENV.AGENTGATE_PROMPT_TOKENS || '600', 10),

  // Cache behaviour under test. 'off' keeps every request a real backend call, which
  // is what you want when measuring capacity. 'on' measures the production mix.
  cache: __ENV.AGENTGATE_CACHE || 'off',
  priority: __ENV.AGENTGATE_PRIORITY || 'interactive',

  requestTimeout: __ENV.AGENTGATE_REQUEST_TIMEOUT || '120s',
  insecureSkipTLS: (__ENV.AGENTGATE_INSECURE || 'false') === 'true',
};

// SLO targets from SPEC section 5, in milliseconds. Thresholds in the scenario files
// reference these so a change to the SLO changes every test at once.
export const SLO = {
  gatewayOverheadP95Ms: 60,
  ttftP95Ms: 1200,
  availability: 0.999,
  controlPlaneExchangeP95Ms: 150,
  telemetryCompleteness: 0.98,
};

// ---------------------------------------------------------------------------
// Custom metrics
// ---------------------------------------------------------------------------

// Latency
export const ttft = new Trend('agentgate_ttft_ms', true);
export const gatewayOverhead = new Trend('agentgate_gateway_overhead_ms', true);
export const providerTime = new Trend('agentgate_provider_time_ms', true);
export const interTokenMean = new Trend('agentgate_intertoken_mean_ms', true);
export const streamDuration = new Trend('agentgate_stream_duration_ms', true);
export const unaryDuration = new Trend('agentgate_unary_duration_ms', true);
export const tokenMintDuration = new Trend('agentgate_token_mint_ms', true);

// Correctness and contract
export const contractOk = new Rate('agentgate_contract_headers_ok');
export const streamContractOk = new Rate('agentgate_stream_contract_ok');
export const usageFrameSeen = new Rate('agentgate_usage_frame_seen');
export const heartbeatSeen = new Rate('agentgate_heartbeat_seen');
export const doneTerminatorSeen = new Rate('agentgate_done_terminator_seen');

// Outcomes
export const successRate = new Rate('agentgate_success_rate');
export const availabilityRate = new Rate('agentgate_availability_rate'); // non-5xx, non no_healthy_backend
export const errorsByCode = new Counter('agentgate_errors_by_code');
export const rateLimited = new Counter('agentgate_rate_limited');
export const quotaExceeded = new Counter('agentgate_quota_exceeded');
export const noHealthyBackend = new Counter('agentgate_no_healthy_backend');
export const unexpected5xx = new Counter('agentgate_unexpected_5xx');
export const guardrailBlocked = new Counter('agentgate_guardrail_blocked');

// Routing and economics
export const attempts = new Trend('agentgate_attempts');
export const failoverRate = new Rate('agentgate_failover_rate');
export const cacheHitRate = new Rate('agentgate_cache_hit_rate');
export const costUsd = new Trend('agentgate_cost_usd', true);
export const tokensIn = new Trend('agentgate_tokens_input');
export const tokensOut = new Trend('agentgate_tokens_output');
export const ratelimitRemaining = new Gauge('agentgate_ratelimit_remaining_tokens');

// ---------------------------------------------------------------------------
// Token minting
// ---------------------------------------------------------------------------

// Per-VU token cache. Each k6 VU has its own JS runtime, so this is VU-local by
// construction, which is what we want: a 4h soak must refresh tokens mid-run.
const tokenCache = {};

function decodeJwtExpiry(token) {
  try {
    const payload = token.split('.')[1];
    // base64url -> base64
    const b64 = payload.replace(/-/g, '+').replace(/_/g, '/');
    const claims = JSON.parse(encoding.b64decode(b64, 'std', 's'));
    return (claims.exp || 0) * 1000;
  } catch (e) {
    // Unparseable token: treat as short-lived so we re-mint rather than fail.
    return Date.now() + 60000;
  }
}

/**
 * Mint (or return a cached) AgentGate access token.
 *
 * identityKey lets multi-tenant scenarios hold several distinct agent identities in
 * one VU. Pass the agent identity or any stable string; it only keys the cache.
 */
export function getToken(identityKey, overrides) {
  const key = identityKey || 'default';
  const cached = tokenCache[key];
  // Refresh 60s before expiry so a long request never runs on an expiring token.
  if (cached && cached.expiresAt - 60000 > Date.now()) {
    return cached.token;
  }

  if (CFG.authMode === 'static') {
    if (!CFG.staticToken) {
      fail('AGENTGATE_AUTH_MODE=static requires AGENTGATE_TOKEN');
    }
    tokenCache[key] = { token: CFG.staticToken, expiresAt: decodeJwtExpiry(CFG.staticToken) };
    return CFG.staticToken;
  }

  const o = overrides || {};
  const clientId = o.clientId || CFG.clientId;
  const clientSecret = o.clientSecret || CFG.clientSecret;
  if (!clientId || !clientSecret) {
    fail('client_credentials mode requires AGENTGATE_CLIENT_ID and AGENTGATE_CLIENT_SECRET');
  }

  const body = {
    grant_type: 'client_credentials',
    client_id: clientId,
    client_secret: clientSecret,
    audience: `${CFG.gateway}`,
    scope: 'models:invoke models:embed',
  };

  const res = http.post(`${CFG.controlplane}/oauth2/token`, body, {
    headers: { 'content-type': 'application/x-www-form-urlencoded' },
    tags: { name: 'controlplane_token' },
    timeout: '15s',
  });

  tokenMintDuration.add(res.timings.duration);

  const ok = check(res, {
    'token exchange returned 200': (r) => r.status === 200,
    'token exchange returned an access_token': (r) => {
      try {
        return !!r.json('access_token');
      } catch (e) {
        return false;
      }
    },
    // SPEC section 5: control plane p95 < 150ms. Recorded per call so the trend is
    // available even when this check passes.
    'token exchange under 1s': (r) => r.timings.duration < 1000,
  });

  if (!ok) {
    fail(`token exchange failed: status=${res.status} body=${String(res.body).slice(0, 200)}`);
  }

  const token = res.json('access_token');
  tokenCache[key] = { token: token, expiresAt: decodeJwtExpiry(token) };
  return token;
}

// ---------------------------------------------------------------------------
// Request building
// ---------------------------------------------------------------------------

let seq = 0;
function nextId(prefix) {
  seq += 1;
  return `${prefix}_${__VU}_${__ITER}_${seq}`;
}

const FILLER =
  'The counterparty submitted a dispute regarding a card-not-present transaction ' +
  'processed through the acquiring bank on the settlement date. Supporting evidence ' +
  'includes the authorisation record, the merchant descriptor and the chargeback code. ';

/**
 * Build a prompt of approximately the requested token count, using the 4-chars-per-token
 * heuristic the gateway itself uses for estimation (SPEC 3.4).
 */
export function promptOfTokens(approxTokens) {
  const targetChars = approxTokens * 4;
  let s = '';
  while (s.length < targetChars) {
    s += FILLER;
  }
  return s.slice(0, targetChars);
}

/**
 * Standard request headers. Every optional x-agentgate-* header from SPEC 2.2 that a
 * load test can meaningfully set is set here.
 */
export function headers(token, opts) {
  const o = opts || {};
  const h = {
    Authorization: `Bearer ${token}`,
    'content-type': 'application/json',
    'x-agentgate-session-id': o.sessionId || nextId('sess'),
    'x-agentgate-request-priority': o.priority || CFG.priority,
    'x-agentgate-cache': o.cache || CFG.cache,
  };
  if (o.pool) {
    h['x-agentgate-pool'] = o.pool;
  }
  if (o.idempotencyKey) {
    h['x-agentgate-idempotency-key'] = o.idempotencyKey;
  }
  if (o.stream) {
    h['accept'] = 'text/event-stream';
  }
  return h;
}

export function chatBody(opts) {
  const o = opts || {};
  const stream = !!o.stream;
  const body = {
    model: o.model || CFG.model,
    messages: [
      { role: 'system', content: 'You are a dispute triage assistant. Answer concisely.' },
      { role: 'user', content: o.prompt || promptOfTokens(o.promptTokens || CFG.promptTokens) },
    ],
    max_tokens: o.maxTokens || CFG.maxTokens,
    temperature: o.temperature === undefined ? 0.2 : o.temperature,
    stream: stream,
    metadata: {
      session_id: o.sessionId || 'loadtest',
      conversation_id: o.conversationId || 'loadtest',
      step: o.step || 'classify',
      tags: ['loadtest', o.scenario || 'unspecified'],
    },
  };
  if (stream) {
    body.stream_options = { include_usage: true };
  }
  return JSON.stringify(body);
}

export function embeddingsBody(opts) {
  const o = opts || {};
  return JSON.stringify({
    model: o.model || CFG.embedModel,
    input: o.input || promptOfTokens(o.promptTokens || 200),
  });
}

export function requestParams(opts) {
  const o = opts || {};
  return {
    headers: headers(o.token, o),
    tags: Object.assign({ name: o.name || 'chat_completions' }, o.tags || {}),
    timeout: o.timeout || CFG.requestTimeout,
    // Streaming responses must not be redirected or transparently retried by the client;
    // the gateway owns retry semantics.
    redirects: 0,
  };
}

// ---------------------------------------------------------------------------
// Response inspection
// ---------------------------------------------------------------------------

function headerOf(res, name) {
  // k6 normalises header names to canonical form, but be defensive: providers and
  // proxies in the path have been known to lowercase them.
  return res.headers[name] || res.headers[name.toLowerCase()] || '';
}

/**
 * Assert the response header contract from SPEC 2.3. These headers are present on
 * EVERY response, success or error - that is the part most worth testing, because it
 * is the part most likely to regress on an error path nobody exercises.
 */
export function checkContractHeaders(res, opts) {
  const o = opts || {};
  const required = [
    'x-agentgate-request-id',
    'x-agentgate-trace-id',
    'x-agentgate-pool',
    'x-agentgate-attempts',
  ];
  // Provider, model, tokens and cost are absent when no backend was reached at all
  // (401, 403, 429 before routing). Only require them once routing happened.
  const routed = res.status < 400 || res.status >= 500;
  if (routed) {
    required.push('x-agentgate-provider', 'x-agentgate-model', 'x-agentgate-cache');
  }

  const missing = required.filter((h) => headerOf(res, h) === '');
  const ok = missing.length === 0;

  contractOk.add(ok, { status: String(res.status) });
  check(res, {
    'contract: required x-agentgate headers present': () => ok,
    'contract: trace id is 32 hex chars': () => {
      const t = headerOf(res, 'x-agentgate-trace-id');
      return t === '' ? !routed : /^[0-9a-f]{32}$/.test(t);
    },
    'contract: attempts is a positive integer': () => {
      const a = headerOf(res, 'x-agentgate-attempts');
      return a === '' ? !routed : /^[1-9][0-9]*$/.test(a);
    },
    'contract: cache header is a known value': () => {
      const c = headerOf(res, 'x-agentgate-cache');
      return c === '' || ['hit', 'miss', 'bypass', 'refresh'].indexOf(c) !== -1;
    },
    'contract: guardrail header is a known shape': () => {
      const g = headerOf(res, 'x-agentgate-guardrail');
      return g === '' || g === 'pass' || /^(blocked|redacted):/.test(g);
    },
  });

  if (!ok && !o.quiet) {
    console.warn(`missing contract headers [${missing.join(', ')}] on status ${res.status}`);
  }
  return ok;
}

/**
 * Record every metric we can derive from one response, and classify the outcome.
 * Returns a small summary object so callers can branch without re-parsing.
 */
export function recordResponse(res, opts) {
  const o = opts || {};
  const code = errorCode(res);
  const status = res.status;

  const isSuccess = status >= 200 && status < 300;
  const isNoHealthy = code === 'no_healthy_backend';
  const is5xx = status >= 500;

  successRate.add(isSuccess);
  // SPEC section 5: availability SLI is non-5xx AND non-no_healthy_backend over total.
  availabilityRate.add(!is5xx && !isNoHealthy);

  if (!isSuccess) {
    errorsByCode.add(1, { code: code || `http_${status}` });
  }
  if (status === 429 && code === 'rate_limited') rateLimited.add(1);
  if (status === 429 && code === 'quota_exceeded') quotaExceeded.add(1);
  if (isNoHealthy) noHealthyBackend.add(1);
  if (code === 'guardrail_blocked') guardrailBlocked.add(1);
  if (is5xx && !isNoHealthy) unexpected5xx.add(1, { code: code || `http_${status}` });

  const at = parseInt(headerOf(res, 'x-agentgate-attempts') || '0', 10);
  if (at > 0) {
    attempts.add(at);
    failoverRate.add(at > 1);
  }

  const cache = headerOf(res, 'x-agentgate-cache');
  if (cache) {
    cacheHitRate.add(cache === 'hit');
  }

  const cost = parseFloat(headerOf(res, 'x-agentgate-cost-usd') || '0');
  if (cost > 0) costUsd.add(cost);

  const ti = parseInt(headerOf(res, 'x-agentgate-tokens-input') || '0', 10);
  const to = parseInt(headerOf(res, 'x-agentgate-tokens-output') || '0', 10);
  if (ti > 0) tokensIn.add(ti);
  if (to > 0) tokensOut.add(to);

  const rem = headerOf(res, 'x-agentgate-ratelimit-remaining-tokens');
  if (rem !== '') ratelimitRemaining.add(parseFloat(rem));

  // Timing. res.timings.waiting is time-to-first-byte: for a streamed response that is
  // the closest client-side proxy for time-to-first-token available without an SSE
  // extension. See test/load/README.md for the caveat and the cross-check.
  if (o.stream) {
    ttft.add(res.timings.waiting, { pool: headerOf(res, 'x-agentgate-pool') || 'unknown' });
    streamDuration.add(res.timings.duration);
  } else {
    unaryDuration.add(res.timings.duration);
  }

  // Gateway overhead: total wall clock minus the provider's share, taken from the
  // server-timing header the gateway emits alongside the SPEC 2.3 set.
  const st = parseServerTiming(headerOf(res, 'server-timing'));
  if (st.provider !== null) {
    providerTime.add(st.provider);
    gatewayOverhead.add(Math.max(0, res.timings.duration - st.provider));
  } else if (st.overhead !== null) {
    gatewayOverhead.add(st.overhead);
  }

  return {
    status: status,
    code: code,
    success: isSuccess,
    attempts: at,
    cache: cache,
    provider: headerOf(res, 'x-agentgate-provider'),
    backend: headerOf(res, 'x-agentgate-model'),
    pool: headerOf(res, 'x-agentgate-pool'),
    guardrail: headerOf(res, 'x-agentgate-guardrail'),
    retryAfter: headerOf(res, 'Retry-After'),
    costUsd: cost,
    tokensIn: ti,
    tokensOut: to,
  };
}

/** Extract the stable error code from an RFC 9457 problem+json body (SPEC 2.4). */
export function errorCode(res) {
  if (res.status >= 200 && res.status < 300) return '';
  try {
    const b = res.json();
    return (b && b.code) || '';
  } catch (e) {
    return '';
  }
}

/**
 * Parse `Server-Timing: agentgate;dur=41.2, provider;dur=812.5`.
 * Returns { overhead, provider } in ms, or nulls when the header is absent.
 */
export function parseServerTiming(value) {
  const out = { overhead: null, provider: null };
  if (!value) return out;
  const parts = String(value).split(',');
  for (let i = 0; i < parts.length; i++) {
    const m = parts[i].trim().match(/^([a-zA-Z0-9_-]+);\s*dur=([0-9.]+)/);
    if (!m) continue;
    if (m[1] === 'agentgate' || m[1] === 'gateway') out.overhead = parseFloat(m[2]);
    if (m[1] === 'provider' || m[1] === 'backend') out.provider = parseFloat(m[2]);
  }
  return out;
}

// ---------------------------------------------------------------------------
// SSE parsing
// ---------------------------------------------------------------------------

/**
 * Parse a complete SSE body into its frames.
 *
 * The gateway's stream contract (SPEC 2.5):
 *   - `data:` frames carrying OpenAI chunk objects
 *   - terminated by `data: [DONE]`
 *   - `event: agentgate.usage` before [DONE] when include_usage is set
 *   - `event: agentgate.failover` only before the first content byte
 *   - heartbeat comment lines beginning with `:` every 15s
 *
 * Returns counts and the parsed usage payload. Per-frame arrival timing is not
 * available from a buffered body; mean inter-token latency is derived instead.
 */
export function parseSSE(body) {
  const out = {
    dataFrames: 0,
    contentChunks: 0,
    contentChars: 0,
    heartbeats: 0,
    done: false,
    usage: null,
    failover: null,
    errorFrame: null,
    finishReasons: [],
    malformed: 0,
  };
  if (!body) return out;

  const lines = String(body).split('\n');
  let currentEvent = null;

  for (let i = 0; i < lines.length; i++) {
    const line = lines[i].replace(/\r$/, '');
    if (line === '') {
      currentEvent = null;
      continue;
    }
    if (line.charAt(0) === ':') {
      out.heartbeats += 1;
      continue;
    }
    if (line.indexOf('event:') === 0) {
      currentEvent = line.slice(6).trim();
      continue;
    }
    if (line.indexOf('data:') !== 0) {
      continue;
    }

    const payload = line.slice(5).trim();
    if (payload === '[DONE]') {
      out.done = true;
      continue;
    }

    out.dataFrames += 1;

    let obj = null;
    try {
      obj = JSON.parse(payload);
    } catch (e) {
      out.malformed += 1;
      continue;
    }

    if (currentEvent === 'agentgate.usage') {
      out.usage = obj;
      continue;
    }
    if (currentEvent === 'agentgate.failover') {
      out.failover = obj;
      continue;
    }
    if (currentEvent === 'error') {
      out.errorFrame = obj;
      continue;
    }

    if (obj.choices && obj.choices.length > 0) {
      const c = obj.choices[0];
      const delta = c.delta || {};
      if (typeof delta.content === 'string' && delta.content.length > 0) {
        out.contentChunks += 1;
        out.contentChars += delta.content.length;
      }
      if (c.finish_reason) {
        out.finishReasons.push(c.finish_reason);
      }
    }
  }
  return out;
}

/**
 * Assert the SSE contract and record the stream metrics. `res` must be the response to
 * a stream:true request.
 */
export function checkStream(res, opts) {
  const o = opts || {};
  const frames = parseSSE(res.body);
  const elapsed = res.timings.duration;
  const firstByte = res.timings.waiting;
  const minHeartbeats = Math.floor(elapsed / 15000); // one every 15s per SPEC 2.5

  const checks = {
    'stream: terminated with [DONE]': () => frames.done,
    'stream: delivered content chunks': () => frames.contentChunks > 0,
    'stream: no malformed data frames': () => frames.malformed === 0,
    'stream: no error frame': () => frames.errorFrame === null,
    'stream: heartbeats present when the stream ran past 15s': () =>
      minHeartbeats === 0 || frames.heartbeats >= minHeartbeats,
    'stream: failover frame only before first content': () =>
      frames.failover === null || frames.failover.before_first_content !== false,
  };
  if (o.expectUsage !== false) {
    checks['stream: usage frame present'] = () => frames.usage !== null;
  }

  const ok = check(res, checks);
  streamContractOk.add(ok);
  doneTerminatorSeen.add(frames.done);
  heartbeatSeen.add(minHeartbeats === 0 || frames.heartbeats >= minHeartbeats);
  usageFrameSeen.add(frames.usage !== null);

  // Mean inter-token latency: (total - ttft) / output tokens. Coarse, but it is the
  // one inter-token figure derivable from a buffered body, and it moves when a
  // provider's generation rate degrades.
  const outTokens =
    (frames.usage && frames.usage.usage && frames.usage.usage.completion_tokens) ||
    Math.max(1, Math.round(frames.contentChars / 4));
  if (outTokens > 1 && elapsed > firstByte) {
    interTokenMean.add((elapsed - firstByte) / (outTokens - 1));
  }

  return frames;
}

// ---------------------------------------------------------------------------
// Convenience callers
// ---------------------------------------------------------------------------

export function chat(token, opts) {
  const o = opts || {};
  const params = requestParams(
    Object.assign({}, o, { token: token, name: o.name || (o.stream ? 'chat_stream' : 'chat_unary') })
  );
  const res = http.post(`${CFG.gateway}/v1/chat/completions`, chatBody(o), params);
  checkContractHeaders(res, o);
  const summary = recordResponse(res, o);
  return { res: res, summary: summary };
}

export function embeddings(token, opts) {
  const o = opts || {};
  const params = requestParams(Object.assign({}, o, { token: token, name: 'embeddings' }));
  const res = http.post(`${CFG.gateway}/v1/embeddings`, embeddingsBody(o), params);
  checkContractHeaders(res, o);
  const summary = recordResponse(res, o);
  return { res: res, summary: summary };
}

export function listModels(token) {
  const res = http.get(`${CFG.gateway}/v1/models`, {
    headers: { Authorization: `Bearer ${token}` },
    tags: { name: 'models' },
  });
  return res;
}

export function tokenCount(token, opts) {
  const o = opts || {};
  const res = http.post(
    `${CFG.gateway}/v1/token-count`,
    JSON.stringify({
      model: o.model || CFG.model,
      messages: [{ role: 'user', content: o.prompt || promptOfTokens(200) }],
    }),
    requestParams(Object.assign({}, o, { token: token, name: 'token_count' }))
  );
  return res;
}

export function health(path) {
  return http.get(`${CFG.gateway}${path || '/healthz'}`, { tags: { name: 'health' } });
}

// ---------------------------------------------------------------------------
// Reporting
// ---------------------------------------------------------------------------

/**
 * Common banner printed at the start of every scenario, so a saved console log states
 * what was actually run rather than what someone remembers running.
 */
export function banner(scenario) {
  console.log(
    [
      `scenario=${scenario}`,
      `gateway=${CFG.gateway}`,
      `env=${CFG.env}`,
      `pool=${CFG.pool}`,
      `model=${CFG.model}`,
      `stream_ratio=${CFG.streamRatio}`,
      `max_tokens=${CFG.maxTokens}`,
      `prompt_tokens=${CFG.promptTokens}`,
      `cache=${CFG.cache}`,
      `priority=${CFG.priority}`,
    ].join(' ')
  );
}

/** Decide whether this iteration is a streaming one, honouring the configured mix. */
export function shouldStream(ratio) {
  const r = ratio === undefined ? CFG.streamRatio : ratio;
  return Math.random() < r;
}
