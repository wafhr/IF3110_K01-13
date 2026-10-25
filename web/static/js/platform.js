window.Atlas = (() => {
  const csrf = () => document.querySelector('meta[name="csrf-token"]')?.content || '';
  async function request(url, method = 'GET', body) {
    const headers = {Accept: 'application/json'};
    if (body !== undefined) {
      headers['Content-Type'] = 'application/json';
    }
    if (method !== 'GET' && method !== 'HEAD') {
      headers['X-CSRF-Token'] = csrf();
    }
    const response = await fetch(url, {method, headers, body: body === undefined ? undefined : JSON.stringify(body)});
    const data = await response.json().catch(() => ({}));
    if (!response.ok) { const error=new Error(data.error || 'Request failed. Please try again.'); error.data=data; error.status=response.status; throw error; }
    return data;
  }

  function setNotice(element, message, isError = false) {
    element.hidden = !message;
    element.textContent = message || '';
    element.className = isError ? 'notice error' : 'notice';
  }

  function setLoading(element, loading, label = 'Loading…') {
    element.setAttribute('aria-busy', String(loading));
    if (loading) element.innerHTML = `<tr><td colspan="99" class="empty-cell">${label}</td></tr>`;
  }

  function pagination({offset, limit, hasMore, onChange}) {
    const nav = document.createElement('nav');
    nav.className = 'pagination';
    const previous = document.createElement('button'); previous.className = 'button small'; previous.textContent = 'Previous'; previous.disabled = offset === 0;
    previous.onclick = () => onChange(Math.max(0, offset - limit));
    const next = document.createElement('button'); next.className = 'button small'; next.textContent = 'Next'; next.disabled = !hasMore;
    next.onclick = () => onChange(offset + limit);
    nav.append(previous, next); return nav;
  }
  return {request, setNotice, setLoading, pagination};
})();
