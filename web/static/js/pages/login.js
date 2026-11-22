import { api, brand, formData, navigate, refreshCSRF, root, state } from '../core.js';

export function loginPage() {
  document.title = 'Sign in · Atlas DB';
  root.innerHTML = `
    <main class="login-card">
      ${brand('/login')}
      <p class="eyebrow">DATABASE MANAGEMENT PLATFORM</p>
      <h1>Welcome back</h1>
      <p class="muted">Sign in to manage your database workspace.</p>
      <p id="login-notice" class="notice error" role="status" hidden></p>
      <form id="login-form" class="stack">
        <label>Username<input name="username" autocomplete="username" required autofocus></label>
        <label>Password<input name="password" type="password" autocomplete="current-password" required></label>
        <button class="button primary full">Sign in</button>
      </form>
      <p class="footnote">Your session is protected with a secure, HttpOnly cookie.</p>
    </main>`;

  root.querySelector('form').onsubmit = async (event) => {
    event.preventDefault();
    const form = event.currentTarget;
    const button = form.querySelector('button');
    button.disabled = true;
    try {
      await api('/auth/login', 'POST', formData(form));
      state.user = (await api('/auth/me')).user;
      await refreshCSRF();
      navigate('/');
    } catch (error) {
      const message = root.querySelector('#login-notice');
      message.hidden = false;
      message.textContent = error.message;
    } finally {
      button.disabled = false;
    }
  };
}
