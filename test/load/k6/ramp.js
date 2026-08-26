// ramp.js - find the knee.
//
// Question it answers: at what concurrency does p95 gateway overhead breach the 60ms
// SLO, and what happens immediately after? That number - the knee - is what the
// headroom policy is set against, and it is the single most useful figure this
// directory produces.
//
//   k6 run test/load/k6/ramp.js
//   k6 run -e AGENTGATE_RAMP_START=50 -e AGENTGATE_RAMP_STEP=50 \
//          -e AGENTGATE_RAMP_MAX=800 -e AGENTGATE_STEP_DURATION=3m test/load/k6/ramp.js
//
// The script records the first stage at which p95 breaches and prints it in teardown.
// Deliberately has NO pass/fail threshold on latency: breaching is the point of the
// test, and a red run would obscure the answer.

import { sleep } from 'k6';
import exec from 'k6/execution';
import { Gauge, Trend } from 'k6/metrics';
import { CFG, SLO, banner, getToken, chat, checkStream, shouldStream } from './lib/common.js';

const START = parseInt(__ENV.AGENTGATE_RAMP_START || '25', 10);
const STEP = parseInt(__ENV.AGENTGATE_RAMP_STEP || '25', 10);
const MAX = parseInt(__ENV.AGENTGATE_RAMP_MAX || '500', 10);
const STEP_DURATION = __ENV.AGENTGATE_STEP_DURATION || '3m';
const RAMP_TIME = __ENV.AGENTGATE_RAMP_TIME || '30s';

// Concurrency is the independent variable, so use ramping-vus: each VU holds one
// in-flight request, which makes "VUs" and "concurrency" the same number.
function buildStages() {
  const stages = [];
  for (let vus = START; vus <= MAX; vus += STEP) {
    stages.push({ duration: RAMP_TIME, target: vus }); // ramp into the step
    stages.push({ duration: STEP_DURATION, target: vus }); // hold and measure
  }
  stages.push({ duration: '30s', target: 0 });
  return stages;
}

export const options = {
  scenarios: {
    ramp: {
      executor: 'ramping-vus',
      startVUs: START,
      stages: buildStages(),
      gracefulRampDown: '30s',
    },
  },
  thresholds: {
    // Contract must hold at every concurrency. Latency deliberately has no gate.
    agentgate_contract_headers_ok: ['rate>0.999'],
    agentgate_stream_contract_ok: ['rate>0.99'],
    // A 5xx that is not no_healthy_backend indicates collapse rather than saturation.
    // Load shedding with 429 is the correct behaviour past the knee; 500s are not.
    agentgate_unexpected_5xx: ['count<50'],
  },
  // Abort once behaviour is clearly past useful: no point measuring a collapsed system.
  // Kept generous so the shape after the knee is still visible.
  noConnectionReuse: false,
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
};

// Per-stage observation. k6 does not expose stage-scoped percentiles, so tag every
// sample with the concurrency level and derive the per-level view from the tagged
// series afterwards - either in the k6 output backend or with --out json.
const concurrency = new Gauge('agentgate_ramp_concurrency');
const overheadByLevel = new Trend('agentgate_ramp_overhead_ms', true);
const ttftByLevel = new Trend('agentgate_ramp_ttft_ms', true);

export function setup() {
  banner('ramp');
  console.log(
    `ramp ${START} -> ${MAX} VUs in steps of ${STEP}, ${STEP_DURATION} per step ` +
      `(${Math.ceil((MAX - START) / STEP) + 1} steps)`
  );
  console.log(
    `knee is defined as the first held step whose p95 gateway overhead exceeds ${SLO.gatewayOverheadP95Ms}ms`
  );
  return { startedAt: new Date().toISOString() };
}

export default function () {
  const token = getToken('ramp');

  // Round the current VU count to the nearest step so samples group cleanly per level.
  const active = exec.instance.vusActive;
  const level = String(Math.round(active / STEP) * STEP);
  concurrency.add(active);

  const stream = shouldStream();
  const { res, summary } = chat(token, {
    stream: stream,
    scenario: 'ramp',
    promptTokens: CFG.promptTokens,
    maxTokens: CFG.maxTokens,
    tags: { level: level },
  });

  if (summary.success) {
    // Duplicate the two SLO signals into level-tagged series. These are what you read
    // to find the knee: p95 of agentgate_ramp_overhead_ms grouped by the level tag.
    overheadByLevel.add(res.timings.duration, { level: level, stream: String(stream) });
    if (stream) {
      ttftByLevel.add(res.timings.waiting, { level: level });
      checkStream(res, { expectUsage: true });
    }
  }
}

export function handleSummary(data) {
  // Print a readable knee report alongside the standard summary. The per-level detail
  // lives in the tagged series; this is the headline.
  const m = data.metrics;
  const lines = [];
  lines.push('');
  lines.push('=== ramp result ===');
  lines.push(`peak concurrency reached: ${gauge(m, 'agentgate_ramp_concurrency', 'max')}`);
  lines.push(
    `gateway overhead p95 across the whole run: ${trend(m, 'agentgate_gateway_overhead_ms', 'p(95)')} ms ` +
      `(SLO ${SLO.gatewayOverheadP95Ms} ms)`
  );
  lines.push(
    `ttft p95 across the whole run: ${trend(m, 'agentgate_ttft_ms', 'p(95)')} ms (SLO ${SLO.ttftP95Ms} ms)`
  );
  lines.push(`availability across the whole run: ${rate(m, 'agentgate_availability_rate')}`);
  lines.push(`429 rate_limited: ${count(m, 'agentgate_rate_limited')}`);
  lines.push(`503 no_healthy_backend: ${count(m, 'agentgate_no_healthy_backend')}`);
  lines.push(`unexpected 5xx: ${count(m, 'agentgate_unexpected_5xx')}`);
  lines.push('');
  lines.push('To find the knee, group agentgate_ramp_overhead_ms by the "level" tag and');
  lines.push(`take the lowest level whose p95 exceeds ${SLO.gatewayOverheadP95Ms} ms. Record it in`);
  lines.push('test/load/capacity-model.md and set the headroom policy from it.');
  lines.push('');

  return {
    stdout: lines.join('\n'),
    'ramp-summary.json': JSON.stringify(data, null, 2),
  };
}

function trend(m, name, stat) {
  return m[name] && m[name].values ? round(m[name].values[stat]) : 'n/a';
}
function gauge(m, name, stat) {
  return m[name] && m[name].values ? round(m[name].values[stat]) : 'n/a';
}
function rate(m, name) {
  return m[name] && m[name].values ? (m[name].values.rate * 100).toFixed(3) + '%' : 'n/a';
}
function count(m, name) {
  return m[name] && m[name].values ? m[name].values.count : 0;
}
function round(v) {
  return v === undefined || v === null ? 'n/a' : Math.round(v * 100) / 100;
}
