// streaming.js - SSE specific.
//
// Question it answers: does the streaming contract in SPEC 2.5 hold under load, and is
// time-to-first-token within the 1200ms SLO?
//
// Assertions, all of them contract-level:
//   - every stream terminates with `data: [DONE]`
//   - `event: agentgate.usage` is present when stream_options.include_usage is set,
//     carries a completion token count, and carries a cost
//   - heartbeat comment lines appear at least every 15s on long generations
//   - `event: agentgate.failover` appears only before the first content byte
//   - the response header set from SPEC 2.3 is present on streaming responses too
//
//   k6 run test/load/k6/streaming.js
//   k6 run -e AGENTGATE_LONG_RATIO=0.3 -e AGENTGATE_MAX_TOKENS=1500 test/load/k6/streaming.js
//
// On TTFT measurement: k6's http module buffers the response, so per-frame arrival
// times are not observable. res.timings.waiting - time to first byte - is used as the
// client-side TTFT. See test/load/README.md, "How TTFT is measured", for the caveat and
// the server-side cross-check you must run alongside this test.

import { check } from 'k6';
import { Counter, Rate, Trend } from 'k6/metrics';
import { CFG, SLO, banner, getToken, chat, checkStream } from './lib/common.js';

const TARGET_RPM = parseInt(__ENV.AGENTGATE_TARGET_RPM || '300', 10);
// Share of requests that generate long output, to exercise the heartbeat path.
const LONG_RATIO = parseFloat(__ENV.AGENTGATE_LONG_RATIO || '0.25');
const LONG_MAX_TOKENS = parseInt(__ENV.AGENTGATE_LONG_MAX_TOKENS || '1200', 10);

export const options = {
  scenarios: {
    streaming: {
      executor: 'constant-arrival-rate',
      rate: TARGET_RPM,
      timeUnit: '1m',
      duration: __ENV.AGENTGATE_DURATION || '20m',
      preAllocatedVUs: parseInt(__ENV.AGENTGATE_PREALLOC_VUS || '60', 10),
      maxVUs: parseInt(__ENV.AGENTGATE_MAX_VUS || '200', 10),
      gracefulStop: '3m',
    },
  },
  thresholds: {
    // --- SLO --------------------------------------------------------------
    agentgate_ttft_ms: [`p(95)<${SLO.ttftP95Ms}`, `p(99)<${SLO.ttftP95Ms * 2}`],

    // --- Contract, all zero-tolerance ------------------------------------
    agentgate_done_terminator_seen: ['rate==1.0'],
    agentgate_usage_frame_seen: ['rate==1.0'],
    agentgate_stream_malformed_frames: ['count==0'],
    agentgate_stream_no_content: ['count==0'],
    agentgate_usage_missing_cost: ['count==0'],
    agentgate_usage_missing_tokens: ['count==0'],
    agentgate_failover_after_content: ['count==0'],
    agentgate_contract_headers_ok: ['rate==1.0'],

    // Heartbeats: only asserted on streams that ran long enough to require one.
    agentgate_heartbeat_missing_on_long_stream: ['count==0'],

    // --- Quality of the stream itself ------------------------------------
    // Mean inter-token latency. A generation that starts fast and then crawls is a
    // real user complaint that TTFT alone never catches.
    agentgate_intertoken_mean_ms: ['p(95)<250'],
    agentgate_stream_truncated: ['count<5'],
    agentgate_availability_rate: [`rate>=${SLO.availability}`],
  },
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max', 'count'],
};

const malformedFrames = new Counter('agentgate_stream_malformed_frames');
const noContent = new Counter('agentgate_stream_no_content');
const usageMissingCost = new Counter('agentgate_usage_missing_cost');
const usageMissingTokens = new Counter('agentgate_usage_missing_tokens');
const failoverAfterContent = new Counter('agentgate_failover_after_content');
const heartbeatMissing = new Counter('agentgate_heartbeat_missing_on_long_stream');
const truncated = new Counter('agentgate_stream_truncated');
const contentChunks = new Trend('agentgate_stream_content_chunks');
const heartbeatCount = new Trend('agentgate_stream_heartbeat_count');
const usageVsHeaderMatch = new Rate('agentgate_usage_matches_headers');
const tokensPerSecond = new Trend('agentgate_stream_tokens_per_second');

export function setup() {
  banner('streaming');
  console.log(
    `streaming at ${TARGET_RPM} rpm, ${Math.round(LONG_RATIO * 100)}% long generations ` +
      `(max_tokens ${LONG_MAX_TOKENS}) to exercise the 15s heartbeat`
  );
  console.log(`TTFT gate: p95 < ${SLO.ttftP95Ms}ms`);
  console.log(
    'Cross-check client TTFT against the gateway metric during the run:\n' +
      '  histogram_quantile(0.95, sum by (le,pool) (rate(agentgate_gateway_ttft_seconds_bucket[5m])))'
  );
  return { startedAt: new Date().toISOString() };
}

export default function () {
  const token = getToken('streaming');
  const long = Math.random() < LONG_RATIO;
  const maxTokens = long ? LONG_MAX_TOKENS : CFG.maxTokens;

  const { res, summary } = chat(token, {
    stream: true,
    scenario: 'streaming',
    promptTokens: CFG.promptTokens,
    maxTokens: maxTokens,
    temperature: 0.2,
    tags: { length: long ? 'long' : 'short' },
  });

  if (!summary.success) {
    return;
  }

  // checkStream records the shared metrics and the shared assertions.
  const frames = checkStream(res, { expectUsage: true });

  // --- Stream-specific assertions beyond the shared set --------------------
  contentChunks.add(frames.contentChunks, { length: long ? 'long' : 'short' });
  heartbeatCount.add(frames.heartbeats);

  if (frames.malformed > 0) {
    malformedFrames.add(frames.malformed);
    console.error(`malformed SSE frames: ${frames.malformed} on request ${requestId(res)}`);
  }

  if (frames.contentChunks === 0) {
    noContent.add(1);
    console.error(`stream delivered no content chunks, request ${requestId(res)}`);
  }

  // A stream that ended without [DONE] and without a finish reason was truncated.
  if (!frames.done || frames.finishReasons.length === 0) {
    truncated.add(1);
    console.error(
      `truncated stream: done=${frames.done} finish_reasons=${frames.finishReasons.length} ` +
        `chunks=${frames.contentChunks} request=${requestId(res)}`
    );
  }

  // --- Usage frame contract ------------------------------------------------
  if (frames.usage !== null) {
    const u = frames.usage.usage || {};
    const hasTokens = u.completion_tokens > 0 && u.prompt_tokens > 0;
    const hasCost = frames.usage.cost_usd !== undefined && frames.usage.cost_usd !== null;

    if (!hasTokens) {
      usageMissingTokens.add(1);
      console.error(`usage frame without token counts, request ${requestId(res)}`);
    }
    if (!hasCost) {
      usageMissingCost.add(1);
      console.error(`usage frame without cost_usd, request ${requestId(res)}`);
    }

    // The usage frame and the response headers must agree. They come from different
    // code paths, and a mismatch means one of them is lying to a consuming team.
    const headerOut = summary.tokensOut;
    const frameOut = u.completion_tokens || 0;
    const agrees = headerOut === 0 || frameOut === 0 || Math.abs(headerOut - frameOut) <= 1;
    usageVsHeaderMatch.add(agrees);
    check(null, {
      'usage frame agrees with x-agentgate-tokens-output': () => agrees,
    });

    // Generation rate, from the authoritative token count.
    const genMs = res.timings.duration - res.timings.waiting;
    if (genMs > 0 && frameOut > 0) {
      tokensPerSecond.add((frameOut / genMs) * 1000, { length: long ? 'long' : 'short' });
    }
  }

  // --- Heartbeat contract, only where it applies ---------------------------
  // SPEC 2.5: a heartbeat comment every 15s. Only assert on streams that actually
  // ran past 15 seconds; asserting on a 900ms stream would be nonsense.
  const elapsedMs = res.timings.duration;
  if (elapsedMs > 16000) {
    const expected = Math.floor(elapsedMs / 15000);
    if (frames.heartbeats < expected) {
      heartbeatMissing.add(1);
      console.error(
        `heartbeats missing: got ${frames.heartbeats}, expected at least ${expected} ` +
          `over ${Math.round(elapsedMs)}ms, request ${requestId(res)}`
      );
    }
  }

  // --- Failover frame ordering ---------------------------------------------
  if (frames.failover !== null && frames.failover.before_first_content === false) {
    failoverAfterContent.add(1);
    console.error(`failover frame after first content byte, request ${requestId(res)}`);
  }
}

function requestId(res) {
  return res.headers['X-Agentgate-Request-Id'] || res.headers['x-agentgate-request-id'] || 'unknown';
}

export function handleSummary(data) {
  const m = data.metrics;
  const lines = [];
  lines.push('');
  lines.push('=== streaming report ===');
  lines.push(`streams:                    ${countOf(m, 'agentgate_stream_content_chunks')}`);
  lines.push('');
  lines.push('Latency');
  lines.push(`  ttft p50:                 ${stat(m, 'agentgate_ttft_ms', 'med')} ms`);
  lines.push(`  ttft p95:                 ${stat(m, 'agentgate_ttft_ms', 'p(95)')} ms  (SLO ${SLO.ttftP95Ms})`);
  lines.push(`  ttft p99:                 ${stat(m, 'agentgate_ttft_ms', 'p(99)')} ms`);
  lines.push(`  inter-token mean p95:     ${stat(m, 'agentgate_intertoken_mean_ms', 'p(95)')} ms`);
  lines.push(`  tokens per second p50:    ${stat(m, 'agentgate_stream_tokens_per_second', 'med')}`);
  lines.push(`  stream duration p95:      ${stat(m, 'agentgate_stream_duration_ms', 'p(95)')} ms`);
  lines.push('');
  lines.push('Contract (all must be zero except the rates)');
  lines.push(`  [DONE] terminator seen:   ${ratePct(m, 'agentgate_done_terminator_seen')}`);
  lines.push(`  usage frame seen:         ${ratePct(m, 'agentgate_usage_frame_seen')}`);
  lines.push(`  usage matches headers:    ${ratePct(m, 'agentgate_usage_matches_headers')}`);
  lines.push(`  malformed frames:         ${countOf(m, 'agentgate_stream_malformed_frames')}`);
  lines.push(`  streams with no content:  ${countOf(m, 'agentgate_stream_no_content')}`);
  lines.push(`  truncated streams:        ${countOf(m, 'agentgate_stream_truncated')}`);
  lines.push(`  usage missing tokens:     ${countOf(m, 'agentgate_usage_missing_tokens')}`);
  lines.push(`  usage missing cost:       ${countOf(m, 'agentgate_usage_missing_cost')}`);
  lines.push(`  heartbeats missing:       ${countOf(m, 'agentgate_heartbeat_missing_on_long_stream')}`);
  lines.push(`  failover after content:   ${countOf(m, 'agentgate_failover_after_content')}`);
  lines.push('');
  lines.push('Cross-check before declaring TTFT proven:');
  lines.push('  the client figure above is time-to-first-byte. Compare it against');
  lines.push('  agentgate_gateway_ttft_seconds_bucket p95 from the gateway for the same window.');
  lines.push('  A gap larger than about 50ms means headers are being flushed ahead of the first');
  lines.push('  content frame and the client number is optimistic. See README.');
  lines.push('');

  return {
    stdout: lines.join('\n'),
    'streaming-summary.json': JSON.stringify(data, null, 2),
  };
}

function countOf(m, name) {
  return m[name] && m[name].values ? m[name].values.count : 0;
}
function ratePct(m, name) {
  return m[name] && m[name].values ? (m[name].values.rate * 100).toFixed(3) + '%' : 'n/a';
}
function stat(m, name, s) {
  return m[name] && m[name].values ? (Math.round(m[name].values[s] * 100) / 100).toString() : 'n/a';
}
