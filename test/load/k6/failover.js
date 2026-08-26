// failover.js - drive traffic while a backend is deliberately failed.
//
// Question it answers: when a backend goes bad, does AgentGate move the traffic without
// the caller finding out? The central assertion is narrow and unforgiving:
//
//   NO request may return a 5xx that should have failed over.
//
// A 502 provider_error or 504 provider_timeout returned to a caller while another
// backend in the pool was healthy and closed-circuit is a failure of this test, full
// stop. 503 no_healthy_backend is only acceptable if the pool genuinely had nothing
// left, and the test records that separately so you can tell the two apart.
//
// Requires a fault-injection target. Two modes:
//   mock  - drives the mockprovider on 8090 and toggles its fault profile (default)
//   drain - drains a real backend with agentctl mid-run (staging only, needs credentials)
//
//   k6 run test/load/k6/failover.js
//   k6 run -e AGENTGATE_FAULT_MODE=drain -e AGENTGATE_FAULT_BACKEND=azure-openai/gpt-4o-mini \
//          test/load/k6/failover.js
//
// The fault is applied from setup() and lifted in teardown(), so a crashed run leaves
// the environment faulted - check and clear it manually if k6 dies mid-flight.

import http from 'k6/http';
import { check } from 'k6';
import { Counter, Rate, Trend } from 'k6/metrics';
import { SLO, banner, getToken, chat, checkStream, errorCode, shouldStream } from './lib/common.js';

const MOCK = __ENV.AGENTGATE_MOCKPROVIDER || 'http://mockprovider.agentgate.svc.cluster.local:8090';
const FAULT_MODE = __ENV.AGENTGATE_FAULT_MODE || 'mock';
const FAULT_BACKEND = __ENV.AGENTGATE_FAULT_BACKEND || 'mock/primary';
const FAULT_KIND = __ENV.AGENTGATE_FAULT_KIND || 'error'; // error | timeout | reset
const FAULT_RATE = parseFloat(__ENV.AGENTGATE_FAULT_RATE || '1.0');
const TARGET_RPM = parseInt(__ENV.AGENTGATE_TARGET_RPM || '600', 10);

export const options = {
  scenarios: {
    failover: {
      executor: 'constant-arrival-rate',
      rate: TARGET_RPM,
      timeUnit: '1m',
      duration: __ENV.AGENTGATE_DURATION || '12m',
      preAllocatedVUs: parseInt(__ENV.AGENTGATE_PREALLOC_VUS || '60', 10),
      maxVUs: parseInt(__ENV.AGENTGATE_MAX_VUS || '200', 10),
      gracefulStop: '2m',
    },
  },
  thresholds: {
    // THE assertion. Zero tolerance.
    agentgate_should_have_failed_over: ['count==0'],

    // Availability must hold through the fault. Failover is the whole point.
    agentgate_availability_rate: ['rate>0.995'],

    // Requests should end up on the surviving backend. If nothing moved, the fault did
    // not land and the run proves nothing.
    agentgate_backend_moved: ['rate>0.5'],

    // Failover costs latency - a failed attempt is paid before the retry. Allow a
    // multiple of the SLO but not an unbounded one.
    agentgate_gateway_overhead_ms: [`p(95)<${SLO.gatewayOverheadP95Ms * 4}`],

    // Retry budget must hold: 25 percent of deadline per request, 10 percent of volume
    // fleet-wide (SPEC 3.3). More than 3 attempts on any request means the budget is
    // not being enforced.
    agentgate_attempts: ['p(99)<=3'],

    // Streams may only fail over BEFORE the first content byte (SPEC 2.5).
    agentgate_stream_failover_after_content: ['count==0'],
    agentgate_stream_contract_ok: ['rate>0.99'],

    agentgate_contract_headers_ok: ['rate>0.999'],
  },
};

const shouldHaveFailedOver = new Counter('agentgate_should_have_failed_over');
const backendMoved = new Rate('agentgate_backend_moved');
const streamFailoverAfterContent = new Counter('agentgate_stream_failover_after_content');
const failoverLatencyCost = new Trend('agentgate_failover_latency_cost_ms', true);
const noHealthyDuringFault = new Counter('agentgate_no_healthy_during_fault');

function applyFault() {
  if (FAULT_MODE === 'mock') {
    const res = http.post(
      `${MOCK}/admin/fault`,
      JSON.stringify({ kind: FAULT_KIND, rate: FAULT_RATE, target: 'primary' }),
      { headers: { 'content-type': 'application/json' }, tags: { name: 'fault_inject' } }
    );
    if (res.status !== 200 && res.status !== 204) {
      throw new Error(`could not apply fault to mockprovider: ${res.status} ${String(res.body).slice(0, 200)}`);
    }
    return;
  }
  if (FAULT_MODE === 'drain') {
    // Drain mode expects the operator to have drained the backend already, because k6
    // should not hold cluster credentials. Verify the intended state instead.
    console.warn(
      `FAULT_MODE=drain: ensure "agentctl backend drain --backend ${FAULT_BACKEND}" has been run ` +
        'before starting, and undrain it afterwards.'
    );
    return;
  }
  throw new Error(`unknown AGENTGATE_FAULT_MODE=${FAULT_MODE}`);
}

function clearFault() {
  if (FAULT_MODE === 'mock') {
    http.del(`${MOCK}/admin/fault`, null, { tags: { name: 'fault_clear' } });
  }
}

export function setup() {
  banner('failover');
  console.log(`fault: mode=${FAULT_MODE} kind=${FAULT_KIND} rate=${FAULT_RATE} backend=${FAULT_BACKEND}`);

  // Establish which backend serves normally, so "moved" means something concrete.
  const token = getToken('failover_setup');
  const { summary } = chat(token, { stream: false, maxTokens: 8, promptTokens: 100, scenario: 'failover' });
  const baselineBackend = summary.backend || 'unknown';
  console.log(`baseline backend before fault: ${baselineBackend} (provider ${summary.provider})`);

  applyFault();
  console.log('fault applied; traffic starts now');

  return { baselineBackend: baselineBackend, startedAt: new Date().toISOString() };
}

export default function (data) {
  const token = getToken('failover');
  const stream = shouldStream(0.4);

  const { res, summary } = chat(token, {
    stream: stream,
    scenario: 'failover',
    promptTokens: 300,
    maxTokens: 128,
    quiet: true,
    tags: { stream: String(stream) },
  });

  const code = errorCode(res);

  // --- The central assertion -------------------------------------------------
  // A provider_error or provider_timeout reaching the caller means we surfaced a
  // backend failure instead of moving past it. no_healthy_backend is different: it
  // means the pool was genuinely empty, which is a capacity finding, not a failover bug.
  if (res.status >= 500) {
    if (code === 'no_healthy_backend') {
      noHealthyDuringFault.add(1);
    } else {
      shouldHaveFailedOver.add(1, { code: code || `http_${res.status}`, stream: String(stream) });
      console.error(
        `5xx that should have failed over: status=${res.status} code=${code} ` +
          `attempts=${summary.attempts} backend=${summary.backend} request_id=${
            res.headers['X-Agentgate-Request-Id'] || res.headers['x-agentgate-request-id']
          }`
      );
    }
  }

  // --- Did traffic actually move? -------------------------------------------
  if (summary.success && summary.backend) {
    backendMoved.add(summary.backend !== data.baselineBackend);
  }

  // --- Failover cost ---------------------------------------------------------
  if (summary.attempts > 1) {
    failoverLatencyCost.add(res.timings.duration, { attempts: String(summary.attempts) });
  }

  check(res, {
    'failover: response is 2xx or an expected 4xx': (r) =>
      (r.status >= 200 && r.status < 300) || r.status === 429 || r.status === 403,
    'failover: attempts recorded on the response': () => summary.attempts >= 1,
    'failover: no request exceeded 3 attempts': () => summary.attempts <= 3,
  });

  // --- Streaming failover contract ------------------------------------------
  if (stream && summary.success) {
    const frames = checkStream(res, { expectUsage: true });
    if (frames.failover !== null && frames.contentChunks > 0) {
      // SPEC 2.5: after the first content byte the stream is never silently restarted.
      // A failover frame arriving after content has flowed is a contract violation.
      if (frames.failover.before_first_content === false) {
        streamFailoverAfterContent.add(1);
        console.error('stream failover frame emitted after first content byte');
      }
    }
    check(null, {
      'failover: a restarted stream still terminated with [DONE]': () =>
        frames.failover === null || frames.done,
    });
  }
}

export function teardown(data) {
  clearFault();
  console.log(`fault cleared. run started ${data.startedAt}`);
  if (FAULT_MODE === 'drain') {
    console.warn(`Remember: agentctl backend undrain --backend ${FAULT_BACKEND}`);
  }
}

export function handleSummary(data) {
  const m = data.metrics;
  const lines = [];
  lines.push('');
  lines.push('=== failover report ===');
  lines.push(`fault:                       ${FAULT_MODE}/${FAULT_KIND} at rate ${FAULT_RATE}`);
  lines.push('');
  lines.push(`5xx that should have failed over: ${countOf(m, 'agentgate_should_have_failed_over')}  <- must be 0`);
  lines.push(`503 no_healthy_backend:           ${countOf(m, 'agentgate_no_healthy_during_fault')}`);
  lines.push(`availability:                     ${ratePct(m, 'agentgate_availability_rate')}`);
  lines.push(`requests served by another backend: ${ratePct(m, 'agentgate_backend_moved')}`);
  lines.push('');
  lines.push(`attempts p95:                     ${stat(m, 'agentgate_attempts', 'p(95)')}`);
  lines.push(`attempts p99:                     ${stat(m, 'agentgate_attempts', 'p(99)')}  <- retry budget`);
  lines.push(`failed-over request duration p95: ${stat(m, 'agentgate_failover_latency_cost_ms', 'p(95)')} ms`);
  lines.push('');
  lines.push(`stream failover after content:    ${countOf(m, 'agentgate_stream_failover_after_content')}  <- must be 0`);
  lines.push(`stream contract ok:               ${ratePct(m, 'agentgate_stream_contract_ok')}`);
  lines.push('');
  lines.push('Interpretation:');
  lines.push('  Any non-zero "should have failed over" is a failure regardless of the rest.');
  lines.push('  "requests served by another backend" near zero means the fault did not land;');
  lines.push('  the run proves nothing and must be repeated.');
  lines.push('  no_healthy_backend above zero means the pool ran out of survivors - a capacity');
  lines.push('  finding for test/load/capacity-model.md, not a failover defect.');
  lines.push('');

  return {
    stdout: lines.join('\n'),
    'failover-summary.json': JSON.stringify(data, null, 2),
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
