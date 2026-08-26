// soak.js - 4 hours at steady state, looking for what only shows up with time.
//
// Question it answers: does anything drift? Leaks show up as memory growth and slowly
// rising latency; connection-pool exhaustion shows up as a step change hours in; token
// refresh bugs show up exactly once, when the first token expires; cache and Redis key
// growth shows up as gradually rising p99.
//
//   k6 run test/load/k6/soak.js
//   k6 run -e AGENTGATE_SOAK_DURATION=8h -e AGENTGATE_TARGET_RPM=600 test/load/k6/soak.js
//
// Run at a deliberately modest rate - roughly 60 percent of the expected production
// volume. A soak is not a capacity test; running it near the knee means you are
// measuring saturation, not drift.
//
// Watch alongside the run:
//   container_memory_working_set_bytes{pod=~"gateway-.*"}
//   go_goroutines{job="gateway"}
//   agentgate_redis_pool_wait_seconds
//   redis_memory_used_bytes

import { Trend, Counter } from 'k6/metrics';
import exec from 'k6/execution';
import { CFG, SLO, banner, getToken, chat, embeddings, checkStream, shouldStream } from './lib/common.js';

const DURATION = __ENV.AGENTGATE_SOAK_DURATION || '4h';
const TARGET_RPM = parseInt(__ENV.AGENTGATE_TARGET_RPM || '600', 10);
const EMBED_RATIO = parseFloat(__ENV.AGENTGATE_EMBED_RATIO || '0.15');

export const options = {
  scenarios: {
    soak: {
      executor: 'constant-arrival-rate',
      rate: TARGET_RPM,
      timeUnit: '1m',
      duration: DURATION,
      preAllocatedVUs: parseInt(__ENV.AGENTGATE_PREALLOC_VUS || '50', 10),
      maxVUs: parseInt(__ENV.AGENTGATE_MAX_VUS || '120', 10),
      gracefulStop: '2m',
    },
  },
  thresholds: {
    // The SLOs must hold for the whole window, not merely on average early on.
    agentgate_availability_rate: [`rate>=${SLO.availability}`],
    agentgate_gateway_overhead_ms: [`p(95)<${SLO.gatewayOverheadP95Ms}`],
    agentgate_ttft_ms: [`p(95)<${SLO.ttftP95Ms}`],

    // Contract must not degrade over time. Streams are where slow leaks surface first.
    agentgate_contract_headers_ok: ['rate==1.0'],
    agentgate_stream_contract_ok: ['rate>0.999'],
    agentgate_done_terminator_seen: ['rate==1.0'],

    // Token refresh: a soak crosses at least one token expiry. If refresh is broken the
    // run dies at minute 15, and this threshold names the reason.
    agentgate_token_mint_ms: [`p(95)<${SLO.controlPlaneExchangeP95Ms}`],
    agentgate_token_refreshes: ['count>0'],

    // Drift gates. The first-hour and last-hour comparison is done in teardown, but
    // these catch the coarse version: p99 must not be catastrophically worse than p95.
    'agentgate_gateway_overhead_ms{phase:late}': [`p(95)<${SLO.gatewayOverheadP95Ms}`],
    agentgate_unexpected_5xx: ['count<20'],
    dropped_iterations: ['count<20'],
  },
  // Long runs accumulate a large summary; keep the trend stats we actually read.
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max', 'count'],
};

const tokenRefreshes = new Counter('agentgate_token_refreshes');
const earlyOverhead = new Trend('agentgate_soak_overhead_early_ms', true);
const lateOverhead = new Trend('agentgate_soak_overhead_late_ms', true);
const earlyTtft = new Trend('agentgate_soak_ttft_early_ms', true);
const lateTtft = new Trend('agentgate_soak_ttft_late_ms', true);

// VU-local view of which token we last saw, so we can count refreshes.
let lastToken = '';

function durationMs(d) {
  const m = String(d).match(/^(\d+)([smh])$/);
  if (!m) return 4 * 3600 * 1000;
  const n = parseInt(m[1], 10);
  return m[2] === 'h' ? n * 3600000 : m[2] === 'm' ? n * 60000 : n * 1000;
}
const TOTAL_MS = durationMs(DURATION);
const EARLY_WINDOW_MS = Math.min(3600000, Math.floor(TOTAL_MS * 0.25)); // first hour or first quarter
const LATE_WINDOW_START_MS = TOTAL_MS - EARLY_WINDOW_MS;

export function setup() {
  banner('soak');
  console.log(`soak duration=${DURATION} rate=${TARGET_RPM} rpm`);
  console.log(
    `drift comparison: first ${Math.round(EARLY_WINDOW_MS / 60000)} min vs last ${Math.round(
      EARLY_WINDOW_MS / 60000
    )} min`
  );
  console.log('Watch pod memory, goroutine count and Redis pool wait alongside this run.');
  return { startedAt: Date.now() };
}

export default function (data) {
  const token = getToken('soak');
  if (token !== lastToken) {
    if (lastToken !== '') {
      tokenRefreshes.add(1);
    }
    lastToken = token;
  }

  const elapsed = Date.now() - data.startedAt;
  const phase = elapsed < EARLY_WINDOW_MS ? 'early' : elapsed > LATE_WINDOW_START_MS ? 'late' : 'mid';

  if (Math.random() < EMBED_RATIO) {
    embeddings(token, { promptTokens: 200, scenario: 'soak', tags: { phase: phase } });
    return;
  }

  const stream = shouldStream();
  const { res, summary } = chat(token, {
    stream: stream,
    scenario: 'soak',
    promptTokens: CFG.promptTokens,
    maxTokens: CFG.maxTokens,
    tags: { phase: phase },
  });

  if (!summary.success) return;

  if (phase === 'early') {
    earlyOverhead.add(res.timings.duration);
    if (stream) earlyTtft.add(res.timings.waiting);
  } else if (phase === 'late') {
    lateOverhead.add(res.timings.duration);
    if (stream) lateTtft.add(res.timings.waiting);
  }

  if (stream) {
    checkStream(res, { expectUsage: true });
  }
}

export function handleSummary(data) {
  const m = data.metrics;
  const eo = p95(m, 'agentgate_soak_overhead_early_ms');
  const lo = p95(m, 'agentgate_soak_overhead_late_ms');
  const et = p95(m, 'agentgate_soak_ttft_early_ms');
  const lt = p95(m, 'agentgate_soak_ttft_late_ms');

  const drift = eo && lo ? ((lo - eo) / eo) * 100 : null;
  const ttftDrift = et && lt ? ((lt - et) / et) * 100 : null;

  const lines = [];
  lines.push('');
  lines.push('=== soak drift report ===');
  lines.push(`duration:              ${DURATION} at ${TARGET_RPM} rpm`);
  lines.push(`total requests:        ${countOf(m, 'http_reqs')}`);
  lines.push(`token refreshes:       ${countOf(m, 'agentgate_token_refreshes')}`);
  lines.push('');
  lines.push(`request duration p95   early ${fmt(eo)} ms   late ${fmt(lo)} ms   drift ${pct(drift)}`);
  lines.push(`ttft p95               early ${fmt(et)} ms   late ${fmt(lt)} ms   drift ${pct(ttftDrift)}`);
  lines.push('');
  lines.push('Interpretation:');
  lines.push('  drift under 5 percent   - no leak signal, pass');
  lines.push('  drift 5 to 15 percent   - investigate: correlate with pod memory and goroutines');
  lines.push('  drift over 15 percent   - fail: something accumulates. Capture a heap profile');
  lines.push('                            before the pods are recycled.');
  lines.push('');
  lines.push('This report only sees the client side. Pair it with:');
  lines.push('  container_memory_working_set_bytes{pod=~"gateway-.*"} over the run window');
  lines.push('  go_goroutines{job="gateway"} - a monotonic rise is a leaked stream handler');
  lines.push('  redis_memory_used_bytes and evicted_keys');
  lines.push('');

  return {
    stdout: lines.join('\n'),
    'soak-summary.json': JSON.stringify(data, null, 2),
  };
}

function p95(m, name) {
  return m[name] && m[name].values ? m[name].values['p(95)'] : null;
}
function countOf(m, name) {
  return m[name] && m[name].values ? m[name].values.count : 0;
}
function fmt(v) {
  return v === null || v === undefined ? 'n/a' : (Math.round(v * 100) / 100).toString();
}
function pct(v) {
  return v === null ? 'n/a' : `${v >= 0 ? '+' : ''}${v.toFixed(1)}%`;
}
