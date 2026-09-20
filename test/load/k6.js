// Janus gateway load-test harness (k6).
//
// Drives the /v1 proxy surface with bearer tokens across modalities and
// encodes the scale acceptance criteria as k6 thresholds, so a run exits
// non-zero when the deployment misses them:
//
//   * 200 concurrent users        -> USERS (pre-allocated VUs)
//   * 10,000 requests / minute    -> RATE_PER_MINUTE (constant arrival rate)
//   * < 100 ms p95 proxy overhead -> janus_overhead_ms threshold (needs
//                                    BASELINE_URL, see README.md)
//   * < 1% failed requests        -> http_req_failed threshold
//
// BASELINE_URL is REQUIRED by default: without it the overhead threshold
// cannot be measured and the run would pass while an acceptance criterion
// goes untested. Waive it explicitly with SKIP_OVERHEAD=1 (smoke runs only).
//
// See test/load/README.md for setup and the exact commands.

import http from 'k6/http';
import { check, fail } from 'k6';
import { Trend } from 'k6/metrics';

const BASE = (__ENV.JANUS_URL || 'http://127.0.0.1:8080').replace(/\/$/, '');
const TOKEN = __ENV.JANUS_TOKEN || '';
const MODEL = __ENV.JANUS_MODEL || 'example-model';
const EMBED_MODEL = __ENV.JANUS_EMBED_MODEL || MODEL;
const DURATION = __ENV.DURATION || '5m';
const USERS = Number(__ENV.USERS || '200');
const RATE_PER_MINUTE = Number(__ENV.RATE_PER_MINUTE || '10000');

// Optional gateway-overhead measurement: the same request is sent to the
// upstream directly and through Janus; the paired difference is the overhead
// the gateway adds (network variance cancels out over the run).
const BASELINE_URL = (__ENV.BASELINE_URL || '').replace(/\/$/, '');
const BASELINE_TOKEN = __ENV.BASELINE_TOKEN || '';
const SKIP_OVERHEAD = __ENV.SKIP_OVERHEAD === '1';

const overheadMs = new Trend('janus_overhead_ms');

const scenarios = {
  sustained: {
    executor: 'constant-arrival-rate',
    rate: RATE_PER_MINUTE,
    timeUnit: '1m',
    duration: DURATION,
    preAllocatedVUs: USERS,
    maxVUs: USERS * 2,
    exec: 'proxyTraffic',
  },
};

if (BASELINE_URL) {
  scenarios.overhead = {
    executor: 'constant-vus',
    vus: 5,
    duration: DURATION,
    exec: 'overheadPair',
  };
}

export const options = {
  scenarios,
  thresholds: {
    // Pass bar: fewer than 1% of proxied requests fail under sustained load.
    'http_req_failed{scenario:sustained}': ['rate<0.01'],
    // Pass bar: the gateway adds < 100ms at p95 (only measured with a baseline).
    ...(BASELINE_URL ? { janus_overhead_ms: ['p(95)<100'] } : {}),
  },
  // Streamed bodies are read fully; no need to persist them.
  discardResponseBodies: false,
};

function authHeaders(token) {
  return {
    headers: {
      'Content-Type': 'application/json',
      Authorization: `Bearer ${token}`,
    },
  };
}

const chatBody = JSON.stringify({
  model: MODEL,
  messages: [{ role: 'user', content: 'Summarize the quarterly report in one sentence.' }],
});

const streamBody = JSON.stringify({
  model: MODEL,
  stream: true,
  messages: [{ role: 'user', content: 'Stream a short greeting.' }],
});

const embedBody = JSON.stringify({
  model: EMBED_MODEL,
  input: 'The quick brown fox jumps over the lazy dog.',
});

export function setup() {
  if (!BASELINE_URL) {
    if (!SKIP_OVERHEAD) {
      fail(
        'BASELINE_URL is unset: the < 100ms p95 gateway-overhead threshold cannot be ' +
          'measured, so this run would exit green while an acceptance criterion goes ' +
          'entirely untested. Set BASELINE_URL (see test/load/README.md) or waive the ' +
          'check explicitly with SKIP_OVERHEAD=1 (smoke runs only).',
      );
    }
    console.warn('!!! =================================================================');
    console.warn('!!! SKIP_OVERHEAD=1 — the < 100ms p95 overhead threshold is NOT measured.');
    console.warn('!!! This run does NOT verify the gateway-overhead acceptance criterion.');
    console.warn('!!! =================================================================');
  }
  if (!TOKEN) {
    fail('JANUS_TOKEN is required: create one via POST /api/v1/tokens (see test/load/README.md)');
  }
  // One canary request so a misconfigured run aborts immediately instead of
  // recording five minutes of 401s.
  const res = http.post(`${BASE}/v1/chat/completions`, chatBody, authHeaders(TOKEN));
  if (res.status !== 200) {
    fail(`canary request returned ${res.status}: ${res.body}`);
  }
}

// proxyTraffic reproduces a realistic modality mix on every arrival.
export function proxyTraffic() {
  const roll = Math.random();
  let res;
  if (roll < 0.6) {
    res = http.post(`${BASE}/v1/chat/completions`, chatBody, authHeaders(TOKEN));
  } else if (roll < 0.75) {
    res = http.post(`${BASE}/v1/chat/completions`, streamBody, authHeaders(TOKEN));
  } else if (roll < 0.95) {
    res = http.post(`${BASE}/v1/embeddings`, embedBody, authHeaders(TOKEN));
  } else {
    res = http.get(`${BASE}/v1/models`, authHeaders(TOKEN));
  }
  check(res, { 'status is 200': (r) => r.status === 200 });
}

// overheadPair sends the same request directly to the upstream and through
// the gateway, recording the difference. Requires BASELINE_URL (+ optionally
// BASELINE_TOKEN for the upstream's own auth).
export function overheadPair() {
  const direct = http.post(
    `${BASELINE_URL}/v1/chat/completions`,
    chatBody,
    authHeaders(BASELINE_TOKEN || TOKEN),
  );
  const proxied = http.post(`${BASE}/v1/chat/completions`, chatBody, authHeaders(TOKEN));
  if (direct.status === 200 && proxied.status === 200) {
    overheadMs.add(Math.max(0, proxied.timings.duration - direct.timings.duration));
  }
}
