// k6 load test for the ingest endpoint: the Phase 9 target is p99 under
// 50 ms at a fixed arrival rate.
//
//   k6 run -e URL=http://127.0.0.1:8080 -e PROJECT=1 -e KEY=pk_... loadtest/ingest.js
//
// Options (all -e): RATE requests per second (default 500), BATCH events
// per request (default 10, what an SDK flushes at once), DURATION (default
// 60s).
//
// It uses a constant *arrival* rate: k6 starts RATE requests every second
// whether or not earlier ones have answered. A closed loop (each user waits
// for its reply before sending again) slows down when the server slows
// down, which hides exactly the latency we want to measure.
import http from 'k6/http';
import { check } from 'k6';

const RATE = Number(__ENV.RATE || 500);
const BATCH = Number(__ENV.BATCH || 10);

export const options = {
  scenarios: {
    ingest: {
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: '1s',
      duration: __ENV.DURATION || '60s',
      // Starting VUs mid-test is slow and makes k6 miss start times, so
      // have plenty ready. Each one is idle most of the time.
      preAllocatedVUs: 200,
      maxVUs: 500,
    },
  },
  thresholds: {
    // The roadmap's "done when": ingest keeps p99 under 50 ms.
    http_req_duration: ['p(99)<50'],
    // Every request must be accepted; a 429 here means the queue is full.
    checks: ['rate>0.999'],
    // k6 couldn't start requests on time: the client, not the server, is the
    // bottleneck and the result says nothing. Raise maxVUs or use a bigger box.
    dropped_iterations: ['count==0'],
  },
  summaryTrendStats: ['avg', 'p(50)', 'p(95)', 'p(99)', 'max'],
};

const url = `${__ENV.URL}/api/v1/projects/${__ENV.PROJECT}/events`;
const params = { headers: { 'Content-Type': 'application/json', 'X-Juju-Key': __ENV.KEY } };

// Random v4 UUID, made locally so the test needs no network imports.
function uuidv4() {
  return 'xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx'.replace(/[xy]/g, (c) => {
    const r = (Math.random() * 16) | 0;
    return (c === 'x' ? r : (r & 0x3) | 0x8).toString(16);
  });
}

// A handful of distinct errors so the worker does real grouping work, not
// one issue over and over.
const kinds = ['TypeError', 'RangeError', 'NetworkError', 'SyntaxError', 'AbortError'];

function event(i) {
  const kind = kinds[i % kinds.length];
  return {
    event_id: uuidv4(),
    type: 'error',
    platform: 'javascript',
    timestamp: new Date().toISOString(),
    release: 'web@1.0.0',
    environment: 'loadtest',
    exception: {
      type: kind,
      message: `${kind} in checkout`,
      stacktrace: {
        frames: [
          { function: 'submit', filename: 'app.js', lineno: 40 + (i % kinds.length), colno: 7, in_app: true },
          { function: 'onClick', filename: 'button.js', lineno: 12, colno: 3, in_app: true },
        ],
      },
    },
    tags: { route: '/checkout' },
  };
}

export default function () {
  const events = [];
  for (let i = 0; i < BATCH; i++) events.push(event(__ITER * BATCH + i));
  const res = http.post(url, JSON.stringify({ sdk: { name: 'juju.k6', version: '0.0.0' }, events }), params);
  check(res, { 'accepted (202)': (r) => r.status === 202 });
}
