<?php
// Offline unit tests for the parsers. Run: php tests/smoke.php
declare(strict_types=1);

require __DIR__ . '/../src/common.php';
require __DIR__ . '/../src/cache.php';
require __DIR__ . '/../src/http.php';
require __DIR__ . '/../src/firefox.php';
require __DIR__ . '/../src/google.php';

$failed = 0;
$total = 0;
function check(string $label, mixed $got, mixed $want): void
{
    global $failed, $total;
    $total++;
    if ($got === $want) {
        echo "  ok   $label\n";
        return;
    }
    $failed++;
    echo "  FAIL $label\n       got:  " . json_encode($got, JSON_UNESCAPED_UNICODE) . "\n       want: " . json_encode($want, JSON_UNESCAPED_UNICODE) . "\n";
}

$uboId = 'cjpalhdlnbpafiamejdnhcphjbkeiagm';
$fooId = 'abcdefghijklmnopabcdefghijklmnop';
$myId = 'ponmlkjihgfedcbaponmlkjihgfedcba';

echo "common\n";
check('safe_filename spaces/symbols', safe_filename(' My Cool/Ext! v2 '), 'My-Cool-Ext--v2'); // same as the Go original: '-' is allowed, so runs aren't merged
check('safe_filename empty', safe_filename('///'), 'store-item');
check('truncate_runes ellipsis', truncate_runes(str_repeat('é', 10), 5), 'éééé…');
check('first_non_empty', first_non_empty('', '  ', 'x', 'y'), 'x');

echo "embedded AF_initDataCallback blob\n";
$page = '<html><script>AF_initDataCallback({key: \'ds:1\', hash: \'2\', data:[[["' . $fooId . '",null,"https://lh3.googleusercontent.com/abc","Dark Reader \\"Pro\\"",4.7,1000,"x","Dark mode for every website. Take care of your eyes, use dark theme for night."]]], sideChannel: {}});</script></html>';
$r = cws_extract_store_results($page, 'extensions', 'https://chromewebstore.google.com/search/x');
check('one listing', count($r), 1);
check('id', $r[0]['id'] ?? null, $fooId);
check('name (escaped quotes survive)', $r[0]['name'] ?? null, 'Dark Reader "Pro"');
check('icon', $r[0]['icon'] ?? null, 'https://lh3.googleusercontent.com/abc');
check('description', $r[0]['description'] ?? null, 'Dark mode for every website. Take care of your eyes, use dark theme for night.');
check('kind', $r[0]['kind'] ?? null, 'extensions');
check('balanced_array handles brackets in strings', cws_balanced_array('["a]","b[",[1]] tail'), '["a]","b[",[1]]');
check('balanced_array unterminated', cws_balanced_array('[1,[2]'), '');

echo "DuckDuckGo fallback\n";
$uddg = urlencode('https://chromewebstore.google.com/detail/ublock-origin/' . $uboId . '?utm_source=x');
$ddg = '<div class="result"><h2 class="result__title"><a class="result__a" href="//duckduckgo.com/l/?uddg=' . $uddg . '&amp;rut=abc">uBlock Origin - Chrome Web Store</a></h2>'
    . '<a class="result__snippet" href="//duckduckgo.com/l/?uddg=' . $uddg . '&amp;rut=abc">Finally, an efficient blocker. <b>Easy</b> on CPU and memory.</a></div>';
$r = cws_extract_store_results($ddg, 'extensions', 'https://html.duckduckgo.com/html/?q=x');
check('merged into one', count($r), 1);
check('name', $r[0]['name'] ?? null, 'uBlock Origin');
check('description', $r[0]['description'] ?? null, 'Finally, an efficient blocker. Easy on CPU and memory.');
check('tracking params dropped', $r[0]['url'] ?? null, 'https://chromewebstore.google.com/detail/ublock-origin/' . $uboId);

echo "Bing fallback\n";
$b64 = 'a1' . rtrim(strtr(base64_encode('https://chromewebstore.google.com/detail/foo/' . $fooId), '+/', '-_'), '=');
$bing = '<li><h2><a href="https://www.bing.com/ck/a?!&amp;&amp;p=x&amp;u=' . $b64 . '&amp;ntb=1">Foo Ext - Chrome Web Store</a></h2><div class="b_caption"><p class="b_lineclamp2">Foo does bar things.</p></div></li>';
$r = cws_extract_store_results($bing, 'extensions', 'https://www.bing.com/search?q=x');
check('one listing', count($r), 1);
check('id decoded from bing token', $r[0]['id'] ?? null, $fooId);
check('name', $r[0]['name'] ?? null, 'Foo Ext');
check('snippet', $r[0]['description'] ?? null, 'Foo does bar things.');

echo "Store card\n";
$card = '<a href="/detail/my-ext/' . $myId . '?hl=en"><img src="https://lh3.googleusercontent.com/i"><h2>My Ext</h2><div>Offered by Acme Inc</div><div>A very useful extension description.</div><div>4.5</div></a>'
    . '<a href="/about">Not a listing</a>';
$r = cws_extract_store_results($card, 'themes', 'https://chromewebstore.google.com/search/x');
check('one listing', count($r), 1);
check('name', $r[0]['name'] ?? null, 'My Ext');
check('publisher', $r[0]['publisher'] ?? null, 'Acme Inc');
check('description (rating line skipped)', $r[0]['description'] ?? null, 'A very useful extension description.');
check('icon', $r[0]['icon'] ?? null, 'https://lh3.googleusercontent.com/i');
check('url canonical', $r[0]['url'] ?? null, 'https://chromewebstore.google.com/detail/my-ext/' . $myId);
check('kind', $r[0]['kind'] ?? null, 'themes');

echo "URL helpers\n";
check('non-store host rejected', cws_normalize_store_href('https://evil.example/detail/x/' . $fooId, 'https://x/'), '');
check('legacy store ok', cws_extract_id_from_url('https://chrome.google.com/webstore/detail/name/' . $fooId), $fooId);
check('id from ?id=', cws_extract_id_from_url('https://chromewebstore.google.com/detail?id=' . $fooId), $fooId);
check('relative resolve', cws_resolve_url('../detail/x', 'https://a.example/search/q'), 'https://a.example/detail/x');
check('protocol-relative resolve', cws_resolve_url('//b.example/p', 'https://a.example/q'), 'https://b.example/p');
check('inside_heading true', cws_inside_heading('<h2 class="t"><a href="x">', 18), true);
check('inside_heading false', cws_inside_heading('<h2>a</h2><div><a href="x">', 21), false);

echo "detail page\n";
$detail = '<html><head><title>Fallback - Chrome Web Store</title>'
    . '<script type="application/ld+json">{"@type":"SoftwareApplication","name":"Great Tool - Chrome Web Store","description":"Does great things.","author":{"@type":"Person","name":"Jane Dev"},"image":{"url":"https://lh3.googleusercontent.com/z"}}</script>'
    . '<meta property="og:title" content="OG title"></head><body></body></html>';
$d = cws_parse_detail_page($detail);
check('name from JSON-LD, suffix stripped', $d['name'], 'Great Tool');
check('publisher', $d['publisher'], 'Jane Dev');
check('description', $d['description'], 'Does great things.');
check('icon', $d['icon'], 'https://lh3.googleusercontent.com/z');
$d = cws_parse_detail_page('<head><title>Only Title - Chrome Web Store</title></head><body><div>Offered by</div><div>Bob Inc</div></body>');
check('title fallback', $d['name'], 'Only Title');
check('"Offered by" on next line', $d['publisher'], 'Bob Inc');

echo "Firefox\n";
$items = amo_listings_from([
    ['slug' => 'ublock-origin', 'name' => ['en-US' => 'uBlock <b>Origin</b>', 'de' => 'x'], 'summary' => ['de' => 'Nur deutsch &amp; mehr'],
     'icon_url' => 'https://addons.cdn.mozilla.net/i.png', 'url' => 'https://addons.mozilla.org/en-US/firefox/addon/ublock-origin/',
     'authors' => [['name' => '', 'username' => 'gorhill']]],
    ['slug' => 'ublock-origin', 'name' => 'dupe'],
    ['slug' => 'bad id!', 'name' => 'bad'],
    ['id' => 42, 'slug' => '', 'name' => '', 'icon_url' => 'http://insecure/x.png', 'url' => 'https://evil.example/'],
], 'extensions');
check('dupes and bad ids dropped', count($items), 2);
check('localized name + tags stripped', $items[0]['name'], 'uBlock Origin');
check('fallback to any language + entities', $items[0]['description'], 'Nur deutsch & mehr');
check('publisher falls back to username', $items[0]['publisher'], 'gorhill');
check('numeric id fallback', $items[1]['id'], '42');
check('empty name -> id', $items[1]['name'], '42');
check('foreign url replaced', $items[1]['url'], 'https://addons.mozilla.org/en-US/firefox/addon/42/');
check('insecure icon dropped', $items[1]['icon'], '');
check('download allowlist ok', amo_download_url_allowed('https://addons.mozilla.org/x.xpi'), true);
check('download allowlist rejects', amo_download_url_allowed('https://evil.example/x.xpi'), false);
check('download allowlist rejects http', amo_download_url_allowed('http://addons.mozilla.org/x.xpi'), false);
check('json omitempty', array_keys(listing_for_json($items[1])), ['id', 'name', 'url', 'kind']);

echo "\n$total checks, $failed failed\n";
exit($failed ? 1 : 0);
