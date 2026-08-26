// smoke.js - minimal correctness pass.
//
// Question it answers: is this deployment wired up correctly enough to be worth load
// testing? One VU, a handful of iterations, every endpoint touched once, every contract
// assertion exercised at least once.
//
// Run it before every other script in this directory. A smoke failure means the
// remaining results would be measuring something broken.
//
//   k6 run test/load/k6/smoke.js
//
// Exits non-zero on any contract violation, so it is safe to use as a deployment gate.

import { check, sleep, group } from 'k6';
import {
  CFG,
  banner,
  getToken,
  chat,
  embeddings,
  listModels,
  tokenCount,
  health,
  checkStream,
  errorCode,
} from './lib/common.js';

export const options = {
  scenarios: {
    smoke: {
      executor: 'per-vu-iterations',
      vus: 1,
      iterations: 5,
      maxDuration: '3m',
    },
  },
  thresholds: {
    // A smoke test tolerates nothing. Any failure is a stop.
    checks: ['rate==1.0'],
    agentgate_contract_headers_ok: ['rate==1.0'],
    agentgate_stream_contract_ok: ['rate==1.0'],
    agentgate_success_rate: ['rate==1.0'],
    http_req_failed: ['rate==0.0'],
  },
};

export function setup() {
  banner('smoke');

  const live = health('/healthz');
  const ready = health('/readyz');
  const ok = check(null, {
    'healthz returns 200': () => live.status === 200,
    'readyz returns 200': () => ready.status === 200,
  });
  if (!ok) {
    throw new Error(`gateway not ready: healthz=${live.status} readyz=${ready.status}`);
  }
  return { startedAt: new Date().toISOString() };
}

export default function () {
  const token = getToken('smoke');

  group('models', function () {
    const res = listModels(token);
    check(res, {
      'GET /v1/models returns 200': (r) => r.status === 200,
      'GET /v1/models returns a data array': (r) => {
        try {
          return Array.isArray(r.json('data'));
        } catch (e) {
          return false;
        }
      },
      'entitled models include the pool under test': (r) => {
        try {
          const ids = r.json('data').map((m) => m.id);
          return ids.indexOf(CFG.model) !== -1;
        } catch (e) {
          return false;
        }
      },
    });
  });

  group('token-count', function () {
    const res = tokenCount(token, { promptTokens: 200 });
    check(res, {
      'POST /v1/token-count returns 200': (r) => r.status === 200,
      'token-count returns a positive estimate': (r) => {
        try {
          return r.json('input_tokens') > 0;
        } catch (e) {
          return false;
        }
      },
    });
  });

  group('chat unary', function () {
    const { res, summary } = chat(token, {
      stream: false,
      maxTokens: 32,
      promptTokens: 200,
      scenario: 'smoke',
    });
    check(res, {
      'unary chat returns 200': (r) => r.status === 200,
      'unary chat returns a choice': (r) => {
        try {
          return r.json('choices').length > 0;
        } catch (e) {
          return false;
        }
      },
      'unary chat reports usage': (r) => {
        try {
          return r.json('usage.total_tokens') > 0;
        } catch (e) {
          return false;
        }
      },
      'unary chat attributed a provider': () => summary.provider !== '',
      'unary chat attributed a cost': () => summary.costUsd >= 0,
      'unary chat made at least one attempt': () => summary.attempts >= 1,
    });
  });

  group('chat streaming', function () {
    const { res } = chat(token, {
      stream: true,
      maxTokens: 64,
      promptTokens: 200,
      scenario: 'smoke',
    });
    check(res, { 'streaming chat returns 200': (r) => r.status === 200 });
    const frames = checkStream(res, { expectUsage: true });
    check(null, {
      'stream reported completion tokens in the usage frame': () =>
        frames.usage !== null &&
        frames.usage.usage !== undefined &&
        frames.usage.usage.completion_tokens > 0,
      'stream reported a cost in the usage frame': () =>
        frames.usage !== null && frames.usage.cost_usd !== undefined,
      'stream reported a finish reason': () => frames.finishReasons.length > 0,
    });
  });

  group('embeddings', function () {
    const { res, summary } = embeddings(token, { promptTokens: 100, scenario: 'smoke' });
    check(res, {
      'embeddings returns 200': (r) => r.status === 200,
      'embeddings returns a vector': (r) => {
        try {
          return r.json('data')[0].embedding.length > 0;
        } catch (e) {
          return false;
        }
      },
      'embeddings attributed a pool': () => summary.pool !== '',
    });
  });

  group('error contract', function () {
    // A deliberately unknown logical model must produce 404 unknown_model with the
    // full problem+json shape AND the standard headers. Error paths are where the
    // contract regresses, because nobody exercises them by accident.
    const { res } = chat(token, {
      model: 'this-model-does-not-exist',
      stream: false,
      maxTokens: 8,
      promptTokens: 50,
      scenario: 'smoke',
      quiet: true,
    });
    check(res, {
      'unknown model returns 404': (r) => r.status === 404,
      'unknown model returns code unknown_model': (r) => errorCode(r) === 'unknown_model',
      'error body is problem+json shaped': (r) => {
        try {
          const b = r.json();
          return (
            typeof b.type === 'string' &&
            typeof b.title === 'string' &&
            b.status === 404 &&
            typeof b.code === 'string' &&
            typeof b.request_id === 'string' &&
            typeof b.trace_id === 'string'
          );
        } catch (e) {
          return false;
        }
      },
      'error response still carries the request id header': (r) =>
        (r.headers['X-Agentgate-Request-Id'] || r.headers['x-agentgate-request-id'] || '') !== '',
    });
  });

  sleep(1);
}

export function teardown(data) {
  console.log(`smoke complete, started ${data.startedAt}`);
}
