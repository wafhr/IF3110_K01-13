import { api, esc, formData, heading, notice, panel, root, shell, state, statusNotice, table } from '../core.js';

const selected = (condition) => (condition ? 'selected' : '');

const userRow = (u) => `
  <tr>
    <td>
      <strong>${esc(u.username)}</strong>${u.id === state.user.id ? ' <span class="small-tag">You</span>' : ''}
      <span class="subline">${esc(u.id)}</span>
    </td>
    <td>${u.active ? 'Active' : 'Inactive'}</td>
    <td>${u.platform_admin ? 'Platform Admin' : 'User'}</td>
    <td>
      <form class="inline-form user-update" data-id="${esc(u.id)}">
        <select name="active">
          <option value="true" ${selected(u.active)}>Active</option>
          <option value="false" ${selected(!u.active)}>Inactive</option>
        </select>
        <select name="platform_admin">
          <option value="false" ${selected(!u.platform_admin)}>User</option>
          <option value="true" ${selected(u.platform_admin)}>Admin</option>
        </select>
        <button class="button small">Save</button>
      </form>
    </td>
  </tr>`;

const createForm = `
  <form id="create-user" class="form-row">
    <label>Username<input name="username" minlength="3" maxlength="64" pattern="[A-Za-z0-9._\\-]+" required></label>
    <label>Temporary password<input name="password" type="password" minlength="12" required autocomplete="new-password"></label>
    <label>Platform role
      <select name="platform_admin"><option value="false">User</option><option value="true">Platform Admin</option></select>
    </label>
    <button class="button primary">Create user</button>
  </form>`;

export async function usersPage() {
  const { users = [] } = await api('/admin/users');
  shell(`
    ${heading('PLATFORM ADMINISTRATION', 'Users', 'Create accounts and manage platform roles.')}
    ${statusNotice}
    ${panel('Add a user', createForm, 'Passwords must be at least 12 characters.')}
    ${panel('All users', table(['User', 'Status', 'Role', 'Update'], users.map(userRow).join('')),
    'Platform Admin does not grant access to target databases.')}`,
  'Users');

  root.querySelector('#create-user').onsubmit = async (event) => {
    event.preventDefault();
    const user = formData(event.currentTarget);
    user.platform_admin = user.platform_admin === 'true';
    try {
      await api('/admin/users', 'POST', user);
      await usersPage();
      notice('User created.');
    } catch (error) {
      notice(error.message, true);
    }
  };

  root.querySelectorAll('.user-update').forEach((form) => {
    form.onsubmit = async (event) => {
      event.preventDefault();
      const fields = formData(form);
      try {
        await api(`/admin/users/${encodeURIComponent(form.dataset.id)}`, 'PATCH', {
          active: fields.active === 'true',
          platform_admin: fields.platform_admin === 'true',
        });
        await usersPage();
        notice('User updated.');
      } catch (error) {
        notice(error.message, true);
      }
    };
  });
}
