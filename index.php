<?php
// Troller - front controller. Routing, the JSON API and the HTML page.
//
//   php -S 127.0.0.1:8080 index.php          (built-in dev server)
//   or point Apache/nginx at this directory and send everything here.
//
//   GET /                      search page (server-rendered)
//   GET /api/search?q=…&type=extensions|themes&store=google|firefox
//   GET /api/download?store=…&id=…&name=…
//   GET /api/health

declare(strict_types=1);

require __DIR__ . '/src/common.php';
require __DIR__ . '/src/cache.php';
require __DIR__ . '/src/http.php';
require __DIR__ . '/src/firefox.php';
require __DIR__ . '/src/google.php';

send_security_headers();

$method = $_SERVER['REQUEST_METHOD'] ?? 'GET';
$path = parse_url($_SERVER['REQUEST_URI'] ?? '/', PHP_URL_PATH) ?: '/';
$path = $path === '/' ? '/' : rtrim($path, '/');

if ($method !== 'GET' && $method !== 'HEAD') {
    header('Allow: GET, HEAD');
    plain_error(405, 'Method Not Allowed');
    return;
}

switch ($path) {
    case '/api/search':
        [$status, $payload] = run_search(
            (string)($_GET['q'] ?? ''),
            (string)($_GET['type'] ?? ''),
            (string)($_GET['store'] ?? '')
        );
        write_json($status, $status === 200 ? response_for_json($payload) : $payload);
        return;

    case '/api/download':
        $store = strtolower(trim((string)($_GET['store'] ?? ''))) ?: DEFAULT_STORE;
        if ($store === 'google') {
            cws_download();
        } elseif ($store === 'firefox') {
            amo_download();
        } else {
            plain_error(400, 'store must be google or firefox');
        }
        return;

    case '/api/health':
        write_json(200, ['status' => 'ok', 'version' => TROLLER_VERSION]);
        return;

    case '/style.css':
        header('Content-Type: text/css; charset=utf-8');
        readfile(__DIR__ . '/static/style.css');
        return;

    case '/scripts.js':
        header('Content-Type: text/javascript; charset=utf-8');
        readfile(__DIR__ . '/static/scripts.js');
        return;

    case '/':
        render_page();
        return;

    default:
        plain_error(404, '404 page not found');
}

// ---------------------------------------------------------------------------
// HTML page
// ---------------------------------------------------------------------------

function h(string $s): string
{
    return htmlspecialchars($s, ENT_QUOTES | ENT_SUBSTITUTE, 'UTF-8');
}

function display_url(string $raw): string
{
    $u = parse_url($raw);
    if ($u === false || !isset($u['host'])) {
        return $raw;
    }
    return $u['host'] . rtrim($u['path'] ?? '', '/');
}

function render_page(): void
{
    $q = trim((string)($_GET['q'] ?? ''));
    $store = (($_GET['store'] ?? '') === 'firefox') ? 'firefox' : 'google';
    $kind = (($_GET['kind'] ?? '') === 'themes') ? 'themes' : 'extensions';

    $payload = null;
    $error = null;
    if ($q !== '') {
        [$status, $result] = run_search($q, $kind, $store);
        if ($status === 200) {
            $payload = $result;
        } else {
            $error = (string)($result['error'] ?? 'Search failed.');
        }
    }

    $stores = [
        'google' => ['name' => 'Chrome Web Store', 'action' => 'Download .crx', 'noun' => ['extensions' => 'Extension', 'themes' => 'Theme']],
        'firefox' => ['name' => 'Firefox Add-ons', 'action' => 'Install .xpi', 'noun' => ['extensions' => 'Add-on', 'themes' => 'Theme']],
    ];
    $info = $stores[$store];

    header('Content-Type: text/html; charset=utf-8');
    header('Cache-Control: no-store');
    ?>
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="color-scheme" content="light dark">
  <meta name="theme-color" content="#ffffff">
  <title>Troller</title>
  <link rel="icon" href="data:,">
  <link rel="stylesheet" href="/style.css">
</head>
<body>
  <div class="topnav">
    <button class="linkish" id="themeToggle" type="button">Dark mode</button>
  </div>

  <div class="page">
    <h1 class="wordmark">Troll<span> er</span></h1>
    <p class="tagline">Browser extensions &amp; themes, nothing else.</p>

    <form id="searchForm" role="search" class="searchbox" method="get" action="/">
      <input id="searchInput" name="q" type="text" maxlength="120" autocomplete="off" autofocus aria-label="Search" required value="<?= h($q) ?>">
      <div class="buttons">
        <button id="searchButton" type="submit">Search</button>
      </div>

      <div class="options">
        <fieldset>
          <legend>Store</legend>
          <label><input type="radio" name="store" value="google"<?= $store === 'google' ? ' checked' : '' ?>> Chrome Web Store <small>(.crx)</small></label>
          <label><input type="radio" name="store" value="firefox"<?= $store === 'firefox' ? ' checked' : '' ?>> Firefox Add-ons <small>(.xpi)</small></label>
        </fieldset>
        <fieldset>
          <legend>Type</legend>
          <label><input type="radio" name="kind" value="extensions"<?= $kind === 'extensions' ? ' checked' : '' ?>> Extensions</label>
          <label><input type="radio" name="kind" value="themes"<?= $kind === 'themes' ? ' checked' : '' ?>> Themes</label>
        </fieldset>
      </div>
    </form>

    <div id="resultsSection" aria-live="polite" aria-busy="false">
      <div id="resultsContent">
<?php if ($error !== null): ?>
        <p class="message error"><?= h($error) ?></p>
<?php elseif ($payload !== null): $results = $payload['results']; ?>
        <div class="bar">
          <span>Searched <b><?= h($info['name']) ?></b> for <b><?= h($q) ?></b></span>
          <span><?= count($results) ?> <?= h($kind) ?></span>
        </div>
<?php   if (!$results): ?>
        <div class="message">
          <p>Nothing came back for "<?= h($q) ?>".</p>
          <p class="note"><?= h($payload['note'] !== '' ? $payload['note'] : 'Try a different or shorter search term.') ?></p>
        </div>
<?php   else: ?>
        <ol class="results">
<?php     foreach ($results as $item):
            $listingUrl = $item['url'] !== '' ? $item['url'] : 'https://chromewebstore.google.com/detail/' . rawurlencode($item['id']);
            $dl = '/api/download?' . http_build_query([
                'store' => $store,
                'id' => $item['id'],
                'name' => $item['name'] !== '' ? $item['name'] : $item['id'],
                'type' => $kind,
            ]);
            $initial = mb_strtoupper(mb_substr(trim($item['name']), 0, 1)) ?: '?';
?>
          <li class="result">
<?php       if ($item['icon'] !== ''): ?>
            <img class="icon" src="<?= h($item['icon']) ?>" alt="" width="32" height="32" loading="lazy" referrerpolicy="no-referrer" data-initial="<?= h($initial) ?>">
<?php       else: ?>
            <div class="icon blank"><?= h($initial) ?></div>
<?php       endif; ?>
            <div class="body">
              <h3><a href="<?= h($listingUrl) ?>" target="_blank" rel="noopener noreferrer"><?= h($item['name'] !== '' ? $item['name'] : 'Listing') ?></a><?php if ($item['publisher'] !== ''): ?><span class="by"> - by <?= h($item['publisher']) ?></span><?php endif; ?></h3>
              <div class="desc"><?= h($item['description'] !== '' ? $item['description'] : 'No description available.') ?></div>
              <div class="meta"><?= h(display_url($listingUrl)) ?> - <?= h($info['noun'][$kind]) ?><a class="dl" href="<?= h($dl) ?>"><?= h($info['action']) ?></a></div>
            </div>
          </li>
<?php     endforeach; ?>
        </ol>
<?php     if ($payload['note'] !== ''): ?>
        <p class="note"><?= h($payload['note']) ?></p>
<?php     endif; ?>
<?php   endif; ?>
<?php endif; ?>
      </div>
    </div>

    <details class="help">
      <summary>Trouble installing a download?</summary>
      <h3>Chrome Web Store (.crx)</h3>
      <p><b>Ungoogled Chromium:</b> open <code>chrome://flags/#extension-mime-request-handling</code>, set <i>Handling of extension MIME type requests</i> to <i>Always prompt for install</i>, then relaunch. If the flag is missing, open <code>chrome://extensions</code>, enable <i>Developer mode</i> and drag the <code>.crx</code> file onto the page.</p>
      <p><b>Chromium / Chrome:</b> try Developer mode and drag-and-drop. Some builds and managed devices block local CRX installs; use the listing page instead.</p>
      <h3>Firefox Add-ons (.xpi)</h3>
      <p>Firefox should offer to install right after clicking the link. If it only saves the file, open <code>about:addons</code>, click the gear icon and choose <i>Install Add-on From File…</i>. Packages are the original signed files from addons.mozilla.org.</p>
      <h3>Invalid package, or no prompt</h3>
      <p>Right-click the link and choose <i>Save link as</i>, then retry from the local file. Make sure the file is not zero bytes and that a proxy did not replace it with an error page.</p>
      <h3>No results</h3>
      <p>The Chrome Web Store changes often and does not offer a public API, so its listings are read from page HTML. Try a shorter search or check your connection. Troller does not try to get around logins or anti-bot checks.</p>
    </details>

    <hr>
    <p class="footer">
      <a href="https://chromewebstore.google.com/" target="_blank" rel="noopener noreferrer">Chrome Web Store</a> -
      <a href="https://addons.mozilla.org/" target="_blank" rel="noopener noreferrer">Firefox Add-ons</a><br>
      Troller <?= h(TROLLER_VERSION) ?> - small, local and open source
    </p>
  </div>

  <script src="/scripts.js" defer></script>
</body>
</html>
<?php
}
