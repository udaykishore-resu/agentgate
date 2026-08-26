// baseline.js - steady state at expected production volume.
//
// Question it answers: at the volume we actually expect, does AgentGate meet the SLOs
// in SPEC section 5? This is the test whose green result is quoted in the operational
// readiness review.
//
// Mix: unary and streaming chat in the configured ratio, plus a small embeddings share,
// because embeddings share the policy chain and the Redis path and their absence would
// flatter the result.
//
//   k6 run -e AGENTGATE_TARGET_RPM=1000 test/load/k6/baseline.js
//
// Thresholds are the SLOs. A threshold failure is a capacity or regression finding, not
// a flaky test - investigate before re-running.

import { sleep, group } from 'k6';
import {
  CFG,
  SLO,
  banner,
  getToken,
  chat,
  embeddings,
  checkStream,
  shouldStream,
} from './lib/common.js';

const TARGET_RPM = parseInt(__ENV.AGENTGATE_TARGET_RPM || '1000', 10);
const DURATION = __ENV.AGENTGATE_DURATION || '30m';
const EMBED_RATIO = parseFloat(__ENV.AGENTGATE_EMBED_RATIO || '0.15');

// Pre-allocation sized from the capacity model: expected concurrency is
// arrival_rate * mean_service_time. At 1000 rpm with a mean of 2.4s that is 40
// concurrent, so allocate 60 and allow headroom to 150 for the streaming tail.
const PREALLOC = parseInt(__ENV.AGENTGATE_PREALLOC_VUS || '60', 10);
const MAXVUS = parseInt(__ENV.AGENTGATE_MAX_VUS || '150', 10);

export const options = {
  scenarios: {
    baseline: {
      executor: 'constant-arrival-rate',
      rate: TARGET_RPM,
      timeUnit: '1m',
      duration: DURATION,
      preAllocatedVUs: PREALLOC,
      maxVUs: MAXVUS,
      gracefulStop: '2m',
    },
  },
  thresholds: {
    // --- SLOs from SPEC section 5 -----------------------------------------
    // Availability: non-5xx and non-no_healthy_backend over total, 99.9%.
    agentgate_availability_rate: [`rate>=${SLO.availability}`],
    // Gateway overhead excluding provider time, p95 < 60ms.
    agentgate_gateway_overhead_ms: [`p(95)<${SLO.gatewayOverheadP95Ms}`],
    // Time to first token, p95 < 1200ms.
    agentgate_ttft_ms: [`p(95)<${SLO.ttftP95Ms}`],
    // Control plane token exchange, p95 < 150ms.
    agentgate_token_mint_ms: [`p(95)<${SLO.controlPlaneExchangeP95Ms}`],

    // --- Contract ---------------------------------------------------------
    agentgate_contract_headers_ok: ['rate==1.0'],
    agentgate_stream_contract_ok: ['rate>0.999'],
    agentgate_done_terminator_seen: ['rate==1.0'],
    agentgate_usage_frame_seen: ['rate>0.999'],

    // --- Health of the run itself ----------------------------------------
    // A 5xx that is not no_healthy_backend should not happen at baseline volume.
    agentgate_unexpected_5xx: ['count<10'],
    // Retries are normal; a high failover rate at steady state is not.
    agentgate_failover_rate: ['rate<0.02'],
    // If the arrival rate cannot be sustained, k6 drops iterations. That invalidates
    // the result, so fail loudly rather than reporting a green run at half the volume.
    dropped_iterations: ['count<10'],

    // --- Per-scenario tagged views ---------------------------------------
    'http_req_duration{name:chat_unary}': ['p(95)<5000'],
    'http_req_duration{name:embeddings}': ['p(95)<1000'],
  },
};

export function setup() {
  banner('baseline');
  console.log(
    `target=${TARGET_RPM} rpm duration=${DURATION} stream_ratio=${CFG.streamRatio} embed_ratio=${EMBED_RATIO}`
  );
  console.log(
    `SLO gates: overhead p95 < ${SLO.gatewayOverheadP95Ms}ms, ttft p95 < ${SLO.ttftP95Ms}ms, availability >= ${SLO.availability}`
  );
  return { startedAt: new Date().toISOString() };
}

export default function () {
  const token = getToken('baseline');

  if (Math.random() < EMBED_RATIO) {
    group('embeddings', function () {
      embeddings(token, { promptTokens: 200, scenario: 'baseline' });
    });
    return;
  }

  const stream = shouldStream();

  if (stream) {
    group('chat stream', function () {
      const { res, summary } = chat(token, {
        stream: true,
        scenario: 'baseline',
        // Vary prompt size around the configured mean so the test does not accidentally
        // measure a single cache-friendly shape.
        promptTokens: jitterInt(CFG.promptTokens, 0.4),
        maxTokens: CFG.maxTokens,
        temperature: 0.2,
      });
      if (summary.success) {
        checkStream(res, { expectUsage: true });
      }
    });
  } else {
    group('chat unary', function () {
      chat(token, {
        stream: false,
        scenario: 'baseline',
        promptTokens: jitterInt(CFG.promptTokens, 0.4),
        maxTokens: CFG.maxTokens,
        temperature: 0.2,
      });
    });
  }

  // No sleep: the arrival-rate executor controls pacing. A sleep here would silently
  // reduce the achieved rate and produce a green run at the wrong volume.
}

function jitterInt(base, fraction) {
  const spread = base * fraction;
  return Math.max(50, Math.round(base - spread / 2 + Math.random() * spread));
}

export function teardown(data) {
  console.log(`baseline complete, started ${data.startedAt}`);
  console.log(
    'Read the result against test/load/README.md section "Reading a baseline result" before ' +
      'declaring capacity proven.'
  );
}
