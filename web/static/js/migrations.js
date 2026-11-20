(() => {
  const root = document.getElementById('migration-workspace');
  if (!root) return;
  const id = root.dataset.databaseId, legacyVersion = root.dataset.version, $key = value => document.getElementById(value);
  const endpoint = `/databases/${encodeURIComponent(id)}/migrations`, jobsEndpoint = `/databases/${encodeURIComponent(id)}/jobs`;
  const notice = $key('migration-notice'), form = $key('migration-form'), sqlInput = $key('migration-sql');
  const planPanel = $key('query-plan'), modeInput = $key('query-plan-mode'), detailPanel = $key('migration-detail'), workflowPanel = $key('workflow-panel');
  const diffButton = $key('compare-schema'), diffPanel = $key('schema-diff-results');
  const activeJobs = ['pending', 'running', 'partial', 'partially_reverted'];
  const state = {user: null, db: null, canEdit: false, definitions: [], jobs: [], poll: null, busy: false};

  const showError = message => Atlas.setNotice(notice, message, true);
  const show = message => Atlas.setNotice(notice, message);
  const time = value => value ? new Date(value).toLocaleString() : '—';
  const actor = value => !value ? '—' : value === state.user?.id ? `${state.user.username} (you)` : value;
  function el(tag, attributes = {}, ...children) {
    const element = document.createElement(tag);
    Object.entries(attributes).forEach(([key, value]) => {
      if (value === undefined || value === null || value === false) return;
      if (key === 'onclick') element.addEventListener('click', value);
      else if (key === 'className') element.className = value;
      else element.setAttribute(key, value === true ? '' : value);
    });
    children.flat().forEach(child => element.append(child instanceof Node ? child : document.createTextNode(child == null ? '—' : String(child))));
    return element;
  }
  const row = (...cells) => el('tr', {}, ...cells.map(cell => cell instanceof Node && cell.tagName === 'TD' ? cell : el('td', {}, cell)));
  const emptyRow = (columns, text) => el('tr', {}, el('td', {colspan: columns, className: 'empty-cell'}, text));
  const button = (label, onclick, className = 'button small') => el('button', {type: 'button', className, onclick}, label);
  const statusPill = status => el('span', {className: 'status-pill'}, status);

  async function revisionSQL(migrationID, revision, action) {
    const {revision: stored} = await Atlas.request(`${endpoint}/definitions/${encodeURIComponent(migrationID)}/revisions/${encodeURIComponent(revision)}`);
    return action === 'rollback' ? stored.sql_down : stored.sql_up;
  }
  function showDetail(entry) {
    detailPanel.hidden = false;
    detailPanel.replaceChildren(
      el('div', {className: 'panel-heading'}, el('div', {}, el('p', {className: 'eyebrow'}, 'MIGRATION DETAIL'), el('h2', {}, entry.title))),
      el('dl', {className: 'detail-list'}, ...[
        ['Status', entry.status], ['Action', entry.action], ['Revision', entry.revision ?? '—'], ['Executed by', actor(entry.executedBy)],
        ['Started', time(entry.startedAt)], ['Finished', time(entry.finishedAt)], ['Execution ID', entry.executionID || '—'],
      ].flatMap(([term, value]) => [el('dt', {}, term), el('dd', {}, value)])),
      ...(entry.error ? [el('p', {className: 'notice error'}, `Error: ${entry.error}`)] : []),
      el('h3', {}, 'SQL'), el('pre', {}, entry.sql || 'Loading SQL…'));
    detailPanel.scrollIntoView({behavior: 'smooth', block: 'nearest'});
    if (!entry.sql && entry.migrationID) {
      revisionSQL(entry.migrationID, entry.revision, entry.action)
        .then(sql => { detailPanel.querySelector('pre').textContent = sql || '(empty)'; })
        .catch(error => { detailPanel.querySelector('pre').textContent = `SQL unavailable: ${error.message}`; });
    }
  }
  function historyEntries(history, jobs) {
    const entries = (history.executions || []).map(execution => ({
      sort: execution.started_at, sequence: execution.sequence, title: execution.legacy_version ? `Migration ${execution.legacy_version}: ${execution.name}` : execution.name,
      name: execution.name, action: execution.action, status: execution.status, revision: execution.revision || (execution.legacy_version ? 'legacy' : null),
      executedBy: execution.executed_by, startedAt: execution.started_at, finishedAt: execution.finished_at, executionID: execution.id, sql: execution.sql,
    }));
    (history.legacy_attempts || []).forEach(attempt => entries.push({
      sort: attempt.started_at, sequence: null, title: `Migration ${attempt.version}: ${attempt.name}`, name: attempt.name, action: 'apply',
      status: attempt.status, revision: 'legacy', executedBy: attempt.executed_by, startedAt: attempt.started_at, finishedAt: attempt.finished_at, sql: attempt.sql,
    }));
    jobs.filter(job => job.status !== 'succeeded').forEach(job => entries.push({
      sort: job.started_at || job.created_at, sequence: null, title: job.name, name: job.name, action: job.action, status: job.status,
      revision: job.revision, executedBy: job.actor_id, startedAt: job.started_at || job.created_at, finishedAt: job.finished_at,
      executionID: job.execution_id, migrationID: job.migration_id, error: job.last_error,
    }));
    return entries.sort((a, b) => new Date(b.sort) - new Date(a.sort));
  }
  function renderHistory(history, jobs) {
    const body = $key('history-list'), entries = historyEntries(history, jobs);
    body.replaceChildren(...(entries.length ? entries.map(entry => {
      const link = el('a', {href: '#migration-detail', onclick: event => { event.preventDefault(); showDetail(entry); }}, entry.sequence ?? '—');
      return row(el('td', {}, link), entry.name, entry.action, statusPill(entry.status), entry.revision ?? '—', actor(entry.executedBy), time(entry.startedAt), time(entry.finishedAt));
    }) : [emptyRow(8, 'No migrations have been run yet.')]));
  }

  function renderJobs(jobs) {
    $key('job-list').replaceChildren(...(jobs.length ? jobs.map(job =>
      row(job.name, job.action, statusPill(job.status), job.attempts, job.last_error || '—', time(job.created_at))
    ) : [emptyRow(6, 'No migration has been queued yet.')]));
  }
  function schedulePoll() {
    clearTimeout(state.poll);
    if (state.jobs.some(job => activeJobs.includes(job.status))) state.poll = setTimeout(refreshExecution, 3000);
  }
  async function refreshExecution() {
    try {
      const [history, {jobs = []}] = await Promise.all([Atlas.request(`${endpoint}/history`), Atlas.request(jobsEndpoint)]);
      const finished = state.jobs.filter(job => activeJobs.includes(job.status)).map(job => jobs.find(next => next.id === job.id)).filter(job => job && !activeJobs.includes(job.status));
      state.jobs = jobs;
      renderHistory(history, jobs);
      renderJobs(jobs);
      finished.forEach(job => job.status === 'succeeded' ? show(`Migration "${job.name}" (${job.action}) committed.`) : showError(`Migration "${job.name}" (${job.action}) ${job.status}${job.last_error ? `: ${job.last_error}` : ''}. Changes were rolled back.`));
      if (finished.length) await loadDefinitions();
    } catch (error) { showError(error.message); }
    schedulePoll();
  }

  async function loadDefinitions() {
    const {migrations = []} = await Atlas.request(`${endpoint}/definitions`);
    state.definitions = migrations;
    $key('definition-list').replaceChildren(...(migrations.length ? migrations.map(definition => {
      const actions = el('div', {className: 'top-actions'});
      if (state.canEdit) {
        if (!definition.frozen_at) actions.append(button('Edit', () => editDefinition(definition.id)));
        ['apply', 'rollback', 'forward'].forEach(action => actions.append(button(`Review ${action}`, () => beginReview(definition.id, action))));
      }
      actions.append(button('Approval status', () => loadWorkflow(definition.id)));
      return row(definition.name, definition.current_revision, definition.frozen_at ? 'Executed (read-only revision)' : 'Draft', el('td', {}, actions));
    }) : [emptyRow(4, state.canEdit ? 'No migrations yet. Create one below.' : 'No migrations yet.')]));
  }
  function resetEditor() {
    form.reset();
    $key('migration-id').value = '';
    $key('migration-revision').value = '';
    $key('editor-title').textContent = 'Create a migration';
    $key('save-migration').textContent = 'Save migration';
    toggleIrreversible();
    clearPlan();
  }
  async function editDefinition(migrationID) {
    try {
      const {migration, revision} = await Atlas.request(`${endpoint}/definitions/${encodeURIComponent(migrationID)}`);
      $key('migration-id').value = migration.id;
      $key('migration-revision').value = revision.number;
      $key('migration-name').value = migration.name;
      sqlInput.value = revision.sql_up;
      $key('migration-sql-down').value = revision.sql_down || '';
      $key('migration-irreversible').checked = revision.irreversible;
      $key('migration-irreversible-reason').value = revision.irreversible_reason || '';
      toggleIrreversible();
      $key('editor-title').textContent = `Edit "${migration.name}" (revision ${revision.number})`;
      $key('save-migration').textContent = 'Save as new revision';
      clearPlan();
      $key('migration-editor').scrollIntoView({behavior: 'smooth', block: 'start'});
    } catch (error) { showError(error.message); }
  }
  function toggleIrreversible() {
    const irreversible = $key('migration-irreversible').checked;
    $key('migration-sql-down').disabled = irreversible;
    $key('migration-irreversible-reason').hidden = $key('irreversible-reason-label').hidden = !irreversible;
    $key('migration-irreversible-reason').required = irreversible;
  }
  $key('migration-irreversible').addEventListener('change', toggleIrreversible);
  $key('reset-migration').addEventListener('click', resetEditor);
  form.addEventListener('submit', async event => {
    event.preventDefault();
    if (state.busy) return;
    const migrationID = $key('migration-id').value, irreversible = $key('migration-irreversible').checked;
    if (!irreversible && !$key('migration-sql-down').value.trim()) { showError('Provide SQL down, or mark the migration as irreversible with a reason.'); return; }
    const body = {name: $key('migration-name').value, sql_up: sqlInput.value, sql_down: irreversible ? '' : $key('migration-sql-down').value,
      irreversible, irreversible_reason: irreversible ? $key('migration-irreversible-reason').value : ''};
    setBusy(true);
    try {
      const result = migrationID
        ? await Atlas.request(`${endpoint}/definitions/${encodeURIComponent(migrationID)}`, 'PUT', {...body, expected_revision: Number($key('migration-revision').value)})
        : await Atlas.request(`${endpoint}/definitions`, 'POST', body);
      show(`Migration "${result.migration.name}" saved as revision ${result.revision.number}. Choose Review apply to run it.`);
      resetEditor();
      await loadDefinitions();
    } catch (error) { showError(error.message); } finally { setBusy(false); }
  });

  function renderWorkflow(workflow) {
    const definition = state.definitions.find(item => item.id === workflow.migration_id);
    const approvals = workflow.approvals || [], pending = approvals.filter(approval => !approval.approved).length;
    const table = el('table', {}, el('thead', {}, el('tr', {}, ...['Group', 'Permission', 'Leader', 'Approval'].map(label => el('th', {}, label)))),
      el('tbody', {}, ...(approvals.length ? approvals.map(approval => {
        const canApprove = !approval.approved && workflow.status === 'pending_review' && approval.leader_id && approval.leader_id === state.user.id;
        const decision = approval.approved ? `Approved ${time(approval.approved_at)}`
          : canApprove ? button('Approve', () => approve(workflow.id), 'button small primary')
          : approval.leader_id ? 'Waiting for the group leader' : 'No leader assigned';
        return row(approval.group_name, approval.permission, actor(approval.leader_id), el('td', {}, decision));
      }) : [emptyRow(4, 'No group has access to this database.')])));
    const summary = workflow.status === 'approved' ? 'Approved by every group leader. It can now be executed.'
      : `${pending} of ${approvals.length} required approval(s) still missing.`;
    const children = [
      el('div', {className: 'panel-heading'}, el('div', {}, el('p', {className: 'eyebrow'}, 'REVIEW & APPROVAL'),
        el('h2', {}, `${definition?.name || 'Migration'} · ${workflow.action}`),
        el('p', {className: 'muted'}, `Revision ${workflow.revision} · status ${workflow.status.replace('_', ' ')}. ${summary}`))),
      el('div', {className: 'table-wrap'}, table),
    ];
    if (workflow.status === 'approved' && state.canEdit) {
      children.push(el('div', {className: 'form-row'}, button(`Execute ${workflow.action}`, () => execute(workflow), 'button primary')));
    }
    workflowPanel.replaceChildren(...children);
    workflowPanel.hidden = false;
  }
  async function beginReview(migrationID, action) {
    try {
      const {workflow} = await Atlas.request(`${endpoint}/definitions/${encodeURIComponent(migrationID)}/review`, 'POST', {action});
      renderWorkflow(workflow);
      show(`Review for ${action} created. Every group leader with access must approve before it can run.`);
      workflowPanel.scrollIntoView({behavior: 'smooth', block: 'start'});
    } catch (error) { showError(error.message); }
  }
  async function loadWorkflow(migrationID) {
    try {
      const {workflow} = await Atlas.request(`${endpoint}/definitions/${encodeURIComponent(migrationID)}/workflow`);
      renderWorkflow(workflow);
      workflowPanel.scrollIntoView({behavior: 'smooth', block: 'start'});
    } catch (error) { showError(error.status === 404 ? 'No review has been requested for this migration yet.' : error.message); }
  }
  async function approve(workflowID) {
    try {
      const {workflow} = await Atlas.request(`${endpoint}/workflows/${encodeURIComponent(workflowID)}/approvals`, 'POST', {});
      renderWorkflow(workflow);
      show('Approval recorded.');
    } catch (error) { showError(error.message); }
  }
  async function execute(workflow) {
    try {
      await Atlas.request(`${endpoint}/definitions/${encodeURIComponent(workflow.migration_id)}/actions`, 'POST', {action: workflow.action, approval_id: workflow.id});
      workflowPanel.hidden = true;
      show('Migration queued. The worker runs it in a transaction; the history updates when it finishes.');
      await refreshExecution();
    } catch (error) { showError(error.message); }
  }

  const setBusy = value => { state.busy = value; form.querySelectorAll('button').forEach(item => { item.disabled = value; }); };
  const clearPlan = () => { planPanel.hidden = true; planPanel.replaceChildren(); };
  sqlInput.addEventListener('input', clearPlan);
  modeInput.addEventListener('change', () => { clearPlan(); $key('query-plan-warning').hidden = modeInput.value !== 'explain_analyze'; });
  function showPlan(result) {
    const analyzed = result.mode === 'explain_analyze';
    planPanel.replaceChildren(el('h2', {}, `Execution plan · ${analyzed ? 'EXPLAIN ANALYZE' : 'EXPLAIN'}`),
      el('p', {className: 'muted'}, analyzed ? 'SQL was executed and the transaction was rolled back. Actual times are milliseconds; row counts and node times are averages per loop. Sequence/auto-increment values and external effects may persist.' : 'Estimated rows and planner cost units, not actual execution time. Preview does not execute the migration.'));
    planPanel.hidden = false;
    if (!Array.isArray(result.plan)) { planPanel.append(el('pre', {}, typeof result.plan === 'string' ? result.plan : JSON.stringify(result.plan, null, 2))); return; }
    result.plan.forEach(entry => { if (entry['Execution Time'] != null) planPanel.append(el('p', {}, `Planning: ${entry['Planning Time'] ?? '—'} ms · Execution: ${entry['Execution Time']} ms`)); });
    const columns = ['Node / join type', 'Relation / index', 'Estimated rows', 'Startup cost', 'Total cost'];
    if (analyzed) columns.push('Actual rows / loop', 'Loops', 'Actual time ms / loop');
    const body = el('tbody');
    const addNode = (node, depth) => {
      const label = el('td', {}, `${'  '.repeat(depth)}${depth ? '↳ ' : ''}${node['Node Type'] || 'Unknown'}${node['Join Type'] ? ` (${node['Join Type']})` : ''}`);
      label.style.whiteSpace = 'pre';
      const values = [[node['Relation Name'], node['Index Name']].filter(Boolean).join(' / '), node['Plan Rows'], node['Startup Cost'], node['Total Cost']];
      if (analyzed) values.push(node['Actual Rows'], node['Actual Loops'], node['Actual Total Time']);
      body.append(row(label, ...values));
      (node.Plans || []).forEach(child => addNode(child, depth + 1));
    };
    result.plan.forEach(entry => { if (entry.Plan) addNode(entry.Plan, 0); });
    planPanel.append(el('div', {className: 'table-wrap'}, el('table', {}, el('thead', {}, el('tr', {}, ...columns.map(label => el('th', {}, label)))), body)),
      el('details', {}, el('summary', {}, 'Full plan JSON'), el('pre', {}, JSON.stringify(result.plan, null, 2))));
  }
  $key('preview-query-plan').addEventListener('click', async () => {
    if (state.busy || !sqlInput.reportValidity()) return;
    const sql = sqlInput.value, analyze = modeInput.value === 'explain_analyze';
    if (analyze && !window.confirm('EXPLAIN ANALYZE executes SQL. Transactional changes will be rolled back, but sequences and external effects may persist. Continue?')) return;
    setBusy(true); planPanel.hidden = true; show(analyze ? 'Executing query for analysis…' : 'Planning query…');
    try {
      const result = await Atlas.request(`${endpoint}/explain`, 'POST', {sql, analyze});
      if (sqlInput.value !== sql || (modeInput.value === 'explain_analyze') !== analyze) { showError('SQL or mode changed while planning. Preview again to see the current plan.'); return; }
      showPlan(result.query_plan); show('Execution plan ready.');
    } catch (error) { showError(error.message); } finally { setBusy(false); }
  });
  diffButton.addEventListener('click', async () => {
    let desired;
    try { desired = JSON.parse($key('desired-schema').value); } catch (_) { showError('Desired schema must be valid JSON.'); return; }
    diffButton.disabled = true; diffPanel.hidden = true; clearPlan(); show('Comparing database schemas…');
    try {
      const {schema_diff: diff} = await Atlas.request(`${endpoint}/schema-diff`, 'POST', {desired_schema: desired});
      diffPanel.replaceChildren(el('h3', {}, 'Schema differences'),
        ...(diff.changes?.length ? diff.changes.map(change => el('p', {}, `${change.kind} · ${change.object}: ${change.detail}${change.destructive ? ' (review carefully)' : ''}`)) : [el('p', {}, 'Schemas match.')]),
        ...(diff.notes || []).map(note => el('p', {className: 'muted'}, note)));
      diffPanel.hidden = false;
      sqlInput.value = diff.sql || '';
      if (!$key('migration-name').value) $key('migration-name').value = 'Schema update';
      show('Comparison complete. Review and edit the generated SQL, add SQL down, then save the migration.');
    } catch (error) { showError(error.message); } finally { diffButton.disabled = false; }
  });

  async function load() {
    try {
      const [{user}, {databases = []}] = await Promise.all([Atlas.request('/auth/me'), Atlas.request('/databases')]);
      state.user = user;
      state.db = databases.find(item => item.id === id);
      if (!state.db) throw new Error('Database access is unavailable for this account.');
      state.canEdit = state.db.permission === 'view_edit';
      $key('migration-database-title').textContent = `Migrations · ${state.db.name}`;
      $key('migration-permission').textContent = state.canEdit ? 'View and edit access' : 'View only access: you can review history, migrations, and approvals.';
      if (state.db.dbms === 'mysql') $key('editor-help').textContent = 'MySQL commits DDL statements implicitly: a later failure cannot undo DDL that already ran. Data changes are rolled back and the failure is kept in the history.';
      $key('migration-editor').hidden = !state.canEdit;
      await Promise.all([loadDefinitions(), refreshExecution()]);
      if (legacyVersion) {
        const {migration} = await Atlas.request(`${endpoint}/${encodeURIComponent(legacyVersion)}`);
        showDetail({title: `Migration ${migration.version}: ${migration.name}`, status: migration.status, action: 'apply', revision: 'legacy',
          executedBy: migration.executed_by, startedAt: migration.started_at, finishedAt: migration.finished_at, sql: migration.sql});
      }
    } catch (error) { showError(error.message); }
  }
  window.addEventListener('pagehide', () => clearTimeout(state.poll));
  load();
})();
