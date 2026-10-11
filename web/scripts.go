// Package web holds the front-end script served by CWS Lite.
//
// The JavaScript lives in a Go string so it ships inside the binary together
// with index.html and style.css; main.go serves it at /scripts.js.
// The source below is plain JS; it must not contain backticks (use string
// concatenation instead of template literals).
package web

// Script is the page's JavaScript.
const Script = `(function () {
  'use strict';
  var root = document.documentElement;
  var themeToggle = document.getElementById('themeToggle');
  var searchForm = document.getElementById('searchForm');
  var searchInput = document.getElementById('searchInput');
  var searchButton = document.getElementById('searchButton');
  var resultsSection = document.getElementById('resultsSection');
  var resultsContent = document.getElementById('resultsContent');
  var activeController = null;

  var STORES = {
    google: { name: 'Chrome Web Store', ext: '.crx', action: 'Download .crx', noun: { extensions: 'Extension', themes: 'Theme' } },
    firefox: { name: 'Firefox Add-ons', ext: '.xpi', action: 'Install .xpi', noun: { extensions: 'Add-on', themes: 'Theme' } }
  };

  /* ---- theme ---- */
  function setTheme(theme, save) {
    var dark = theme === 'dark';
    root.dataset.theme = dark ? 'dark' : 'light';
    themeToggle.textContent = dark ? 'Light mode' : 'Dark mode';
    var meta = document.querySelector('meta[name="theme-color"]');
    if (meta) meta.content = dark ? '#1a1a1a' : '#ffffff';
    if (save) { try { localStorage.setItem('cws-lite-theme', theme); } catch (_) {} }
  }
  var saved = null;
  try { saved = localStorage.getItem('cws-lite-theme'); } catch (_) {}
  if (saved !== 'dark' && saved !== 'light') {
    saved = window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
  }
  setTheme(saved, false);
  themeToggle.addEventListener('click', function () {
    setTheme(root.dataset.theme === 'dark' ? 'light' : 'dark', true);
  });

  /* ---- helpers ---- */
  function el(tag, className, text) {
    var node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined) node.textContent = text;
    return node;
  }

  function showMessage(text, isError) {
    resultsContent.replaceChildren(el('p', 'message' + (isError ? ' error' : ''), text));
  }

  function setLoading(loading) {
    resultsSection.setAttribute('aria-busy', String(loading));
    searchButton.disabled = loading;
    searchButton.textContent = loading ? 'Searching...' : 'Search';
  }

  function displayURL(raw) {
    try {
      var u = new URL(raw);
      return u.hostname + u.pathname.replace(/\/$/, '');
    } catch (_) { return raw; }
  }

  function placeholderIcon(item) {
    return el('div', 'icon blank', (item.name || '?').trim().charAt(0).toUpperCase());
  }

  function iconFor(item) {
    if (!item.icon || !/^https:\/\//i.test(item.icon)) return placeholderIcon(item);
    var img = document.createElement('img');
    img.className = 'icon';
    img.src = item.icon;
    img.alt = '';
    img.width = 32;
    img.height = 32;
    img.loading = 'lazy';
    img.referrerPolicy = 'no-referrer';
    img.addEventListener('error', function () { img.replaceWith(placeholderIcon(item)); });
    return img;
  }

  function listingURL(item) {
    return item.url || 'https://chromewebstore.google.com/detail/' + encodeURIComponent(item.id || '');
  }

  function link(href, label, className, external) {
    var a = el('a', className, label);
    a.href = href;
    if (external) { a.target = '_blank'; a.rel = 'noopener noreferrer'; }
    return a;
  }

  /* ---- rendering ---- */
  function renderResults(payload, query, store, kind) {
    var info = STORES[store];
    var results = Array.isArray(payload.results) ? payload.results : [];
    var fragment = document.createDocumentFragment();

    var bar = el('div', 'bar');
    var left = el('span');
    left.append('Searched ', el('b', '', info.name), ' for ', el('b', '', query));
    bar.append(left, el('span', '', results.length + ' ' + kind));
    fragment.append(bar);

    if (!results.length) {
      var empty = el('div', 'message');
      empty.append(el('p', '', 'Nothing came back for "' + query + '".'));
      empty.append(el('p', 'note', payload.note || 'Try a different or shorter search term.'));
      fragment.append(empty);
      resultsContent.replaceChildren(fragment);
      return;
    }

    var list = el('ol', 'results');
    results.forEach(function (item) {
      var row = el('li', 'result');
      var body = el('div', 'body');

      var title = el('h3');
      title.append(link(listingURL(item), item.name || 'Listing', '', true));
      if (item.publisher) title.append(el('span', 'by', ' - by ' + item.publisher));
      body.append(title);

      body.append(el('div', 'desc', item.description || 'No description available.'));

      var meta = el('div', 'meta', displayURL(listingURL(item)) + ' - ' + info.noun[kind]);
      var params = new URLSearchParams({ store: store, id: item.id || '', name: item.name || item.id || 'download', type: kind });
      meta.append(link('/api/download?' + params.toString(), info.action, 'dl'));
      body.append(meta);

      row.append(iconFor(item), body);
      list.append(row);
    });
    fragment.append(list);
    if (payload.note) fragment.append(el('p', 'note', payload.note));
    resultsContent.replaceChildren(fragment);
  }

  /* ---- search ---- */
  searchForm.addEventListener('submit', function (event) {
    event.preventDefault();
    var query = searchInput.value.trim();
    if (!query) { searchInput.focus(); return; }
    var data = new FormData(searchForm);
    var store = data.get('store') === 'firefox' ? 'firefox' : 'google';
    var kind = data.get('kind') === 'themes' ? 'themes' : 'extensions';

    if (activeController) activeController.abort();
    var controller = new AbortController();
    activeController = controller;
    setLoading(true);
    showMessage('Searching ' + STORES[store].name + '...', false);

    var params = new URLSearchParams({ q: query, type: kind, store: store });
    fetch('/api/search?' + params.toString(), { signal: controller.signal, headers: { Accept: 'application/json' } })
      .then(function (response) {
        return response.json().then(function (payload) {
          if (!response.ok) throw new Error(payload.error || 'Search failed (HTTP ' + response.status + ').');
          renderResults(payload, query, store, kind);
        });
      })
      .catch(function (error) {
        if (error.name === 'AbortError') return;
        showMessage((error.message || 'Unexpected error.') + ' Check your connection, then see "Trouble installing" below for common fixes.', true);
      })
      .then(function () {
        if (activeController === controller) setLoading(false);
      });
  });
})();
`
