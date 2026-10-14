// Everything else is server-rendered PHP; this only handles the theme toggle,
// a "Searching..." state on submit, and a letter fallback for broken icons.
(function () {
  var root = document.documentElement;
  var toggle = document.getElementById('themeToggle');
  var button = document.getElementById('searchButton');

  function setTheme(theme, save) {
    root.dataset.theme = theme;
    toggle.textContent = theme === 'dark' ? 'Light mode' : 'Dark mode';
    if (save) try { localStorage.setItem('troller-theme', theme); } catch (e) {}
  }

  var saved;
  try { saved = localStorage.getItem('troller-theme'); } catch (e) {}
  setTheme(saved || (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'));
  toggle.onclick = function () { setTheme(root.dataset.theme === 'dark' ? 'light' : 'dark', 1); };

  document.getElementById('searchForm').onsubmit = function () {
    button.disabled = true;
    button.textContent = 'Searching...';
  };
  addEventListener('pageshow', function () { // back button restores the page as-is
    button.disabled = false;
    button.textContent = 'Search';
  });

  addEventListener('error', function (e) {
    var img = e.target;
    if (img.tagName !== 'IMG' || !img.dataset.initial) return;
    var blank = document.createElement('div');
    blank.className = 'icon blank';
    blank.textContent = img.dataset.initial;
    img.replaceWith(blank);
  }, true);
})();
