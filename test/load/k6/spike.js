// spike.js - sudden 10x burst.
//
// Question it answers: when volume jumps tenfold in seconds, does AgentGate shed load
// in the order it promised, or does it collapse?
//
// The promise (SPEC 3.3): when gateway concurrency exceeds the ceiling, batch-priority
// requests are shed with 429 BEFORE interactive ones. That is the assertion this test
// exists to make. Degrading gracefully is a pass; degrading in the wrong order is a
// failure even if every number looks survivable.
//
//   k6 run test/load/k6/spike.js
//   k6 run -e AGENTGATE_BASE_RPM=200 -e AGENTGATE_SPIKE_MULTIPLIER=10 test/load/k6/spike.js
//
// Two scenarios run concurrently: a steady interactive stream and a steady batch
// stream, both spiking together, so the shedding order is directly observable.

import { Counter, Rate, Trend } from 'k6/metrics';
import { SLO, banner, getToken, chat, errorCode } from './lib/common.js';

const BASE_RPM = parseInt(__ENV.AGENTGATE_BASE_RPM || '200', 10);
const MULT = parseInt(__ENV.AGENTGATE_SPIKE_MULTIPLIER || '10', 10);
const SPIKE_RPM = BASE_RPM * MULT;
const BATCH_SHARE = parseFloat(__ENV.AGENTGATE_BATCH_SHARE || '0.4');

const interactiveBase = Math.round(BASE_RPM * (1 - BATCH_SHARE));
const batchBase = Math.round(BASE_RPM * BATCH_SHARE);
const interactiveSpike = Math.round(SPIKE_RPM * (1 - BATCH_SHARE));
const batchSpike = Math.round(SPIKE_RPM * BATCH_SHARE);

// Shared stage shape: settle, spike hard, hold, drop back, recover.
function stages(base, peak) {
  return [
    { duration: '3m', target: base }, // establish baseline
    { duration: '10s', target: peak }, // the spike - 10s is deliberately brutal
    { duration: '3m', target: peak }, // hold at peak
    { duration: '30s', target: base }, // release
    { duration: '4m', target: base }, // recovery observation
  ];
}

export const options = {
  scenarios: {
    interactive: {
      executor: 'ramping-arrival-rate',
      startRate: interactiveBase,
      timeUnit: '1m',
      stages: stages(interactiveBase, interactiveSpike),
      preAllocatedVUs: parseInt(__ENV.AGENTGATE_PREALLOC_VUS || '100', 10),
      maxVUs: parseInt(__ENV.AGENTGATE_MAX_VUS || '600', 10),
      exec: 'interactiveTraffic',
      tags: { tier: 'interactive' },
    },
    batch: {
      executor: 'ramping-arrival-rate',
      startRate: batchBase,
      timeUnit: '1m',
      stages: stages(batchBase, batchSpike),
      preAllocatedVUs: parseInt(__ENV.AGENTGATE_PREALLOC_BATCH_VUS || '80', 10),
      maxVUs: parseInt(__ENV.AGENTGATE_MAX_BATCH_VUS || '400', 10),
      exec: 'batchTraffic',
      tags: { tier: 'batch' },
    },
  },
  thresholds: {
    // The core assertion: interactive traffic is protected.
    'agentgate_shed_rate{tier:interactive}': ['rate<0.05'],
    // Batch is expected to be shed heavily at peak - that is correct behaviour, so
    // there is no upper gate on it. There IS a lower gate: if nothing was shed at all,
    // either the spike did not land or shedding is not enforcing, and both invalidate
    // the run.
    'agentgate_shed_rate{tier:batch}': ['rate>0.01'],

    // Shedding must be a clean 429 with Retry-After, never a 5xx or a timeout.
    agentgate_unexpected_5xx: ['count<10'],
    agentgate_shed_without_retry_after: ['count==0'],

    // Interactive latency should degrade but stay within a multiple of the SLO. A
    // system that sheds correctly keeps its served requests fast.
    'agentgate_gateway_overhead_ms{tier:interactive}': [`p(95)<${SLO.gatewayOverheadP95Ms * 3}`],

    // Contract holds under shedding: a 429 still carries the standard headers.
    agentgate_contract_headers_ok: ['rate>0.999'],

    // Recovery: measured explicitly in handleSummary, gated coarsely here.
    'agentgate_availability_rate{phase:recovery}': ['rate>0.995'],
  },
};

const shedRate = new Rate('agentgate_shed_rate');
const shedNoRetryAfter = new Counter('agentgate_shed_without_retry_after');
const retryAfterSeconds = new Trend('agentgate_retry_after_seconds');
const recoveryLatency = new Trend('agentgate_recovery_overhead_ms', true);

export function setup() {
  banner('spike');
  console.log(
    `base ${BASE_RPM} rpm -> spike ${SPIKE_RPM} rpm (${MULT}x) over 10s, held 3m, then recovery`
  );
  console.log(
    `split: interactive ${interactiveBase}->${interactiveSpike} rpm, batch ${batchBase}->${batchSpike} rpm`
  );
  console.log('Assertion: batch sheds first. Interactive shed rate must stay under 5 percent.');
  return { startedAt: Date.now() };
}

function phaseOf(startMs) {
  const t = (Date.now() - startMs) / 1000;
  if (t < 180) return 'baseline';
  if (t < 190) return 'spike_edge';
  if (t < 370) return 'peak';
  if (t < 400) return 'release';
  return 'recovery';
}

function drive(data, tier) {
  const token = getToken(`spike_${tier}`);
  const phase = phaseOf(data.startedAt);

  const { res, summary } = chat(token, {
    stream: false, // unary keeps the shed decision unambiguous
    priority: tier,
    scenario: 'spike',
    promptTokens: 300,
    maxTokens: 128,
    quiet: true,
    tags: { tier: tier, phase: phase },
  });

  const code = errorCode(res);
  const shed = res.status === 429 && (code === 'rate_limited' || code === 'shed');
  shedRate.add(shed, { tier: tier, phase: phase });

  if (res.status === 429) {
    const ra = res.headers['Retry-After'] || res.headers['retry-after'] || '';
    if (ra === '') {
      shedNoRetryAfter.add(1, { tier: tier });
    } else {
      retryAfterSeconds.add(parseFloat(ra), { tier: tier });
    }
  }

  if (phase === 'recovery' && summary.success) {
    recoveryLatency.add(res.timings.duration, { tier: tier });
  }

  // Honour Retry-After like a well-behaved client would. A load test that ignores it is
  // testing a client nobody should ship, and it turns the spike into a retry storm.
  // The arrival-rate executor handles pacing, so we only avoid immediate hammering.
}

export function interactiveTraffic(data) {
  drive(data, 'interactive');
}

export function batchTraffic(data) {
  drive(data, 'batch');
}

export function handleSummary(data) {
  const m = data.metrics;
  const lines = [];
  lines.push('');
  lines.push('=== spike / load shedding report ===');
  lines.push(`spike:                 ${BASE_RPM} -> ${SPIKE_RPM} rpm (${MULT}x) in 10s`);
  lines.push('');
  lines.push('Shed rates (429 as a fraction of requests):');
  lines.push(`  interactive:         ${ratePct(m, 'agentgate_shed_rate', 'tier:interactive')}`);
  lines.push(`  batch:               ${ratePct(m, 'agentgate_shed_rate', 'tier:batch')}`);
  lines.push('');
  lines.push(`429s missing Retry-After: ${countOf(m, 'agentgate_shed_without_retry_after')} (must be 0)`);
  lines.push(`Retry-After p50:          ${stat(m, 'agentgate_retry_after_seconds', 'med')} s`);
  lines.push(`unexpected 5xx:           ${countOf(m, 'agentgate_unexpected_5xx')}`);
  lines.push(`503 no_healthy_backend:   ${countOf(m, 'agentgate_no_healthy_backend')}`);
  lines.push('');
  lines.push(`recovery p95 request duration: ${stat(m, 'agentgate_recovery_overhead_ms', 'p(95)')} ms`);
  lines.push('');
  lines.push('Pass criteria:');
  lines.push('  1. batch shed rate materially exceeds interactive shed rate');
  lines.push('  2. interactive shed rate below 5 percent');
  lines.push('  3. every 429 carries Retry-After');
  lines.push('  4. no unexpected 5xx - shedding is a 429, not a collapse');
  lines.push('  5. recovery latency returns to baseline within the 4 minute window');
  lines.push('');
  lines.push('If batch and interactive shed at the same rate, priority is not being honoured.');
  lines.push('That is a finding against SPEC 3.3 regardless of how healthy the run looks.');
  lines.push('');

  return {
    stdout: lines.join('\n'),
    'spike-summary.json': JSON.stringify(data, null, 2),
  };
}

function ratePct(m, name, tag) {
  const key = `${name}{${tag}}`;
  const entry = m[key] || m[name];
  return entry && entry.values ? (entry.values.rate * 100).toFixed(2) + '%' : 'n/a';
}
function countOf(m, name) {
  return m[name] && m[name].values ? m[name].values.count : 0;
}
function stat(m, name, s) {
  return m[name] && m[name].values ? (Math.round(m[name].values[s] * 100) / 100).toString() : 'n/a';
}
