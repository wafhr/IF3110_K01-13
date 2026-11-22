import {
  accessibleDatabase, api, apiDatabase, databaseIdFrom, esc, heading, notice, pageDatabase, panel, root, shell, statusNotice, table,
} from '../core.js';

const pageSize = 50;

function tableDetails(t) {
  const columns = (t.columns || []).map((c) => `
    <tr><td>${esc(c.name)}</td><td>${esc(c.data_type)}</td><td>${c.nullable ? 'Yes' : 'No'}</td>
    <td>${esc(c.default || '')}</td><td>${esc(c.identity || '')}</td></tr>`).join('');
  const constraints = (t.constraints || []).map((c) => `
    <tr><td>${esc(c.name)}</td><td>${esc(c.type)}</td><td>${esc(c.definition)}</td></tr>`).join('');
  const indexes = (t.indexes || []).map((i) => `
    <tr><td>${esc(i.name)}</td><td>${esc(i.definition)}</td><td>${i.unique ? 'Yes' : 'No'}</td></tr>`).join('');
  return `
    <h2>${esc(t.schema)}.${esc(t.name)} · ${esc(t.type)}</h2>
    ${panel('Columns', table(['Name', 'Type', 'Nullable', 'Default', 'Identity'], columns))}
    ${panel('Constraints', table(['Name', 'Type', 'Definition'], constraints))}
    ${panel('Indexes', table(['Name', 'Definition', 'Unique'], indexes))}
    <section id="table-data" class="subsection"><h3>Rows</h3><p>Loading rows…</p></section>`;
}

async function loadRows(dataPanel, tablePath, offset = 0) {
  const { data } = await api(`${tablePath}/data?limit=${pageSize}&offset=${offset}`);
  const range = data.rows.length ? `${offset + 1}–${offset + data.rows.length}` : '(empty)';
  const rows = (data.rows || []).map((r) => `<tr>${r.map((c) => `<td>${esc(c === null ? 'NULL' : c)}</td>`).join('')}</tr>`).join('');
  dataPanel.innerHTML = `
    <h3>Rows ${range}</h3>
    ${table((data.columns || []).map((c) => c.name), rows)}
    <div class="pagination">
      <button id="prev" class="button small" ${offset === 0 ? 'disabled' : ''}>Previous</button>
      <button id="next" class="button small" ${data.has_more ? '' : 'disabled'}>Next</button>
    </div>`;
  dataPanel.querySelector('#prev').onclick = () => loadRows(dataPanel, tablePath, Math.max(0, offset - pageSize));
  dataPanel.querySelector('#next').onclick = () => loadRows(dataPanel, tablePath, offset + pageSize);
}

export async function databasePage(path) {
  const id = databaseIdFrom(path);
  const base = apiDatabase(id);
  const db = await accessibleDatabase(id);
  const { schemas = [] } = await api(`${base}/schemas`);
  shell(`
    ${heading('DATABASE WORKSPACE', db.name, `${db.dbms} · ${db.database_name} · ${db.permission === 'view_edit' ? 'View and edit' : 'View only'}`)}
    ${statusNotice}
    <div class="form-row">
      <label>Schema
        <select id="schema-select">
          <option value="">Choose schema</option>${schemas.map((s) => `<option>${esc(s.name)}</option>`).join('')}
        </select>
      </label>
      <label>Table<select id="table-select" disabled><option>Select a schema first</option></select></label>
      <a class="button primary" data-nav href="${pageDatabase(id)}/migrations">Migrations & history</a>
      <a class="button" data-nav href="/">Back to databases</a>
    </div>
    <section class="panel" id="table-details">
      <div class="empty-state"><h3>Select a table</h3><p>Choose a schema and table to inspect its structure and rows.</p></div>
    </section>`,
  'Database');

  const schema = root.querySelector('#schema-select');
  const select = root.querySelector('#table-select');
  const details = root.querySelector('#table-details');

  schema.onchange = async () => {
    select.disabled = true;
    select.innerHTML = '<option>Loading…</option>';
    details.innerHTML = '';
    try {
      const { tables = [] } = await api(`${base}/schemas/${encodeURIComponent(schema.value)}/tables`);
      select.innerHTML = '<option value="">Choose table</option>' + tables.map((t) => `<option>${esc(t.name)}</option>`).join('');
      select.disabled = false;
    } catch (error) {
      notice(error.message, true);
    }
  };

  select.onchange = async () => {
    if (!select.value) return;
    const tablePath = `${base}/schemas/${encodeURIComponent(schema.value)}/tables/${encodeURIComponent(select.value)}`;
    details.setAttribute('aria-busy', 'true');
    try {
      const { table: t } = await api(tablePath);
      details.innerHTML = tableDetails(t);
      await loadRows(root.querySelector('#table-data'), tablePath);
    } catch (error) {
      notice(error.message, true);
    } finally {
      details.removeAttribute('aria-busy');
    }
  };
}
