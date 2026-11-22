import { api, esc, formData, heading, notice, panel, root, route, shell, statusNotice } from '../core.js';

const removeButton = (kind, id) => `<button class="button small ${kind}" data-id="${esc(id)}">Remove</button>`;

const userOptions = (users, selectedID) =>
  users.map((u) => `<option value="${esc(u.id)}" ${u.id === selectedID ? 'selected' : ''}>${esc(u.username)}</option>`).join('');

function groupDetail(group, users, members, databases, registered) {
  const dbName = (id) => registered.find((d) => d.id === id)?.name || id;
  const missingLeader = group.leader_id
    ? ''
    : `<p class="notice error">This group has no leader. Assign one before granting database access${
      databases.length ? '; migrations on its databases cannot be approved until then' : ''}.</p>`;
  const memberItems = members
    .map((m) => `<li>${esc(m.username)}${m.active ? '' : ' (inactive)'}${removeButton('remove-member', m.id)}</li>`)
    .join('') || '<li class="muted">No members.</li>';
  const grantItems = databases
    .map((g) => `<li>${esc(dbName(g.database_id))} · ${esc(g.permission)}${removeButton('remove-grant', g.database_id)}</li>`)
    .join('') || '<li class="muted">No database grants.</li>';
  return `
    <section class="panel">
      <h2>${esc(group.name)} · leader</h2>
      ${missingLeader}
      <form id="leader-form" class="form-row">
        <label>Group leader
          <select name="user_id"><option value="">No leader</option>${userOptions(members.filter((m) => m.active), group.leader_id)}</select>
        </label>
        <button class="button">Save leader</button>
        <span class="muted">The leader must be an active member.</span>
      </form>
      <h3>Members</h3>
      <form id="member-add" class="form-row">
        <label>Add user<select name="user_id">${userOptions(users)}</select></label>
        <button class="button small">Add member</button>
      </form>
      <ul class="item-list">${memberItems}</ul>
      <h3>Database grants</h3>
      <form id="grant-form" class="form-row">
        <label>Database
          <select name="database_id" required>${registered.map((d) => `<option value="${esc(d.id)}">${esc(d.name)}</option>`).join('')}</select>
        </label>
        <label>Permission
          <select name="permission"><option value="view_only">View only</option><option value="view_edit">View and edit</option></select>
        </label>
        <button class="button small">Save grant</button>
      </form>
      <ul class="item-list">${grantItems}</ul>
    </section>`;
}

function groupRow(group, users, selectedID) {
  const leader = group.leader_id
    ? ` · leader ${esc(users.find((u) => u.id === group.leader_id)?.username || group.leader_id)}`
    : ' · no leader';
  return `
    <div class="group-row ${group.id === selectedID ? 'selected' : ''}">
      <a data-nav href="/manage/groups?group=${encodeURIComponent(group.id)}">
        ${esc(group.name)}<span class="subline">${esc(group.id)}${leader}</span>
      </a>
      <button class="button small delete-group" data-id="${esc(group.id)}">Delete</button>
    </div>`;
}

// run performs an admin change, reports its outcome, and reloads the page.
async function run(action, success) {
  try {
    await action();
    await groupsPage();
    if (success) notice(success);
  } catch (error) {
    notice(error.message, true);
  }
}

function bindGroup(selectedID) {
  const base = `/admin/groups/${encodeURIComponent(selectedID)}`;
  root.querySelector('#leader-form')?.addEventListener('submit', (event) => {
    event.preventDefault();
    run(() => api(`${base}/leader`, 'PUT', formData(event.currentTarget)), 'Leader updated.');
  });
  root.querySelector('#member-add')?.addEventListener('submit', (event) => {
    event.preventDefault();
    run(() => api(`${base}/members/${event.currentTarget.elements.user_id.value}`, 'PUT', {}), 'Member added.');
  });
  root.querySelectorAll('.remove-member').forEach((button) => {
    button.onclick = () => run(() => api(`${base}/members/${button.dataset.id}`, 'DELETE'));
  });
  root.querySelector('#grant-form')?.addEventListener('submit', (event) => {
    event.preventDefault();
    const grant = formData(event.currentTarget);
    run(() => api(`${base}/databases/${encodeURIComponent(grant.database_id)}`, 'PUT', { permission: grant.permission }), 'Database grant saved.');
  });
  root.querySelectorAll('.remove-grant').forEach((button) => {
    button.onclick = () => run(() => api(`${base}/databases/${button.dataset.id}`, 'DELETE'));
  });
}

export async function groupsPage() {
  const [{ groups = [] }, { users = [] }, { databases: registered = [] }] = await Promise.all([
    api('/admin/groups'), api('/admin/users'), api('/admin/databases'),
  ]);
  const selectedID = new URLSearchParams(location.search).get('group') || groups[0]?.id;
  const group = groups.find((g) => g.id === selectedID);
  let detail = panel('Group details', '<p class="muted">Select a group to manage its access.</p>');
  if (group) {
    const [{ members = [] }, { databases = [] }] = await Promise.all([
      api(`/admin/groups/${selectedID}/members`),
      api(`/admin/groups/${selectedID}/databases`),
    ]);
    detail = groupDetail(group, users, members, databases, registered);
  }
  const list = groups.map((g) => groupRow(g, users, selectedID)).join('') || '<p class="muted">No groups yet.</p>';
  shell(`
    ${heading('PLATFORM ADMINISTRATION', 'Groups & access', 'Manage leaders, members, and database grants.')}
    ${statusNotice}
    ${panel('Create a group', `
      <form id="create-group" class="form-row">
        <label>Group name<input name="name" maxlength="100" required></label>
        <button class="button primary">Create group</button>
      </form>`)}
    <div class="split-layout">
      ${panel('Groups', `<nav class="group-list">${list}</nav>`)}
      ${detail}
    </div>`,
  'Groups');

  root.querySelector('#create-group').onsubmit = async (event) => {
    event.preventDefault();
    try {
      const result = await api('/admin/groups', 'POST', formData(event.currentTarget));
      route(`/manage/groups?group=${encodeURIComponent(result.group.id)}`);
      groupsPage();
    } catch (error) {
      notice(error.message, true);
    }
  };
  root.querySelectorAll('.delete-group').forEach((button) => {
    button.onclick = () => {
      if (!confirm('Delete this group and its memberships and database grants?')) return;
      run(() => api(`/admin/groups/${button.dataset.id}`, 'DELETE'));
    };
  });
  if (selectedID) bindGroup(selectedID);
}
