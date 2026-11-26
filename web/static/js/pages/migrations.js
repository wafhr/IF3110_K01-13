import {
  accessibleDatabase, actorLabel, api, apiDatabase, databaseIdFrom, esc, formData, heading, notice, pageDatabase, panel, root, route,
  shell, state, statusNotice, table,
} from '../core.js';

const settledJobs = ['succeeded', 'reverted', 'failed', 'partial', 'partially_reverted'];
const stoppedJobs = ['partial', 'partially_reverted'];
const activeJobIDs = (jobs) => jobs.filter((j) => !settledJobs.includes(j.status)).map((j) => j.id);
const time = (value) => new Date(value).toLocaleString();
const jobHeaders = ['Job ID', 'Migration', 'Action', 'Status', 'Progress', 'Attempts', 'Last error', 'Actions'];
const analyzeWarning = 'EXPLAIN ANALYZE executes the SQL. Transactional changes are rolled back, but sequence and external effects may persist. Continue?';
const attemptAction = (a) => (a.operation === 'compensate' ? 'compensate' : a.action);

const definitionForm = `
  <form id="migration-form" class="stack">
    <input name="id" type="hidden"><input name="expected_revision" type="hidden">
    <label>Name<input name="name" maxlength="160" required></label>
    <label>SQL up<textarea name="sql_up" rows="7" required maxlength="1048576"></textarea></label>
    <label>SQL down<textarea name="sql_down" rows="7" maxlength="1048576"></textarea></label>
    <label class="checkbox-label"><input name="irreversible" type="checkbox"> Irreversible</label>
    <label>Reason<textarea name="irreversible_reason" rows="2"></textarea></label>
    <details>
      <summary>Batch configuration (optional)</summary>
      <div class="form-row">
        <label>Table<input name="batch_table" placeholder="public.accounts"></label>
        <label>Key column<input name="batch_key_column" placeholder="id"></label>
        <label>Batch size<input name="batch_size" type="number" min="1" max="100000"></label>
      </div>
      <p class="muted">Batch SQL must use $1 and $2 for the first and last key in each range.</p>
    </details>
    <div class="form-row">
      <button class="button primary">Save definition</button>
      <button class="button" type="button" id="reset-migration">New definition</button>
    </div>
  </form>
  <details class="schema-diff-tool">
    <summary>Compare with desired schema and generate SQL</summary>
    <label>Desired schema JSON<textarea id="desired-schema" rows="8" spellcheck="false">{"tables":[{"schema":"public","name":"products","columns":[{"name":"id","type":"bigint","nullable":false},{"name":"title","type":"varchar(200)","nullable":false}]}]}</textarea></label>
    <button class="button" id="compare-schema" type="button">Compare schemas</button>
    <div id="schema-diff-results" aria-live="polite"></div>
  </details>
  <section class="subsection">
    <h3>Execution plan preview</h3>
    <label>SQL<textarea id="plan-sql" rows="5" placeholder="Enter one supported SQL statement"></textarea></label>
    <label class="checkbox-label"><input id="plan-analyze" type="checkbox"> EXPLAIN ANALYZE (executes SQL, then rolls back transactional changes)</label>
    <button class="button" id="preview-plan" type="button">Preview plan</button>
    <div id="plan-results"></div>
  </section>`;

function definitionRow(m, isEdit) {
  const name = isEdit ? `<button class="button small select-migration" data-id="${esc(m.id)}">${esc(m.name)}</button>` : esc(m.name);
  const reviews = isEdit
    ? ['apply', 'rollback', 'forward']
      .map((action) => ` <button class="button small action" data-action="${action}" data-id="${esc(m.id)}">Review ${action}</button>`)
      .join('')
    : '';
  return `
    <tr>
      <td>${name}</td><td>${esc(m.current_revision)}</td><td>${m.frozen_at ? 'Frozen' : 'Draft'}</td>
      <td><button class="button small workflow-status" data-id="${esc(m.id)}">Review status</button>${reviews}</td>
    </tr>`;
}

const definitionTable = (migrations, isEdit) =>
  table(['Migration', 'Revision', 'State', 'Actions'], migrations.map((m) => definitionRow(m, isEdit)).join(''));

// historyTable lists committed executions, Milestone 1 attempts, and every
// attempt that did not commit, newest first.
function historyTable(executions, legacy, attempts) {
  const row = (at, cells) => ({ at, html: `<tr>${cells.map((c) => `<td>${c}</td>`).join('')}</tr>` });
  const entries = [
    ...executions.map((x) => row(x.started_at, [
      esc(x.sequence), esc(x.name), esc(x.action), esc(x.status),
      `<button class="button small execution-detail" data-id="${esc(x.id)}">${esc(x.id)}</button>`,
      esc(time(x.started_at)), esc(actorLabel(x.executed_by)),
    ])),
    ...legacy.map((x) => row(x.started_at, [
      esc(x.version), esc(x.name), 'legacy', esc(x.status), '—', esc(time(x.started_at)), esc(actorLabel(x.executed_by)),
    ])),
    ...attempts.filter((a) => a.status !== 'succeeded').map((a) => row(a.started_at, [
      '—', esc(a.name), esc(attemptAction(a)), `${esc(a.status)} · attempt ${esc(a.attempt)}`,
      `<button class="button small attempt-detail" data-id="${esc(a.id)}">${esc(a.execution_id)}</button>`,
      esc(time(a.started_at)), esc(actorLabel(a.actor_id)),
    ])),
  ];
  entries.sort((a, b) => new Date(b.at) - new Date(a.at));
  return table(['Sequence', 'Migration', 'Action', 'Status', 'Execution ID', 'Started', 'Actor'], entries.map((e) => e.html).join(''));
}

function jobRow(j, isEdit) {
  const statusClass = ['succeeded', 'reverted'].includes(j.status) ? 'active' : j.status === 'failed' ? 'inactive' : '';
  const progress = j.progress || {};
  let actions = '';
  if (isEdit && stoppedJobs.includes(j.status)) {
    actions = `<button class="button small resume-job" data-id="${esc(j.id)}">Resume</button>`;
    if (j.status === 'partial') actions += ` <button class="button small cancel-job" data-id="${esc(j.id)}">Compensate</button>`;
  }
  return `
    <tr>
      <td><code>${esc(j.id)}</code></td><td>${esc(j.name)}</td><td>${esc(j.action)}</td>
      <td><span class="state ${statusClass}">${esc(j.status)}</span></td>
      <td>${progress.committed_rows ?? 0} / ${progress.total_rows ?? 0} rows · ${progress.committed_batches ?? 0} batches</td>
      <td>${j.attempts}</td><td>${esc(j.last_error || '')}</td><td>${actions}</td>
    </tr>`;
}

const executionDetail = (x) => `
  <h3>${esc(x.name)} · ${esc(x.action)} · ${esc(x.status)}</h3>
  <p>Execution <code>${esc(x.id)}</code> · revision ${esc(x.revision)} · actor ${esc(actorLabel(x.executed_by))}${
    x.reverts_execution_id ? ` · reverts ${esc(x.reverts_execution_id)}` : ''}</p>
  <pre>${esc(x.sql)}</pre>`;

const attemptDetail = (a) => `
  <h3>${esc(a.name)} · ${esc(attemptAction(a))} · ${esc(a.status)}</h3>
  <p>Execution <code>${esc(a.execution_id)}</code> · attempt ${esc(a.attempt)} · revision ${esc(a.revision)} ·
    actor ${esc(actorLabel(a.actor_id))} · ${esc(time(a.started_at))} – ${esc(time(a.finished_at))}${
    a.committed_batches ? ` · ${esc(a.committed_batches)} batches / ${esc(a.committed_rows)} rows committed` : ''}</p>
  ${a.error ? `<p class="notice error">${esc(a.error)}</p>` : ''}
  <pre>${esc(a.sql)}</pre>`;

function workflowView(workflow, isEdit) {
  const approvals = workflow.approvals || [];
  const approval = (a) => {
    if (a.approved) return 'Approved';
    const mine = a.leader_id === state.user.id;
    const label = mine ? 'Approve' : 'Awaiting group leader';
    return `<button class="button small approve-group" data-id="${esc(workflow.id)}" ${mine ? '' : 'disabled'}>${label}</button>`;
  };
  const rows = approvals.map((a) => `
    <tr><td>${esc(a.group_name)}</td><td>${esc(a.permission)}</td>
    <td>${esc(a.leader_id ? actorLabel(a.leader_id) : 'Not assigned')}</td><td>${approval(a)}</td></tr>`).join('');
  return `
    <p><strong>${esc(workflow.action)}</strong> · ${esc(workflow.status)} · revision ${esc(workflow.revision)}</p>
    <p>Workflow ID: <code>${esc(workflow.id || 'Not submitted')}</code></p>
    ${approvals.length ? table(['Group', 'Permission', 'Leader', 'Approval'], rows) : '<p>No approval groups are configured.</p>'}
    ${isEdit && workflow.status === 'approved' ? '<button class="button primary" id="queue-action">Queue approved action</button>' : ''}`;
}

function finishedJobNotice(job) {
  const problem = ['failed', ...stoppedJobs].includes(job.status);
  const reason = job.last_error || 'see job details';
  if (job.status === 'failed') {
    notice(`Migration ${job.name} failed and was rolled back: ${reason}`, true);
  } else if (problem) {
    notice(`Migration ${job.name} stopped as ${job.status.replace('_', ' ')}: ${reason}. Resume or compensate it from the jobs table.`, true);
  } else {
    notice(`Migration ${job.name} ${job.status}.`);
  }
}

function definitionBody(form) {
  const fields = formData(form);
  const irreversible = fields.irreversible === 'on';
  const body = {
    name: fields.name,
    sql_up: fields.sql_up,
    sql_down: irreversible ? '' : fields.sql_down,
    irreversible,
    irreversible_reason: fields.irreversible_reason,
  };
  const batch = { table: fields.batch_table, key_column: fields.batch_key_column, size: Number(fields.batch_size || 0) };
  if (batch.table || batch.key_column || batch.size) body.batch = batch;
  if (fields.id) body.expected_revision = Number(fields.expected_revision);
  return { id: fields.id, body };
}

export async function migrationPage(path) {
  const id = databaseIdFrom(path);
  const base = apiDatabase(id);
  const apiBase = `${base}/migrations`;
  const jobsRoot = `${base}/jobs`;
  const db = await accessibleDatabase(id);
  let hide = new URLSearchParams(location.search).get('hide_rollback') === 'true';
  const historyPath = () => `${apiBase}/history${hide ? '?hide_rollback=true' : ''}`;
  const attemptsPath = `${base}/attempts?limit=100`;
  const readAttempts = (before) => api(before ? `${attemptsPath}&before=${encodeURIComponent(before)}` : attemptsPath);
  const [{ migrations = [] }, initialHistory, { jobs = [] }, initialAttempts] = await Promise.all([
    api(`${apiBase}/definitions`), api(historyPath()), api(jobsRoot), readAttempts(),
  ]);
  const isEdit = db.permission === 'view_edit';
  let latestJobs = jobs;
  let history = initialHistory;
  let historyVersion = 0;
  let nextBefore = initialAttempts.next_before;
  const latestAttempts = new Map(initialAttempts.attempts.map((attempt) => [attempt.id, attempt]));

  shell(`
    ${heading('DATABASE WORKSPACE', `${db.name} · migrations`, 'Review definitions, approvals, committed history, and asynchronous job progress.')}
    ${statusNotice}
    <p><a class="button" data-nav href="${pageDatabase(id)}">← Database viewer</a></p>
    ${panel('Migration definitions', `
      ${isEdit ? definitionForm : '<p class="muted">View only access. You can inspect definitions and history.</p>'}
      <div id="definition-list">${definitionTable(migrations, isEdit)}</div>`,
    'SQL is preserved exactly; once an execution is attempted its revision becomes immutable.')}
    ${panel('Review & approval', '<div id="workflow-panel"><p class="muted">Choose Review apply, rollback, or forward on a definition.</p></div>')}
    ${panel('Migration history', `
      <label class="checkbox-label"><input type="checkbox" id="hide-rollback" ${hide ? 'checked' : ''}> Hide rollback pairs</label>
      <div id="history-list"></div>
      <div hidden><button class="button small" id="load-older-attempts" type="button">Load older attempts</button></div>
      <div id="execution-detail" class="subsection" hidden></div>`,
    'Every attempt is recorded, including failures and retries. Hide rollback only filters committed rollback pairs.')}
    ${panel('Asynchronous jobs', `<div id="job-list">${table(jobHeaders, jobs.map((j) => jobRow(j, isEdit)).join(''))}</div>`,
    'Jobs run through the worker. Progress updates while this page is open.')}`,
  'Migrations');

  const historyList = root.querySelector('#history-list');
  const olderAttempts = root.querySelector('#load-older-attempts');
  const hideRollback = root.querySelector('#hide-rollback');

  const showDetail = (html) => {
    const detail = root.querySelector('#execution-detail');
    detail.hidden = false;
    detail.innerHTML = html;
    detail.scrollIntoView({ behavior: 'smooth', block: 'center' });
  };

  function bindHistory() {
    historyList.querySelectorAll('.execution-detail').forEach((button) => {
      button.onclick = async () => {
        try {
          const { execution } = await api(`${apiBase}/history/${button.dataset.id}`);
          showDetail(executionDetail(execution));
        } catch (error) {
          notice(error.message, true);
        }
      };
    });
    historyList.querySelectorAll('.attempt-detail').forEach((button) => {
      button.onclick = () => {
        const attempt = latestAttempts.get(button.dataset.id);
        if (attempt) showDetail(attemptDetail(attempt));
      };
    });
  }

  function renderHistory() {
    const { executions = [], legacy_attempts: legacy = [] } = history;
    historyList.innerHTML = historyTable(executions, legacy, [...latestAttempts.values()]);
    olderAttempts.parentElement.hidden = !nextBefore;
    bindHistory();
  }

  async function refreshExecutions() {
    const version = ++historyVersion;
    const result = await api(historyPath());
    if (version === historyVersion) history = result;
  }

  async function refreshAttempts() {
    let page = await readAttempts();
    const hasLoadedAttempts = latestAttempts.size > 0;
    if (!hasLoadedAttempts) nextBefore = page.next_before;
    const recent = [];
    // Cari titik temu dengan riwayat yang sudah dimuat agar lonjakan attempt
    // di antara dua refresh tidak meninggalkan celah.
    for (;;) {
      recent.push(...page.attempts);
      if (!hasLoadedAttempts || !page.next_before || page.attempts.some((a) => latestAttempts.has(a.id))) break;
      page = await readAttempts(page.next_before);
    }
    recent.forEach((attempt) => latestAttempts.set(attempt.id, attempt));
  }

  // Muat ulang definisi karena statusnya dapat berubah setelah eksekusi.
  async function refreshHistory() {
    const [{ migrations: m = [] }] = await Promise.all([
      api(`${apiBase}/definitions`), refreshExecutions(), refreshAttempts(),
    ]);
    if (!historyList.isConnected) return;
    root.querySelector('#definition-list').innerHTML = definitionTable(m, isEdit);
    renderHistory();
    bindDefinitions();
  }

  function bindJobActions() {
    root.querySelectorAll('.resume-job').forEach((button) => {
      button.onclick = async () => {
        try {
          await api(`${jobsRoot}/${button.dataset.id}/resume`, 'POST', {});
          refreshJobs();
        } catch (error) {
          notice(error.message, true);
        }
      };
    });
    root.querySelectorAll('.cancel-job').forEach((button) => {
      button.onclick = async () => {
        if (!confirm('Compensate batches already committed for this job?')) return;
        try {
          await api(`${jobsRoot}/${button.dataset.id}/cancel`, 'POST', {});
          refreshJobs();
        } catch (error) {
          notice(error.message, true);
        }
      };
    });
  }

  // refreshJobs polls while a job is pending or running and refreshes the
  // history once a job settles.
  async function refreshJobs() {
    try {
      const before = activeJobIDs(latestJobs);
      const { jobs: latest = [] } = await api(jobsRoot);
      const body = root.querySelector('#job-list tbody');
      if (!body) return;
      latestJobs = latest;
      body.innerHTML = latest.map((j) => jobRow(j, isEdit)).join('') || '<tr><td colspan="8" class="empty-cell">No records.</td></tr>';
      bindJobActions();
      const now = activeJobIDs(latest);
      if (before.some((jobID) => !now.includes(jobID))) {
        await refreshHistory();
        const done = latest.find((j) => before.includes(j.id) && !now.includes(j.id));
        if (done) finishedJobNotice(done);
      }
      if (now.length) state.poll = setTimeout(refreshJobs, 3000);
    } catch (error) {
      notice(error.message, true);
    }
  }

  function renderWorkflow(workflow, migrationID) {
    const workflowPanel = root.querySelector('#workflow-panel');
    workflowPanel.innerHTML = workflowView(workflow, isEdit);
    workflowPanel.querySelectorAll('.approve-group').forEach((button) => {
      button.onclick = async () => {
        try {
          const result = await api(`${apiBase}/workflows/${button.dataset.id}/approvals`, 'POST', {});
          renderWorkflow(result.workflow, migrationID);
        } catch (error) {
          notice(error.message, true);
        }
      };
    });
    workflowPanel.querySelector('#queue-action')?.addEventListener('click', async () => {
      try {
        const result = await api(`${apiBase}/definitions/${migrationID}/actions`, 'POST', { action: workflow.action, approval_id: workflow.id });
        workflowPanel.innerHTML = `<p class="muted">Queued ${esc(workflow.action)} as job <code>${esc(result.job_id)}</code>.</p>`;
        notice(`Queued job ${result.job_id}.`);
        await refreshJobs();
      } catch (error) {
        notice(error.message, true);
      }
    });
  }

  async function beginReview(migrationID, action) {
    const workflowPanel = root.querySelector('#workflow-panel');
    workflowPanel.innerHTML = '<p>Preparing review…</p>';
    try {
      const { workflow } = await api(`${apiBase}/definitions/${migrationID}/review`, 'POST', { action });
      const latest = await api(`${apiBase}/definitions/${migrationID}/workflow`);
      renderWorkflow(latest.workflow || workflow, migrationID);
    } catch (error) {
      workflowPanel.innerHTML = `<p class="notice error">${esc(error.message)}</p>`;
    }
  }

  function bindEditor(form) {
    const elements = form.elements;
    elements.irreversible.onchange = () => {
      elements.sql_down.disabled = elements.irreversible.checked;
      elements.irreversible_reason.required = elements.irreversible.checked;
    };
    form.onsubmit = async (event) => {
      event.preventDefault();
      const { id: migrationID, body } = definitionBody(form);
      try {
        await api(migrationID ? `${apiBase}/definitions/${migrationID}` : `${apiBase}/definitions`, migrationID ? 'PUT' : 'POST', body);
        await migrationPage(path);
        notice('Migration definition saved.');
      } catch (error) {
        notice(error.message, true);
      }
    };
    root.querySelector('#reset-migration').onclick = () => {
      form.reset();
      elements.id.value = '';
      elements.expected_revision.value = '';
      elements.sql_down.disabled = false;
      elements.irreversible_reason.required = false;
    };
    root.querySelector('#compare-schema').onclick = async () => {
      try {
        const desired = JSON.parse(root.querySelector('#desired-schema').value);
        const { schema_diff: diff } = await api(`${apiBase}/schema-diff`, 'POST', { desired_schema: desired });
        const changes = (diff.changes || []).map((c) => `${esc(c.kind)} · ${esc(c.object)}: ${esc(c.detail)}`).join('<br>') || 'Schemas match.';
        const notes = (diff.notes || []).map((n) => `<p class="muted">${esc(n)}</p>`).join('');
        root.querySelector('#schema-diff-results').innerHTML = `<p>${changes}</p>${notes}`;
        elements.sql_up.value = diff.sql || '';
        if (!elements.name.value) elements.name.value = 'Schema update';
      } catch (error) {
        notice(error.message, true);
      }
    };
    root.querySelector('#preview-plan').onclick = async () => {
      const sql = root.querySelector('#plan-sql').value;
      const analyze = root.querySelector('#plan-analyze').checked;
      if (!sql.trim()) {
        notice('Enter SQL to preview.');
        return;
      }
      if (analyze && !confirm(analyzeWarning)) return;
      try {
        const { query_plan: plan } = await api(`${apiBase}/explain`, 'POST', { sql, analyze });
        root.querySelector('#plan-results').innerHTML = `<h4>${esc(plan.mode)}</h4><pre>${esc(JSON.stringify(plan.plan, null, 2))}</pre>`;
      } catch (error) {
        notice(error.message, true);
      }
    };
  }

  // fillEditor loads a definition's latest revision into the editor.
  async function fillEditor(form, migrationID) {
    const elements = form.elements;
    const { migration, revision } = await api(`${apiBase}/definitions/${migrationID}`);
    elements.id.value = migration.id;
    elements.expected_revision.value = revision.number;
    elements.name.value = migration.name;
    elements.sql_up.value = revision.sql_up;
    elements.sql_down.value = revision.sql_down;
    elements.irreversible.checked = revision.irreversible;
    elements.sql_down.disabled = revision.irreversible;
    elements.irreversible_reason.value = revision.irreversible_reason;
    elements.batch_table.value = revision.batch?.table || '';
    elements.batch_key_column.value = revision.batch?.key_column || '';
    elements.batch_size.value = revision.batch?.size || '';
    form.scrollIntoView({ behavior: 'smooth' });
  }

  function bindDefinitions() {
    const form = root.querySelector('#migration-form');
    root.querySelectorAll('.select-migration').forEach((button) => {
      button.onclick = () => fillEditor(form, button.dataset.id).catch((error) => notice(error.message, true));
    });
    root.querySelectorAll('.action').forEach((button) => {
      button.onclick = () => beginReview(button.dataset.id, button.dataset.action);
    });
    root.querySelectorAll('.workflow-status').forEach((button) => {
      button.onclick = async () => {
        try {
          const { workflow } = await api(`${apiBase}/definitions/${button.dataset.id}/workflow`);
          renderWorkflow(workflow, button.dataset.id);
          root.querySelector('#workflow-panel').scrollIntoView({ behavior: 'smooth', block: 'center' });
        } catch (error) {
          notice(error.message, true);
        }
      };
    });
  }

  olderAttempts.onclick = async () => {
    if (!nextBefore) return;
    olderAttempts.disabled = true;
    try {
      const page = await readAttempts(nextBefore);
      page.attempts.forEach((attempt) => latestAttempts.set(attempt.id, attempt));
      nextBefore = page.next_before;
      if (historyList.isConnected) renderHistory();
    } catch (error) {
      if (historyList.isConnected) notice(error.message, true);
    } finally {
      olderAttempts.disabled = false;
    }
  };
  hideRollback.onchange = async () => {
    const previous = hide;
    hide = hideRollback.checked;
    hideRollback.disabled = true;
    try {
      await refreshExecutions();
      if (!historyList.isConnected) return;
      const url = new URL(location.href);
      if (hide) url.searchParams.set('hide_rollback', 'true');
      else url.searchParams.delete('hide_rollback');
      route(url.pathname + url.search);
      renderHistory();
    } catch (error) {
      hide = previous;
      hideRollback.checked = previous;
      if (historyList.isConnected) notice(error.message, true);
    } finally {
      hideRollback.disabled = false;
    }
  };
  renderHistory();
  const form = root.querySelector('#migration-form');
  if (form) bindEditor(form);
  bindDefinitions();
  bindJobActions();
  if (activeJobIDs(jobs).length) state.poll = setTimeout(refreshJobs, 3000);
}
