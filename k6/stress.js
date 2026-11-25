import exec from 'k6/execution';
import http from 'k6/http';
import { Counter, Trend } from 'k6/metrics';
import { sleep } from 'k6';

const baseURL = 'http://server:8080';
const mode = __ENV.STRESS_TARGETS || 'same';
if (mode !== 'same' && mode !== 'different') {
  throw new Error('STRESS_TARGETS must be same or different.');
}

const phaseSeconds = 12;
const phases = [
  { name: 'normal', reads: 10, jobs: 1 },
  { name: 'elevated', reads: 30, jobs: 4 },
  { name: 'peak', reads: 60, jobs: 8 },
  { name: 'recovery', reads: 10, jobs: 1 },
];
const jobResults = new Counter('migration_job_results');
const completions = new Counter('migration_job_completions');
const queueWait = new Trend('migration_queue_wait_ms', true);
const migrationDuration = new Trend('migration_duration_ms', true);
const outcomes = ['accepted', 'approval_error', 'submission_error', 'failed', 'succeeded'];

const expected = Object.fromEntries([200, 201, 202, 204].map((status) => [status, http.expectedStatuses(status)]));
const scenarios = {};
const thresholds = { dropped_iterations: ['count==0'] };

for (const [index, phase] of phases.entries()) {
  const window = {
    executor: 'constant-arrival-rate',
    timeUnit: '1s',
    startTime: (phaseSeconds * index) + 's',
    duration: phaseSeconds + 's',
    tags: { phase: phase.name },
  };
  const readSlots = Math.max(4, Math.ceil(phase.reads / 4));
  const jobSlots = Math.max(4, phase.jobs * 4);
  scenarios[phase.name + '_read'] = {
    ...window, exec: 'readAPI', rate: phase.reads, preAllocatedVUs: readSlots, gracefulStop: '5s',
  };
  scenarios[phase.name + '_job'] = {
    ...window, exec: 'runMigration', rate: phase.jobs, preAllocatedVUs: jobSlots, gracefulStop: '20s',
  };

  const apiTags = '{phase:' + phase.name + ',traffic:measured}';
  thresholds['http_req_duration' + apiTags] = ['p(95)<500'];
  thresholds['http_req_failed' + apiTags] = ['rate<0.01'];
  // Daftarkan submetrik supaya ringkasan k6 menyertakan nilai per fase.
  thresholds['http_reqs' + apiTags] = ['count>0'];
  thresholds['migration_queue_wait_ms{phase:' + phase.name + '}'] = ['min>=0'];
  thresholds['migration_duration_ms{phase:' + phase.name + '}'] = ['min>=0'];
  thresholds['migration_job_completions{completion_phase:' + phase.name + '}'] = ['count>=0'];
  for (const result of outcomes) {
    thresholds['migration_job_results{phase:' + phase.name + ',result:' + result + '}'] = ['count>=0'];
  }
}
thresholds['migration_job_completions{completion_phase:drain}'] = ['count>=0'];

export const options = {
  scenarios,
  thresholds,
  setupTimeout: '3m',
  summaryTrendStats: ['med', 'p(95)'],
  systemTags: ['status', 'method', 'name', 'scenario', 'expected_response'],
};

function request(method, path, name, status, session, body, traffic = 'support') {
  const headers = { Accept: 'application/json', Origin: baseURL };
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (session?.csrf) headers['X-CSRF-Token'] = session.csrf;

  return http.request(method, baseURL + path, body === undefined ? null : JSON.stringify(body), {
    headers,
    cookies: session ? { platform_session: { value: session.cookie, replace: true } } : {},
    responseCallback: expected[status],
    tags: { name, traffic },
    timeout: '3s',
    redirects: 0,
  });
}

function requireStatus(response, status, name) {
  if (response.status !== status) {
    throw new Error(name + ' returned HTTP ' + response.status + ', expected ' + status);
  }
  return response;
}

function login() {
  // Login mencabut sesi dari cookie lama. Hapus cookie lokal sebelum membuat sesi baru.
  http.cookieJar().clear(baseURL);
  const credentials = { username: 'stress_admin', password: 'if3110-stress-admin' };
  const response = requireStatus(request('POST', '/auth/login', 'login', 200, null, credentials), 200, 'login');
  const userID = response.json('user.id');
  const cookie = response.cookies.platform_session?.[0]?.value;
  if (!userID || !cookie) throw new Error('Login did not return a user and session cookie');

  const csrf = requireStatus(request('GET', '/auth/csrf', 'csrf', 200, { cookie }), 200, 'csrf').json('csrf_token');
  if (!csrf) throw new Error('Login did not return a CSRF token');

  return { userID, cookie, csrf };
}

function databasePath(databaseID) {
  return '/databases/' + databaseID;
}

export function setup() {
  const admin = login();
  const runLabel = 'k6 ' + mode + ' ' + Date.now();
  const databases = ['stress_a', 'stress_b'].map((database) => {
    const connection = {
      name: runLabel + ' ' + database, dbms: 'postgres', host: 'postgres', port: 5432,
      database_name: database, username: 'stress_target', password: 'if3110-stress-target', ssl_mode: 'disable',
    };
    return requireStatus(
      request('POST', '/admin/databases', 'register target', 201, admin, connection), 201, 'register target',
    ).json('database.id');
  });
  const group = requireStatus(
    request('POST', '/admin/groups', 'create group', 201, admin, { name: runLabel }), 201, 'create group',
  ).json('group.id');
  const groupPath = '/admin/groups/' + group;
  requireStatus(request('PUT', groupPath + '/members/' + admin.userID, 'add member', 204, admin, {}), 204, 'add member');
  requireStatus(
    request('PUT', groupPath + '/leader', 'assign leader', 204, admin, { user_id: admin.userID }), 204, 'assign leader',
  );

  for (const databaseID of databases) {
    const path = groupPath + '/databases/' + databaseID;
    requireStatus(request('PUT', path, 'grant edit', 204, admin, { permission: 'view_edit' }), 204, 'grant edit');
  }

  // Siapkan definisi unik lebih dulu. Review dan approval memakai riwayat terbaru saat VU mulai.
  const definitions = {};
  for (const phase of phases) {
    // Sediakan satu definisi cadangan untuk iterasi yang dimulai di batas akhir fase.
    definitions[phase.name] = Array.from({ length: phase.jobs * phaseSeconds + 1 }, (_, index) => {
      const databaseID = databases[mode === 'same' ? 0 : index % databases.length];
      const path = databasePath(databaseID) + '/migrations/definitions';
      const definition = {
        name: phase.name + ' job ' + (index + 1),
        sql_up: 'WITH pause AS MATERIALIZED (SELECT pg_sleep(0.35)) '
          + 'UPDATE public.k6_counters SET value = value + 1 FROM pause WHERE id = 1;',
        sql_down: 'UPDATE public.k6_counters SET value = value - 1 WHERE id = 1;',
      };
      const response = requireStatus(
        request('POST', path, 'create definition', 201, admin, definition), 201, 'create definition',
      );
      return { databaseID, migrationID: response.json('migration.id') };
    });
  }

  // Setiap VU memiliki sesi sendiri agar penguncian satu sesi tidak mendominasi pengukuran.
  const sessionCount = Object.values(scenarios).reduce((total, scenario) => total + scenario.preAllocatedVUs, 0);
  const sessions = Array.from({ length: sessionCount }, () => login());
  return { databases, definitions, sessions };
}

export function readAPI({ databases, sessions }) {
  const database = databases[exec.scenario.iterationInTest % databases.length];
  const path = databasePath(database) + '/schemas/public/tables/k6_counters/data?limit=10';
  request('GET', path, 'read table data', 200, sessions[exec.vu.idInTest - 1], undefined, 'measured');
}

export function runMigration({ definitions, sessions }) {
  const phaseName = exec.scenario.name.replace(/_job$/, '');
  const fixture = definitions[phaseName][exec.scenario.iterationInTest];
  if (!fixture) exec.test.abort('No migration definition available for the scheduled iteration.');
  const session = sessions[exec.vu.idInTest - 1];
  const database = databasePath(fixture.databaseID);
  const migration = database + '/migrations/definitions/' + fixture.migrationID;
  const result = (name) => jobResults.add(1, { result: name });

  const review = request('POST', migration + '/review', 'review apply', 201, session, { action: 'apply' });
  if (review.status !== 201) {
    result('approval_error');
    return;
  }
  const workflow = review.json('workflow.id');
  const approval = request('POST', database + '/migrations/workflows/' + workflow + '/approvals', 'approve', 200, session, {});
  if (approval.status !== 200 || approval.json('workflow.status') !== 'approved') {
    result('approval_error');
    return;
  }

  const payload = { action: 'apply', approval_id: workflow };
  const submitted = request('POST', migration + '/actions', 'queue approved job', 202, session, payload, 'measured');
  if (submitted.status !== 202) {
    result('submission_error');
    return;
  }
  result('accepted');

  const job = waitForJob(database + '/jobs/' + submitted.json('job_id'), session);
  if (!job) return;

  const started = Date.parse(job.started_at);
  const finished = Date.parse(job.finished_at);
  const queued = Date.parse(job.created_at);
  if (Number.isFinite(started) && Number.isFinite(queued)) queueWait.add(started - queued);

  if (job.status !== 'succeeded') {
    result('failed');
    return;
  }
  result('succeeded');
  migrationDuration.add(finished - started);
  const phaseIndex = phases.findIndex((phase) => phase.name === phaseName);
  const runStart = exec.scenario.startTime - phaseIndex * phaseSeconds * 1000;
  const completionIndex = Math.floor((finished - runStart) / (phaseSeconds * 1000));
  completions.add(1, { completion_phase: phases[completionIndex]?.name ?? 'drain' });
}

function waitForJob(path, session) {
  const deadline = Date.now() + 15000;
  while (Date.now() < deadline) {
    const response = request('GET', path, 'read job status', 200, session);
    if (response.status === 200) {
      const job = response.json('job');
      if (['succeeded', 'failed', 'reverted', 'partial', 'partially_reverted'].includes(job.status)) return job;
    }
    sleep(0.4);
  }
  return null;
}

export function handleSummary({ metrics }) {
  function value(metric, stat) {
    const number = metrics[metric]?.values?.[stat];
    return Number.isFinite(number) ? Math.round(number * 1000) / 1000 : null;
  }

  const results = phases.map((phase) => {
    const apiTags = '{phase:' + phase.name + ',traffic:measured}';
    const stageTags = '{phase:' + phase.name + '}';
    const errorRate = metrics['http_req_failed' + apiTags]?.values?.rate ?? 0;
    const count = (result) => value('migration_job_results{phase:' + phase.name + ',result:' + result + '}', 'count') ?? 0;
    const jobsCompleted = value('migration_job_completions{completion_phase:' + phase.name + '}', 'count') ?? 0;
    const accepted = count('accepted');
    const succeeded = count('succeeded');
    const failed = count('failed');

    return {
      phase: phase.name,
      api_p50_ms: value('http_req_duration' + apiTags, 'med'),
      api_p95_ms: value('http_req_duration' + apiTags, 'p(95)'),
      api_requests_per_second: Number((value('http_reqs' + apiTags, 'count') / phaseSeconds).toFixed(2)),
      api_error_percent: Number((errorRate * 100).toFixed(3)),
      approvals_failed: count('approval_error'),
      submissions_rejected: count('submission_error'),
      jobs_accepted: accepted,
      jobs_succeeded: succeeded,
      jobs_failed: failed,
      jobs_unfinished: accepted - succeeded - failed,
      queue_wait_p95_ms: value('migration_queue_wait_ms' + stageTags, 'p(95)'),
      migration_success_p95_ms: value('migration_duration_ms' + stageTags, 'p(95)'),
      jobs_completed_per_second: Number((jobsCompleted / phaseSeconds).toFixed(2)),
    };
  });

  return {
    stdout: '\n' + JSON.stringify({
      mode,
      phase_seconds: phaseSeconds,
      dropped_iterations: value('dropped_iterations', 'count') ?? 0,
      successful_jobs_during_drain: value('migration_job_completions{completion_phase:drain}', 'count') ?? 0,
      phases: results,
    }, null, 2) + '\n',
  };
}
