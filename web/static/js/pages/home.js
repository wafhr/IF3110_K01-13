import { api, esc, heading, pageDatabase, panel, shell, state, statusNotice } from '../core.js';

const adminCards = `
  <section class="card-grid">
    <a class="feature-card" data-nav href="/manage/users">
      <h2>Users</h2><p>Manage accounts and platform roles.</p>
    </a>
    <a class="feature-card" data-nav href="/manage/groups">
      <h2>Groups & access</h2><p>Manage leaders, members, and database grants.</p>
    </a>
    <a class="feature-card" data-nav href="/manage/databases">
      <h2>Target databases</h2><p>Register and maintain connections.</p>
    </a>
  </section>`;

const databaseCard = (db) => `
  <a class="feature-card" data-nav href="${pageDatabase(db.id)}">
    <span class="eyebrow">${esc(db.dbms === 'mysql' ? 'MYSQL' : 'POSTGRESQL')} · ${esc(db.permission)}</span>
    <h2>${esc(db.name)}</h2>
    <p>${esc(db.database_name)}</p>
    <span class="card-link">Open workspace →</span>
  </a>`;

export async function homePage() {
  state.dbs = (await api('/databases')).databases || [];
  const cards = state.dbs.map(databaseCard).join('') || '<p class="muted">No databases are available to your account.</p>';
  shell(`
    ${heading('WORKSPACE', `Good to see you, ${state.user.username}`, 'Database access is managed through group membership.')}
    ${state.user.platform_admin ? adminCards : ''}
    ${panel('Your databases', `${statusNotice}<div class="card-grid">${cards}</div>`)}`,
  'Workspace');
}
