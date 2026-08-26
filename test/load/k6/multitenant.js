// multitenant.js - fair-share isolation with a runaway in the mix.
//
// Question it answers: when one agent misbehaves, do the others notice?
//
// Buckets are keyed tenant:team:agent:env (SPEC 3.4). The promise is that an agent
// exceeding its quota is limited, and that its excess load does not degrade anyone
// else. This test drives several agents with different quotas, plus one deliberately
// running at 20x its allocation, and asserts:
//
//   1. The runaway is limited - it does not simply get served.
//   2. Well-behaved agents are NOT limited - no collateral 429s.
//   3. Well-behaved agents' latency stays within SLO while the runaway runs.
//   4. No agent's cache entry is served to another agent or tenant.
//   5. Each agent's rate-limit headers reflect its own quota, not a shared pool.
//
// Assertion 3 is the one that catches the real defect. A platform can limit correctly
// and still let a runaway consume shared concurrency - that is the failure mode in the
// worked postmortem in docs/runbooks/postmortem-template.md.
//
//   k6 run test/load/k6/multitenant.js
//
// Requires a set of pre-registered load-test agents with distinct quotas. Provide them
// as JSON in AGENTGATE_AGENTS, or accept the defaults below and register them first.

import { check } from 'k6';
import { Counter, Rate, Trend } from 'k6/metrics';
import { SLO, banner, getToken, chat, errorCode } from './lib/common.js';

// Each entry: identity, client credentials, expected quota, and the rate to drive.
// The runaway is flagged so its 429s are expected rather than counted as collateral.
const AGENTS = JSON.parse(
  __ENV.AGENTGATE_AGENTS ||
    JSON.stringify([
      {
        key: 'tenant-a-high',
        identity: 'agent://fsclient/payments-risk/dispute-triage',
        tenant: 'fsclient',
        clientId: __ENV.AGENTGATE_CLIENT_ID_A || '',
        clientSecret: __ENV.AGENTGATE_CLIENT_SECRET_A || '',
        rpmQuota: 600,
        drivePct: 60,
        runaway: false,
      },
      {
        key: 'tenant-a-low',
        identity: 'agent://fsclient/payments-risk/kyc-summariser',
        tenant: 'fsclient',
        clientId: __ENV.AGENTGATE_CLIENT_ID_B || '',
        clientSecret: __ENV.AGENTGATE_CLIENT_SECRET_B || '',
        rpmQuota: 120,
        drivePct: 60,
        runaway: false,
      },
      {
        key: 'tenant-b',
        identity: 'agent://fsclient-markets/rates-analytics/curve-explainer',
        tenant: 'fsclient-markets',
        clientId: __ENV.AGENTGATE_CLIENT_ID_C || '',
        clientSecret: __ENV.AGENTGATE_CLIENT_SECRET_C || '',
        rpmQuota: 300,
        drivePct: 60,
        runaway: false,
      },
      {
        key: 'runaway',
        identity: 'agent://fsclient/payments-risk/loadgen-runaway',
        tenant: 'fsclient',
        clientId: __ENV.AGENTGATE_CLIENT_ID_R || '',
        clientSecret: __ENV.AGENTGATE_CLIENT_SECRET_R || '',
        rpmQuota: 60,
        drivePct: 2000, // 20x its quota - this is the point of the test
        runaway: true,
      },
    ])
);

function rateFor(a) {
  return Math.max(1, Math.round((a.rpmQuota * a.drivePct) / 100));
}

// One arrival-rate scenario per agent so each drives at its own independent rate.
function buildScenarios() {
  const s = {};
  for (let i = 0; i < AGENTS.length; i++) {
    const a = AGENTS[i];
    s[a.key] = {
      executor: 'constant-arrival-rate',
      rate: rateFor(a),
      timeUnit: '1m',
      duration: __ENV.AGENTGATE_DURATION || '15m',
      preAllocatedVUs: Math.max(5, Math.ceil(rateFor(a) / 10)),
      maxVUs: Math.max(20, Math.ceil(rateFor(a) / 2)),
      exec: 'drive',
      env: { AGENT_INDEX: String(i) },
      tags: { agent_key: a.key, runaway: String(a.runaway) },
      gracefulStop: '1m',
    };
  }
  return s;
}

export const options = {
  scenarios: buildScenarios(),
  thresholds: {
    // 1. The runaway must actually be limited.
    'agentgate_limited_rate{runaway:true}': ['rate>0.5'],

    // 2. Well-behaved agents must not be limited at all. Any 429 here is collateral
    //    damage and means isolation is not holding.
    agentgate_collateral_limited: ['count==0'],

    // 3. Well-behaved agents must keep meeting the SLO while the runaway runs. This is
    //    the assertion that catches shared-concurrency starvation.
    'agentgate_wellbehaved_duration_ms': [`p(95)<5000`],
    'agentgate_gateway_overhead_ms{runaway:false}': [`p(95)<${SLO.gatewayOverheadP95Ms}`],
    'agentgate_availability_rate{runaway:false}': [`rate>=${SLO.availability}`],

    // 4. Cache isolation. Any cross-agent or cross-tenant hit is a SEV1-class defect.
    agentgate_cross_agent_cache_hit: ['count==0'],

    // 5. Rate-limit headers must reflect each agent's own quota.
    agentgate_quota_header_mismatch: ['count==0'],

    agentgate_contract_headers_ok: ['rate>0.999'],
    agentgate_unexpected_5xx: ['count<10'],
  },
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max', 'count'],
};

const limitedRate = new Rate('agentgate_limited_rate');
const collateralLimited = new Counter('agentgate_collateral_limited');
const wellBehavedDuration = new Trend('agentgate_wellbehaved_duration_ms', true);
const runawayDuration = new Trend('agentgate_runaway_duration_ms', true);
const crossAgentCacheHit = new Counter('agentgate_cross_agent_cache_hit');
const quotaHeaderMismatch = new Counter('agentgate_quota_header_mismatch');
const servedRate = new Rate('agentgate_served_rate');

export function setup() {
  banner('multitenant');
  for (let i = 0; i < AGENTS.length; i++) {
    const a = AGENTS[i];
    console.log(
      `agent ${a.key}: quota ${a.rpmQuota} rpm, driving at ${rateFor(a)} rpm ` +
        `(${a.drivePct}% of quota)${a.runaway ? '   <- RUNAWAY' : ''}`
    );
  }
  console.log('');
  console.log('Pass requires: runaway limited, others untouched, others still within SLO.');
  return { startedAt: new Date().toISOString() };
}

export function drive() {
  const idx = parseInt(__ENV.AGENT_INDEX || '0', 10);
  const a = AGENTS[idx];

  const token = getToken(a.key, { clientId: a.clientId, clientSecret: a.clientSecret });

  // A prompt unique to this agent. If another agent ever receives a cached response to
  // it, the cache key is not tenant-scoped and assertion 4 fires.
  const marker = `cache-isolation-marker-${a.key}`;
  const prompt = `${marker}. Summarise the dispute in one sentence.`;

  const { res, summary } = chat(token, {
    stream: false,
    scenario: 'multitenant',
    prompt: prompt,
    maxTokens: 64,
    temperature: 0, // deterministic, so the exact cache is eligible
    cache: 'on', // exercise the cache path deliberately
    quiet: true,
    tags: { agent_key: a.key, runaway: String(a.runaway), tenant: a.tenant },
  });

  const code = errorCode(res);
  const limited = res.status === 429 && (code === 'rate_limited' || code === 'quota_exceeded');

  limitedRate.add(limited, { agent_key: a.key, runaway: String(a.runaway) });
  servedRate.add(summary.success, { agent_key: a.key, runaway: String(a.runaway) });

  // --- 2. Collateral damage -------------------------------------------------
  if (limited && !a.runaway) {
    collateralLimited.add(1, { agent_key: a.key });
    console.error(
      `collateral 429 for well-behaved agent ${a.key}: code=${code} ` +
        `remaining=${res.headers['X-Agentgate-Ratelimit-Remaining-Tokens'] || 'n/a'}`
    );
  }

  // --- 3. Latency isolation -------------------------------------------------
  if (summary.success) {
    if (a.runaway) {
      runawayDuration.add(res.timings.duration);
    } else {
      wellBehavedDuration.add(res.timings.duration, { agent_key: a.key });
    }
  }

  // --- 4. Cache isolation ---------------------------------------------------
  // A hit is fine - this agent has asked before. A hit whose content answers another
  // agent's marker prompt is not.
  if (summary.cache === 'hit' && summary.success) {
    let body = '';
    try {
      body = res.json('choices')[0].message.content || '';
    } catch (e) {
      body = '';
    }
    for (let j = 0; j < AGENTS.length; j++) {
      const other = AGENTS[j];
      if (other.key === a.key) continue;
      if (body.indexOf(`cache-isolation-marker-${other.key}`) !== -1) {
        crossAgentCacheHit.add(1, { got: a.key, from: other.key });
        console.error(
          `CACHE ISOLATION BREACH: agent ${a.key} (tenant ${a.tenant}) received content ` +
            `marked for ${other.key} (tenant ${other.tenant}). request=${
              res.headers['X-Agentgate-Request-Id'] || res.headers['x-agentgate-request-id']
            }`
        );
      }
    }
  }

  // --- 5. Quota headers reflect this agent's own limit ----------------------
  const limitHeader =
    res.headers['X-Agentgate-Ratelimit-Limit-Tokens'] ||
    res.headers['x-agentgate-ratelimit-limit-tokens'] ||
    '';
  if (limitHeader !== '' && a.expectedTpm) {
    const reported = parseFloat(limitHeader);
    if (Math.abs(reported - a.expectedTpm) > 1) {
      quotaHeaderMismatch.add(1, { agent_key: a.key });
      console.error(
        `quota header mismatch for ${a.key}: reported ${reported}, expected ${a.expectedTpm}`
      );
    }
  }

  check(res, {
    'response is 2xx or a limiting 429': (r) =>
      (r.status >= 200 && r.status < 300) || r.status === 429,
    '429 carries Retry-After': (r) =>
      r.status !== 429 || (r.headers['Retry-After'] || r.headers['retry-after'] || '') !== '',
    'response is attributed to a pool': () => summary.pool !== '',
  });
}

export function handleSummary(data) {
  const m = data.metrics;
  const lines = [];
  lines.push('');
  lines.push('=== multi-tenant fair share report ===');
  lines.push('');
  lines.push('Per agent (served / limited):');
  for (let i = 0; i < AGENTS.length; i++) {
    const a = AGENTS[i];
    lines.push(
      `  ${pad(a.key, 16)} quota ${pad(String(a.rpmQuota) + ' rpm', 10)} ` +
        `driven ${pad(String(rateFor(a)) + ' rpm', 10)} ` +
        `served ${ratePct(m, 'agentgate_served_rate', `agent_key:${a.key}`)} ` +
        `limited ${ratePct(m, 'agentgate_limited_rate', `agent_key:${a.key}`)}` +
        (a.runaway ? '   <- runaway' : '')
    );
  }
  lines.push('');
  lines.push('Isolation assertions');
  lines.push(`  collateral 429s on well-behaved agents: ${countOf(m, 'agentgate_collateral_limited')}  <- must be 0`);
  lines.push(`  cross-agent cache hits:                 ${countOf(m, 'agentgate_cross_agent_cache_hit')}  <- must be 0`);
  lines.push(`  quota header mismatches:                ${countOf(m, 'agentgate_quota_header_mismatch')}  <- must be 0`);
  lines.push('');
  lines.push('Latency isolation');
  lines.push(`  well-behaved request duration p95: ${stat(m, 'agentgate_wellbehaved_duration_ms', 'p(95)')} ms`);
  lines.push(`  well-behaved request duration p99: ${stat(m, 'agentgate_wellbehaved_duration_ms', 'p(99)')} ms`);
  lines.push(`  runaway request duration p95:      ${stat(m, 'agentgate_runaway_duration_ms', 'p(95)')} ms`);
  lines.push('');
  lines.push('Interpretation:');
  lines.push('  The runaway being limited is necessary but not sufficient. The result that');
  lines.push('  matters is the well-behaved p95: if it rose while the runaway ran, the agents');
  lines.push('  are sharing something the quota does not govern - concurrency, a connection');
  lines.push('  pool, or a Redis hot key. Compare against a baseline run with the runaway');
  lines.push('  scenario removed.');
  lines.push('');
  lines.push('  Any cross-agent cache hit is a SEV1. Stop, preserve the run output, and follow');
  lines.push('  docs/runbooks/cache-poisoning-suspected.md.');
  lines.push('');

  return {
    stdout: lines.join('\n'),
    'multitenant-summary.json': JSON.stringify(data, null, 2),
  };
}

function pad(s, n) {
  let out = String(s);
  while (out.length < n) out += ' ';
  return out;
}
function countOf(m, name) {
  return m[name] && m[name].values ? m[name].values.count : 0;
}
function ratePct(m, name, tag) {
  const key = tag ? `${name}{${tag}}` : name;
  const entry = m[key] || m[name];
  return entry && entry.values ? (entry.values.rate * 100).toFixed(1) + '%' : 'n/a';
}
function stat(m, name, s) {
  return m[name] && m[name].values ? (Math.round(m[name].values[s] * 100) / 100).toString() : 'n/a';
}
