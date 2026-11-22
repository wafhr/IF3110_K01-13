import { esc, heading, navigate, refreshCSRF, root, route, setRenderer, shell, state, api } from './core.js';
import { adminDatabasesPage } from './pages/databases.js';
import { groupsPage } from './pages/groups.js';
import { homePage } from './pages/home.js';
import { loginPage } from './pages/login.js';
import { migrationPage } from './pages/migrations.js';
import { usersPage } from './pages/users.js';
import { databasePage } from './pages/workspace.js';

const adminPages = {
  '/manage/users': usersPage,
  '/manage/groups': groupsPage,
  '/manage/databases': adminDatabasesPage,
};

async function render() {
  clearTimeout(state.poll);
  const path = location.pathname;
  try {
    if (path === '/login') return state.user ? navigate('/') : loginPage();
    if (!state.user) {
      state.user = (await api('/auth/me')).user;
      await refreshCSRF();
    }
    if (path === '/') return await homePage();
    if (adminPages[path] && state.user.platform_admin) return await adminPages[path]();
    if (path.startsWith('/workspace/databases/') && path.endsWith('/migrations')) return await migrationPage(path);
    if (path.startsWith('/workspace/databases/')) return await databasePage(path);
    return navigate('/');
  } catch (error) {
    if (error.status === 401) {
      state.user = null;
      route('/login');
      return loginPage();
    }
    shell(`
      ${heading('REQUEST FAILED', 'Unable to load this page', error.message)}
      <p id="page-notice" role="status" class="notice error">${esc(error.message)}</p>
      <a class="button" data-nav href="/">Back to databases</a>`,
    'Error');
  }
}

setRenderer(render);

root.addEventListener('click', (event) => {
  const link = event.target.closest('a[data-nav]');
  if (!link || event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
  event.preventDefault();
  navigate(link.pathname + link.search + link.hash);
});
addEventListener('popstate', render);
render();
