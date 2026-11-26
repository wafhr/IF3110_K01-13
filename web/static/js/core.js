export const root = document.querySelector('#app');
export const state = { user: null, csrf: '', dbs: [], poll: null };

const entities = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };
export const esc = (value) => String(value ?? '').replace(/[&<>"']/g, (c) => entities[c]);

export const apiDatabase = (id) => `/databases/${encodeURIComponent(id)}`;
export const pageDatabase = (id) => `/workspace/databases/${encodeURIComponent(id)}`;
export const databaseIdFrom = (path) => path.split('/')[3] || '';

let renderPage = async () => {};

// setRenderer registers the router so pages can navigate without importing it.
export function setRenderer(render) {
  renderPage = render;
}

export const route = (path) => history.pushState({}, '', path);

export async function navigate(path) {
  const previousPath = location.pathname;
  route(path);
  const destinationPath = location.pathname;
  const result = await renderPage();
  if (destinationPath !== previousPath && location.pathname === destinationPath) {
    window.scrollTo(0, 0);
  }
  return result;
}

export async function api(path, method = 'GET', body) {
  const headers = { Accept: 'application/json' };
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (method !== 'GET' && method !== 'HEAD') headers['X-CSRF-Token'] = state.csrf;
  const response = await fetch(path, {
    method,
    credentials: 'same-origin',
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const data = response.status === 204 ? {} : await response.json().catch(() => ({}));
  if (!response.ok) {
    const error = new Error(data.error || `Request failed (${response.status})`);
    error.status = response.status;
    error.data = data;
    throw error;
  }
  return data;
}

export async function refreshCSRF() {
  try {
    state.csrf = (await api('/auth/csrf')).csrf_token || '';
  } catch {
    state.csrf = '';
  }
}

// accessibleDatabase finds a database the signed-in user can open.
export async function accessibleDatabase(id) {
  const db = state.dbs.find((x) => x.id === id) || (await api('/databases')).databases.find((x) => x.id === id);
  if (!db) throw new Error('Database access is unavailable for this account.');
  return db;
}

export const actorLabel = (id) => (id === state.user.id ? `${state.user.username} (you)` : id);

export function notice(message, error = false) {
  const node = root.querySelector('[role="status"]');
  if (!node) return;
  node.textContent = message;
  node.className = error ? 'notice error' : 'notice';
  node.hidden = !message;
}

export const brand = (href) => `
  <a class="brand" data-nav href="${href}">
    <span class="brand-mark">A</span><span>Atlas<span class="brand-light">DB</span></span>
  </a>`;

export function shell(content, title = 'Atlas DB') {
  document.title = `${title} · Atlas DB`;
  const adminLinks = state.user?.platform_admin
    ? `<a class="button ghost" data-nav href="/manage/users">Users</a>
       <a class="button ghost" data-nav href="/manage/groups">Groups</a>
       <a class="button ghost" data-nav href="/manage/databases">Manage databases</a>`
    : '';
  root.innerHTML = `
    <header class="topbar">
      ${brand('/')}
      <nav class="top-actions" aria-label="Main navigation">
        <a class="button ghost" data-nav href="/">Databases</a>
        ${adminLinks}
        <span class="user-chip">${esc(state.user?.username)}</span>
        <button class="button ghost" id="logout">Sign out</button>
      </nav>
    </header>
    <main class="container">${content}</main>`;
  root.querySelector('#logout')?.addEventListener('click', async () => {
    try {
      await api('/auth/logout', 'POST', {});
    } finally {
      state.user = null;
      navigate('/login');
    }
  });
}

export const heading = (eyebrow, title, description = '') => `
  <div class="page-heading"><div>
    <p class="eyebrow">${esc(eyebrow)}</p>
    <h1>${esc(title)}</h1>
    ${description ? `<p class="muted">${esc(description)}</p>` : ''}
  </div></div>`;

export const panel = (title, body, description = '') => `
  <section class="panel">
    <div class="panel-heading"><div>
      <h2>${esc(title)}</h2>
      ${description ? `<p class="muted">${esc(description)}</p>` : ''}
    </div></div>
    ${body}
  </section>`;

export const table = (headers, rows) => `
  <div class="table-wrap"><table>
    <thead><tr>${headers.map((x) => `<th>${esc(x)}</th>`).join('')}</tr></thead>
    <tbody>${rows || `<tr><td colspan="${headers.length}" class="empty-cell">No records.</td></tr>`}</tbody>
  </table></div>`;

export const statusNotice = '<p id="page-notice" role="status" hidden></p>';

// formData returns the named fields of a form as a plain object.
export const formData = (form) => Object.fromEntries(new FormData(form));
