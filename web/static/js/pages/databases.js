import { api, esc, formData, heading, notice, panel, root, shell, statusNotice, table } from '../core.js';

const databaseRow = (d) => `
  <tr>
    <td><strong>${esc(d.name)}</strong><span class="subline">${esc(d.id)}</span></td>
    <td>${esc(d.dbms)}</td>
    <td>${esc(d.username)}@${esc(d.host)}:${d.port}/${esc(d.database_name)}</td>
    <td>${esc(d.ssl_mode)}</td>
    <td>
      <button class="button small edit-db" data-id="${esc(d.id)}">Edit</button>
      <button class="button small test-db" data-id="${esc(d.id)}">Test</button>
      <button class="button small delete-db" data-id="${esc(d.id)}">Delete</button>
    </td>
  </tr>`;

const registrationForm = `
  <form id="database-form" class="form-row">
    <input name="id" type="hidden">
    <label>Name<input name="name" maxlength="100" required></label>
    <label>DBMS<select name="dbms"><option value="postgres">PostgreSQL</option><option value="mysql">MySQL</option></select></label>
    <label>Host<input name="host" required></label>
    <label>Port<input name="port" type="number" min="1" max="65535" value="5432" required></label>
    <label>Database<input name="database_name" required></label>
    <label>Username<input name="username" required></label>
    <label>Password<input name="password" type="password" autocomplete="new-password" required></label>
    <label>SSL/TLS
      <select name="ssl_mode">
        <option>require</option><option>disable</option><option>prefer</option>
        <option>allow</option><option>verify-ca</option><option>verify-full</option>
      </select>
    </label>
    <button class="button" type="button" id="test-new-db">Test connection</button>
    <button class="button primary">Save database</button>
    <button class="button" type="reset">Clear</button>
  </form>`;

export async function adminDatabasesPage() {
  const { databases = [] } = await api('/admin/databases');
  shell(`
    ${heading('PLATFORM ADMINISTRATION', 'Target databases', 'Register and maintain database connections. Passwords are never displayed.')}
    ${statusNotice}
    ${panel('Register database', registrationForm, 'Enter the connection password when registering or updating settings.')}
    ${panel('Registered databases', table(['Name', 'DBMS', 'Connection', 'SSL/TLS', 'Actions'], databases.map(databaseRow).join('')))}`,
  'Databases');

  const form = root.querySelector('#database-form');
  form.elements.dbms.onchange = () => {
    form.elements.port.value = form.elements.dbms.value === 'mysql' ? 3306 : 5432;
  };
  const payload = () => {
    const fields = formData(form);
    delete fields.id;
    fields.port = Number(fields.port);
    if (!fields.password) delete fields.password;
    return fields;
  };

  root.querySelector('#test-new-db').onclick = async () => {
    const { name, ...connection } = payload();
    try {
      await api('/admin/databases/test-connection', 'POST', connection);
      notice('Connection succeeded.');
    } catch (error) {
      notice(error.message, true);
    }
  };

  form.onsubmit = async (event) => {
    event.preventDefault();
    const id = form.elements.id.value;
    try {
      await api(id ? `/admin/databases/${id}` : '/admin/databases', id ? 'PUT' : 'POST', payload());
      await adminDatabasesPage();
      notice(id ? 'Database updated.' : 'Database registered.');
    } catch (error) {
      notice(error.message, true);
    }
  };

  root.querySelectorAll('.edit-db').forEach((button) => {
    button.onclick = async () => {
      try {
        const { database } = await api(`/admin/databases/${button.dataset.id}`);
        Object.entries(database).forEach(([key, value]) => {
          if (form.elements[key]) form.elements[key].value = value;
        });
        form.elements.password.required = true;
        form.elements.password.value = '';
        form.elements.password.setAttribute('aria-label', 'Enter the database password to save updated settings');
        window.scrollTo({ top: 0, behavior: 'smooth' });
      } catch (error) {
        notice(error.message, true);
      }
    };
  });

  root.querySelectorAll('.test-db').forEach((button) => {
    button.onclick = async () => {
      try {
        await api(`/admin/databases/${button.dataset.id}/test-connection`, 'POST', {});
        notice('Connection succeeded.');
      } catch (error) {
        notice(error.message, true);
      }
    };
  });

  root.querySelectorAll('.delete-db').forEach((button) => {
    button.onclick = async () => {
      if (!confirm('Delete this registration? The target database is not deleted.')) return;
      try {
        await api(`/admin/databases/${button.dataset.id}`, 'DELETE');
        adminDatabasesPage();
      } catch (error) {
        notice(error.message, true);
      }
    };
  });
}
