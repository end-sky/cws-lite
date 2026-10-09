// Package web holds the front-end script served by CWS Lite.
//
// The JavaScript lives in a Go string so it ships inside the binary together
// with index.html and style.css; main.go serves it at /scripts.js.
// The source below is plain JS; it must not contain backticks (use string
// concatenation instead of template literals).
package web

// Script is the page's JavaScript.
const Script = `(() => {
  const root = document.documentElement;
  const themeToggle = document.getElementById('themeToggle');
  const themeLabel = document.getElementById('themeLabel');
  const themeIcon = document.getElementById('themeIcon');
  const searchForm = document.getElementById('searchForm');
  const searchInput = document.getElementById('searchInput');
  const searchButton = document.getElementById('searchButton');
  const resultsSection = document.getElementById('resultsSection');
  const resultsContent = document.getElementById('resultsContent');
  const modeButtons = [...document.querySelectorAll('[data-kind]')];
  let kind = 'extensions';
  let activeController = null;

  const sun = '<circle cx="12" cy="12" r="4"/><path d="M12 2v2m0 16v2M4.93 4.93l1.42 1.42m11.3 11.3 1.42 1.42M2 12h2m16 0h2M4.93 19.07l1.42-1.42m11.3-11.3 1.42-1.42"/>';
  const moon = '<path d="M20.8 14.1A8.4 8.4 0 0 1 9.9 3.2 8.7 8.7 0 1 0 20.8 14.1Z"/>';
  const downloadIcon = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 3v12m-5-5 5 5 5-5"/><path d="M5 17v3h14v-3"/></svg>';

  function setTheme(theme) {
    const dark = theme === 'dark';
    root.dataset.theme = dark ? 'dark' : 'light';
    themeLabel.textContent = dark ? 'Light mode' : 'Dark mode';
    themeIcon.innerHTML = dark ? moon : sun;
    const meta = document.querySelector('meta[name="theme-color"]');
    if (meta) meta.content = dark ? '#11151b' : '#f6f7f9';
    try { localStorage.setItem('cws-lite-theme', dark ? 'dark' : 'light'); } catch (_) {}
  }
  let savedTheme = 'light';
  try { savedTheme = localStorage.getItem('cws-lite-theme') || 'light'; } catch (_) {}
  setTheme(savedTheme);
  themeToggle.addEventListener('click', () => setTheme(root.dataset.theme === 'dark' ? 'light' : 'dark'));

  modeButtons.forEach(button => button.addEventListener('click', () => {
    kind = button.dataset.kind;
    modeButtons.forEach(item => item.setAttribute('aria-pressed', String(item === button)));
    searchInput.placeholder = kind === 'themes' ? 'Search themes…' : 'Search extensions…';
  }));

  function showMessage(title, body, isError = false) {
    const box = document.createElement('div');
    box.className = 'message' + (isError ? ' error' : '');
    const heading = document.createElement('strong');
    heading.textContent = title;
    const text = document.createElement('div');
    text.textContent = body;
    box.append(heading, text);
    resultsContent.replaceChildren(box);
  }

  function setLoading(loading) {
    resultsSection.setAttribute('aria-busy', String(loading));
    searchButton.disabled = loading;
    searchButton.innerHTML = loading
      ? '<span class="loading-dots" aria-hidden="true"><span></span><span></span><span></span></span><span>Searching</span>'
      : '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><circle cx="10.8" cy="10.8" r="6.8"/><path d="m16 16 4.2 4.2"/></svg><span>Search</span>';
  }

  function addLink(parent, href, label, className, isDownload = false) {
    const link = document.createElement('a');
    link.href = href;
    link.className = className;
    link.textContent = label;
    if (isDownload) {
      const iconWrap = document.createElement('span');
      iconWrap.innerHTML = downloadIcon;
      link.textContent = '';
      link.append(iconWrap.firstElementChild, document.createTextNode(label));
      link.setAttribute('download', '');
    } else {
      link.target = '_blank';
      link.rel = 'noopener noreferrer';
    }
    parent.append(link);
    return link;
  }

  function renderResults(payload, query, category) {
    const results = Array.isArray(payload.results) ? payload.results : [];
    const header = document.createElement('div');
    header.className = 'results-head';
    const title = document.createElement('h2');
    title.textContent = results.length ? results.length + ' ' + (category === 'themes' ? 'themes' : 'extensions') + ' found' : 'No listings found';
    const meta = document.createElement('div');
    meta.className = 'results-meta';
    meta.textContent = payload.source ? 'Source: ' + payload.source : 'Search results';
    header.append(title, meta);

    const fragment = document.createDocumentFragment();
    fragment.append(header);
    if (!results.length) {
      const note = document.createElement('div');
      note.className = 'message';
      const strong = document.createElement('strong');
      strong.textContent = 'Nothing came back for “' + query + '”';
      const p = document.createElement('div');
      p.textContent = payload.note || 'Try a different or shorter search term.';
      note.append(strong, p);
      fragment.append(note);
    } else {
      const grid = document.createElement('div');
      grid.className = 'results-grid';
      results.forEach(item => {
        const card = document.createElement('article');
        card.className = 'result-card';
        const icon = document.createElement('div');
        icon.className = 'result-icon';
        icon.setAttribute('aria-hidden', 'true');
        if (item.icon && /^https:\/\//i.test(item.icon)) {
          const image = document.createElement('img');
          image.src = item.icon;
          image.alt = '';
          image.loading = 'lazy';
          image.referrerPolicy = 'no-referrer';
          image.addEventListener('error', () => { icon.replaceChildren(document.createTextNode((item.name || '?').trim().charAt(0).toUpperCase())); });
          icon.append(image);
        } else {
          icon.textContent = (item.name || '?').trim().charAt(0).toUpperCase();
        }
        const body = document.createElement('div');
        body.className = 'result-body';
        const h3 = document.createElement('h3');
        h3.className = 'result-title';
        const listing = document.createElement('a');
        listing.textContent = item.name || 'Chrome Web Store listing';
        listing.href = item.url || 'https://chromewebstore.google.com/detail/' + encodeURIComponent(item.id || '');
        listing.target = '_blank';
        listing.rel = 'noopener noreferrer';
        h3.append(listing);
        const kindLabel = document.createElement('div');
        kindLabel.className = 'kind-label';
        kindLabel.textContent = category === 'themes' ? 'Theme' : 'Extension';
        const description = document.createElement('p');
        description.className = 'result-description';
        description.textContent = item.description || 'Open the official listing to see the description, permissions, reviews, and publisher details.';
        const actions = document.createElement('div');
        actions.className = 'result-actions';
        const params = new URLSearchParams({ id: item.id || '', name: item.name || item.id || 'chrome-store-item', type: category });
        addLink(actions, '/api/download?' + params.toString(), 'Install / download .crx', 'download-link', true);
        addLink(actions, item.url || 'https://chromewebstore.google.com/detail/' + encodeURIComponent(item.id || ''), 'View listing ↗', 'listing-link');
        if (item.publisher) {
          const publisher = document.createElement('p');
          publisher.className = 'result-publisher';
          publisher.textContent = 'by ' + item.publisher;
          body.append(h3, publisher, kindLabel, description, actions);
        } else {
          body.append(h3, kindLabel, description, actions);
        }
        card.append(icon, body);
        grid.append(card);
      });
      fragment.append(grid);
      if (payload.note) {
        const note = document.createElement('p');
        note.className = 'search-hint';
        note.textContent = payload.note;
        fragment.append(note);
      }
    }
    resultsContent.replaceChildren(fragment);
  }

  searchForm.addEventListener('submit', async event => {
    event.preventDefault();
    const query = searchInput.value.trim();
    if (!query) {
      searchInput.focus();
      return;
    }
    if (activeController) activeController.abort();
    const controller = new AbortController();
    activeController = controller;
    setLoading(true);
    showMessage('Searching…', 'Fetching public Web Store listings.');
    try {
      const params = new URLSearchParams({ q: query, type: kind });
      const response = await fetch('/api/search?' + params.toString(), { signal: controller.signal, headers: { 'Accept': 'application/json' } });
      const payload = await response.json();
      if (!response.ok) throw new Error(payload.error || 'Search failed (HTTP ' + response.status + ').');
      renderResults(payload, query, kind);
      if (payload.source && payload.source.startsWith('public search index')) {
        const meta = resultsContent.querySelector('.results-meta');
        if (meta) meta.title = 'The Store did not expose readable results; using an indexed search fallback.';
      }
    } catch (error) {
      if (error.name === 'AbortError') return;
      showMessage('Search could not finish', (error.message || 'Unexpected error.') + ' Check the connection and open the troubleshooting section below for common fixes.', true);
    } finally {
      if (activeController === controller) setLoading(false);
    }
  });
})();
  
`
