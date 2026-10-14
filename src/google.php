<?php
// Chrome Web Store: HTML scraper, detail-page enrichment and CRX download.
//
// Extraction is layered so that a single layout change doesn't blank out the
// results:
//
//  1. Store pages: embedded AF_initDataCallback JSON blobs (the Store is a
//     client-rendered app and its data lives there), then plain <a> cards.
//  2. Search-index fallbacks (DuckDuckGo / Bing): title + snippet anchors.
//  3. Enrichment: every listing missing a title, description or publisher is
//     completed from its own detail page (JSON-LD, Open Graph tags, "Offered
//     by" text). Search pages never carry the publisher.

declare(strict_types=1);

const CWS_PLACEHOLDER_NAME = 'Chrome Web Store listing';
const CWS_DETAIL_TIMEOUT = 6.0;
const CWS_ENRICH_BUDGET = 10.0;
const CWS_ENRICH_CONCURRENCY = 6;
const CWS_DETAIL_LIFETIME = 1800;
const CWS_MAX_CACHED_DETAILS = 512;
const CWS_MAX_EMBEDDED_BLOB = 4 << 20;
const CWS_DEFAULT_PRODVERSION = '156.0.0.0';

const CWS_ANCHOR_RE = '~<a\b([^>]*)>(.*?)</a\s*>~isu';
const CWS_ATTRIBUTE_RE = '~([a-zA-Z_:][-a-zA-Z0-9_:.]*)\s*=\s*(?:"([^"]*)"|\'([^\']*)\'|([^\s>]+))~isu';
const CWS_TAG_RE = '~<[^>]+>~su';
const CWS_SCRIPT_STYLE_RE = '~<script\b[^>]*>.*?</script\s*>|<style\b[^>]*>.*?</style\s*>|<noscript\b[^>]*>.*?</noscript\s*>~isu';
const CWS_BLOCK_END_RE = '~</(?:div|p|h[1-6]|span|li|section|article|button|header|footer|strong|em|br|a|dd|dt|td|th)\s*>|<br\s*/?>|<(?:div|p|h[1-6]|li|section|article|header|footer)\b[^>]*>~iu';
const CWS_SPACE_RE = '~[\t\x0b\f\r ]+~u';
const CWS_BARE_ID_RE = '~^[a-p]{32}\z~';
const CWS_STORE_ID_RE = '~(?:/detail/[^/?#]*/|/detail/|[?&]id=)([a-p]{32})(?:[/?#&]|\z)~i';
const CWS_STORE_HOST_RE = '~(?:^|\.)chromewebstore\.google\.com\z~i';
const CWS_LEGACY_HOST_RE = '~(?:^|\.)chrome\.google\.com\z~i';

const CWS_HEADING_OPEN_RE = '~<h[1-6][\s>]~';
const CWS_HEADING_RE = '~<h[1-6]\b[^>]*>(.*?)</h[1-6]\s*>~is';
const CWS_IMG_RE = '~<img\b([^>]*)>~is';
const CWS_META_RE = '~<meta\b([^>]*)>~is';
const CWS_TITLE_RE = '~<title\b[^>]*>(.*?)</title\s*>~is';
const CWS_JSONLD_RE = '~<script\b[^>]*type\s*=\s*["\']application/ld\+json["\'][^>]*>(.*?)</script\s*>~is';
const CWS_STORE_SUFFIX_RE = '~\s*[-–—|·:]\s*Chrome Web Store\s*$~iu';
const CWS_BYLINE_RE = '~^(?:offered|published|developed|provided)?\s*by\s*:?\s*(.+)$~iu';
const CWS_OFFERED_BY_RE = '~^offered by\s*:?\s*(.*)$~iu';
const CWS_RATING_LINE_RE = '~^\(?[0-9][0-9.,]*\s?[km]?\+?\)?(\s*(ratings?|users?|reviews?|stars?|out of 5.*))?$~i';
const CWS_RATED_LINE_RE = '~^(rated\s+)?[0-9.]+\s*(\(.*\)|out of 5.*|stars?.*)$~i';
const CWS_SNIPPET_BLOCK_RES = [
    '~<(?:div|span|p)[^>]*class=["\'][^"\']*(?:result__snippet|b_caption|b_lineclamp|snippet|description)[^"\']*["\'][^>]*>(.*?)</(?:div|span|p)\s*>~is',
    '~<meta[^>]+name=["\']description["\'][^>]+content=["\']([^"\']+)~is',
];
const CWS_NOISE_LINES = [
    'featured', 'extensions', 'extension', 'themes', 'theme', 'overview', 'chrome web store',
    'add to chrome', 'add to chromium', 'remove from chrome', 'established publisher',
    "editors' pick", "editor's pick", "editors’ pick", 'recommended', 'install',
];

// ---------------------------------------------------------------------------
// Searching
// ---------------------------------------------------------------------------

/** Looks the query up on the Web Store, falling back to public search indexes. */
function cws_search(string $query, string $kind): array
{
    $response = new_search_response($query, 'google', $kind);
    $storeQuery = $query;
    $lower = mb_strtolower($query);
    if ($kind === KIND_THEMES && !str_contains($lower, 'theme')) {
        $storeQuery .= ' theme';
    } elseif ($kind === KIND_EXTENSIONS && !str_contains($lower, 'extension')) {
        $storeQuery .= ' extension';
    }

    $storeUrl = 'https://chromewebstore.google.com/search/' . rawurlencode($storeQuery) . '?hl=en-US';
    [$body, $finalUrl, $err] = fetch_html($storeUrl);
    if ($err === null) {
        $response['results'] = cws_extract_store_results($body, $kind, $finalUrl);
        if (count($response['results']) > 0) {
            $response['results'] = cws_enrich_listings($response['results']);
            $response['source'] = 'Chrome Web Store';
            $response['note'] = 'Fetched from the Web Store page. Listings are parsed from public page HTML, so a Store redesign may temporarily affect results.';
            return $response;
        }
    }

    // Google increasingly relies on client-side rendering, so use an index-only fallback
    // rather than attempting to bypass a challenge, login, or anti-bot screen.
    $fallbackQuery = 'site:chromewebstore.google.com/detail ' . $query;
    $fallbackQuery .= $kind === KIND_THEMES ? ' chrome theme' : ' chrome extension';
    $fallbacks = [
        ['public search index (DuckDuckGo)', 'https://html.duckduckgo.com/html/?q=' . urlencode($fallbackQuery)],
        ['public search index (Bing)', 'https://www.bing.com/search?q=' . urlencode($fallbackQuery)],
    ];
    foreach ($fallbacks as [$name, $url]) {
        [$fbBody, , $fbErr] = fetch_html($url);
        if ($fbErr !== null) {
            continue;
        }
        $response['results'] = cws_extract_store_results($fbBody, $kind, $url);
        if (count($response['results']) > 0) {
            $response['results'] = cws_enrich_listings($response['results']);
            $response['source'] = $name;
            $response['note'] = 'The Web Store page did not expose readable listings, so these links came from a public search index. Open a listing to verify its details before downloading.';
            return $response;
        }
    }

    $response['note'] = 'No listings could be parsed. The Web Store may be using a new page layout or temporarily rate-limiting automated requests. Try a shorter query, wait a little, or open the Web Store directly.';
    return $response;
}

// ---------------------------------------------------------------------------
// Result extraction
// ---------------------------------------------------------------------------

/** Parses a Store search page or a search-index results page. */
function cws_extract_store_results(string $s, string $kind, string $pageUrl): array
{
    $results = [];
    $index = [];

    $add = static function (array $candidate) use (&$results, &$index): void {
        $id = $candidate['id'];
        if (isset($index[$id])) {
            $results[$index[$id]] = cws_merge_listings($results[$index[$id]], $candidate);
            return;
        }
        if (count($results) >= MAX_RESULTS) {
            return;
        }
        $index[$id] = count($results);
        $results[] = $candidate;
    };

    // 1. Data blobs embedded in the page (only the Store itself has these).
    foreach (cws_extract_embedded_listings($s, $kind) as $candidate) {
        $add($candidate);
    }
    // 2. Anchor cards / search-engine result links.
    foreach (cws_extract_anchor_listings($s, $kind, $pageUrl) as $candidate) {
        $add($candidate);
    }

    foreach ($results as &$r) {
        if ($r['name'] === '') {
            $r['name'] = CWS_PLACEHOLDER_NAME;
        }
    }
    unset($r);
    return $results;
}

/** Fills the gaps of $a with whatever $b knows. */
function cws_merge_listings(array $a, array $b): array
{
    if ($a['name'] === '' || $a['name'] === CWS_PLACEHOLDER_NAME) {
        $a['name'] = $b['name'];
    }
    if (strlen($b['description']) > strlen($a['description'])) {
        $a['description'] = $b['description'];
    }
    if ($a['publisher'] === '') {
        $a['publisher'] = $b['publisher'];
    }
    if ($a['icon'] === '') {
        $a['icon'] = $b['icon'];
    }
    return $a;
}

function cws_new_listing(string $id, string $kind): array
{
    $l = new_listing($id, $kind);
    $l['url'] = 'https://chromewebstore.google.com/detail/' . $id;
    return $l;
}

function cws_extract_anchor_listings(string $s, string $kind, string $pageUrl): array
{
    $out = [];
    $count = preg_match_all(CWS_ANCHOR_RE, $s, $matches, PREG_SET_ORDER | PREG_OFFSET_CAPTURE);
    if (!$count) {
        return $out;
    }

    foreach ($matches as $m) {
        [$whole, $start] = $m[0];
        $end = $start + strlen($whole);
        $attrs = cws_parse_attributes($m[1][0]);
        $inner = $m[2][0];
        $href = html_entity_decode(trim($attrs['href'] ?? ''), ENT_QUOTES | ENT_HTML5, 'UTF-8');
        if ($href === '') {
            continue;
        }
        $resolved = cws_normalize_store_href($href, $pageUrl);
        $id = cws_extract_id_from_url($resolved);
        if ($id === '') {
            continue;
        }
        $listing = cws_new_listing($id, $kind);
        $parsed = $resolved === '' ? false : parse_url($resolved);
        if ($parsed !== false && str_contains($parsed['path'] ?? '', '/detail/')) {
            $listing['url'] = $resolved;
            if (($parsed['host'] ?? '') === 'chromewebstore.google.com') {
                // Drop tracking parameters; keep the canonical listing URL.
                $listing['url'] = ($parsed['scheme'] ?? 'https') . '://' . $parsed['host'] . ($parsed['path'] ?? '');
            }
        }

        $class = strtolower($attrs['class'] ?? '');
        $lines = cws_visible_lines($inner);

        if (str_contains($class, 'result__snippet')) {
            // DuckDuckGo snippet link: pure description.
            $listing['description'] = truncate_runes(implode(' ', $lines), 360);
        } elseif (str_contains($class, 'result__a') || cws_inside_heading($s, $start)) {
            // Search-engine title link ("Name - Chrome Web Store").
            $listing['name'] = cws_trim_listing_name(implode(' ', $lines));
            if ($listing['description'] === '') {
                $listing['description'] = cws_nearby_snippet($s, $end, 1900);
            }
        } else {
            // Store card.
            $listing['name'] = cws_card_name($inner, $attrs, $lines);
            [$listing['publisher'], $lines] = cws_split_publisher($lines);
            $listing['description'] = cws_choose_description($lines, $listing['name']);
            if ($listing['description'] === '') {
                $listing['description'] = cws_nearby_snippet($s, $end, 1900);
            }
        }

        $listing['description'] = truncate_runes($listing['description'], 360);
        $icon = cws_extract_icon($inner);
        if ($icon !== '' && cws_is_safe_remote_image($icon)) {
            $listing['icon'] = $icon;
        }
        $out[] = $listing;
    }
    return $out;
}

/** True when the anchor at $pos sits directly inside an <h1>-<h6> (Bing's result titles). */
function cws_inside_heading(string $s, int $pos): bool
{
    $start = max(0, $pos - 80);
    $window = strtolower(substr($s, $start, $pos - $start));
    if (!preg_match_all(CWS_HEADING_OPEN_RE, $window, $m, PREG_OFFSET_CAPTURE) || !$m[0]) {
        return false;
    }
    $lastOpen = $m[0][count($m[0]) - 1][1];
    $closed = strrpos($window, '</h');
    return $lastOpen > ($closed === false ? -1 : $closed);
}

function cws_card_name(string $inner, array $attrs, array $lines): string
{
    if (preg_match(CWS_HEADING_RE, $inner, $m)) {
        $hl = cws_visible_lines($m[1]);
        if (count($hl) > 0) {
            $name = cws_trim_listing_name($hl[0]);
            if ($name !== '') {
                return $name;
            }
        }
    }
    $name = first_non_empty($attrs['aria-label'] ?? '', $attrs['title'] ?? '');
    if ($name !== '') {
        return cws_trim_listing_name($name);
    }
    $img = cws_first_img_alt($inner);
    if ($img !== '' && !str_contains(strtolower($img), 'icon')) {
        return cws_trim_listing_name($img);
    }
    return cws_trim_listing_name(cws_choose_name($lines));
}

function cws_first_img_alt(string $inner): string
{
    if (preg_match_all(CWS_IMG_RE, $inner, $matches, PREG_SET_ORDER)) {
        foreach ($matches as $m) {
            $alt = trim(cws_parse_attributes($m[1])['alt'] ?? '');
            if ($alt !== '') {
                return $alt;
            }
        }
    }
    return '';
}

/** Pulls an "Offered by X" / "By X" line out of a card. Returns [publisher, remainingLines]. */
function cws_split_publisher(array $lines): array
{
    $rest = [];
    $publisher = '';
    foreach ($lines as $line) {
        if ($publisher === '' && preg_match(CWS_BYLINE_RE, $line, $m)
            && mb_strlen($m[1]) <= 80 && count(cws_fields($line)) <= 8) {
            $publisher = trim($m[1]);
            continue;
        }
        $rest[] = $line;
    }
    return [$publisher, $rest];
}

/** strings.Fields */
function cws_fields(string $s): array
{
    return preg_split('/\s+/u', trim($s), -1, PREG_SPLIT_NO_EMPTY) ?: [];
}

// ---------------------------------------------------------------------------
// Embedded AF_initDataCallback data
// ---------------------------------------------------------------------------

function cws_extract_embedded_listings(string $s, string $kind): array
{
    $out = [];
    $seen = [];
    foreach (cws_embedded_blobs($s) as $blob) {
        cws_walk_for_listings($blob, 0, static function (array $l) use (&$out, &$seen, $kind): void {
            if (isset($seen[$l['id']])) {
                return;
            }
            $seen[$l['id']] = true;
            $l['kind'] = $kind;
            $out[] = $l;
        });
    }
    return $out;
}

/** The decoded `data:` payload of each AF_initDataCallback({...}) call. */
function cws_embedded_blobs(string $s): array
{
    $marker = 'AF_initDataCallback(';
    $blobs = [];
    $pos = 0;
    $len = strlen($s);
    while ($pos < $len) {
        $i = strpos($s, $marker, $pos);
        if ($i === false) {
            return $blobs;
        }
        $start = $i + strlen($marker);
        $pos = $start;
        $d = strpos($s, 'data:', $start);
        if ($d === false) {
            return $blobs;
        }
        $d -= $start;
        if ($d > 400) { // belongs to some other call
            continue;
        }
        $p = $start + $d + strlen('data:');
        $br = strpos($s, '[', $p);
        if ($br === false || $br - $p > 40) {
            continue;
        }
        $raw = cws_balanced_array(substr($s, $br, CWS_MAX_EMBEDDED_BLOB));
        if ($raw === '') {
            continue;
        }
        $v = json_decode($raw, true);
        if (is_array($v)) {
            $blobs[] = $v;
        }
    }
    return $blobs;
}

/** The JSON array literal at the start of $s ('' when it never closes). */
function cws_balanced_array(string $s): string
{
    $depth = 0;
    $n = strlen($s);
    $i = 0;
    while ($i < $n) {
        // Jump to the next structurally interesting byte.
        $i += strcspn($s, '"[]', $i);
        if ($i >= $n) {
            break;
        }
        $c = $s[$i];
        if ($c === '"') {
            // Skip the string, honouring backslash escapes.
            $i++;
            while ($i < $n) {
                $i += strcspn($s, '"\\', $i);
                if ($i >= $n) {
                    return '';
                }
                if ($s[$i] === '\\') {
                    $i += 2;
                    continue;
                }
                break;
            }
            $i++;
            continue;
        }
        if ($c === '[') {
            $depth++;
        } else {
            $depth--;
            if ($depth === 0) {
                return substr($s, 0, $i + 1);
            }
        }
        $i++;
    }
    return '';
}

function cws_is_list(array $a): bool
{
    return array_is_list($a);
}

function cws_walk_for_listings(mixed $node, int $depth, callable $emit): void
{
    if ($depth > 14 || !is_array($node) || !cws_is_list($node)) {
        return;
    }
    $l = cws_listing_from_array($node);
    if ($l !== null) {
        $emit($l);
        return;
    }
    foreach ($node as $child) {
        cws_walk_for_listings($child, $depth + 1, $emit);
    }
}

/**
 * Recognises the Store's positional listing record:
 * [id, ?, iconURL, name, rating, ratingCount, ..., description, ...].
 * Positions are only a first guess; anything else is found heuristically, and
 * detail-page enrichment corrects what is still wrong.
 */
function cws_listing_from_array(array $arr): ?array
{
    if (count($arr) < 4) {
        return null;
    }
    $id = $arr[0];
    if (!is_string($id) || !preg_match(CWS_BARE_ID_RE, $id)) {
        return null;
    }
    $texts = [];
    $icon = '';
    cws_collect_strings(array_slice($arr, 1), 0, static function (string $v) use (&$texts, &$icon): void {
        $v = trim($v);
        if ($v === '') {
            return;
        }
        if (str_starts_with($v, 'https://') || str_starts_with($v, 'http://') || str_starts_with($v, '//')) {
            if ($icon === '' && cws_is_image_host($v)) {
                $icon = str_starts_with($v, '//') ? substr($v, 2) : $v;
                if (!str_starts_with($icon, 'http')) {
                    $icon = 'https://' . $icon;
                }
            }
            return;
        }
        if (preg_match(CWS_BARE_ID_RE, $v) || str_starts_with($v, '/')) {
            return;
        }
        $texts[] = $v;
    });

    $l = cws_new_listing($id, '');
    $name4 = $arr[3];
    if (is_string($name4) && $name4 !== '' && !str_starts_with($name4, 'http') && mb_strlen($name4) <= 100) {
        $l['name'] = cws_trim_listing_name(html_entity_decode($name4, ENT_QUOTES | ENT_HTML5, 'UTF-8'));
    }
    if ($l['name'] === '') {
        foreach ($texts as $t) {
            if (mb_strlen($t) <= 100 && !str_contains($t, "\n") && !cws_is_noise_line($t)) {
                $l['name'] = cws_trim_listing_name(html_entity_decode($t, ENT_QUOTES | ENT_HTML5, 'UTF-8'));
                break;
            }
        }
    }
    $best = '';
    foreach ($texts as $t) {
        if ($t !== $l['name'] && mb_strlen($t) >= 30 && strlen($t) > strlen($best)) {
            $best = $t;
        }
    }
    $l['description'] = truncate_runes(implode(' ', cws_visible_lines($best)), 360);
    if ($icon !== '' && cws_is_safe_remote_image($icon)) {
        $l['icon'] = $icon;
    }
    if ($l['name'] === '' || ($l['description'] === '' && $l['icon'] === '')) {
        return null;
    }
    return $l;
}

function cws_collect_strings(mixed $v, int $depth, callable $fn): void
{
    if ($depth > 3) {
        return;
    }
    if (is_string($v)) {
        $fn($v);
    } elseif (is_array($v) && cws_is_list($v)) {
        foreach ($v as $c) {
            cws_collect_strings($c, $depth + 1, $fn);
        }
    }
}

function cws_is_image_host(string $raw): bool
{
    $raw = str_starts_with($raw, '//') ? substr($raw, 2) : $raw;
    $u = parse_url($raw);
    if ($u === false || ($u['host'] ?? '') === '') {
        $u = parse_url('https://' . $raw);
        if ($u === false) {
            return false;
        }
    }
    $h = strtolower($u['host'] ?? '');
    foreach (['googleusercontent.com', 'gstatic.com', 'ggpht.com'] as $suffix) {
        if (str_ends_with($h, $suffix)) {
            return true;
        }
    }
    return false;
}

// ---------------------------------------------------------------------------
// Detail-page enrichment (publisher, title, description)
// ---------------------------------------------------------------------------

/**
 * Completes missing fields from each listing's detail page, in parallel and
 * within a fixed time budget. Failures are silent: the listing just keeps
 * whatever the search page gave us.
 */
function cws_enrich_listings(array $listings): array
{
    $queue = [];
    foreach ($listings as $i => $l) {
        $cached = cache_get('detail', $l['id'], CWS_DETAIL_LIFETIME);
        if ($cached !== null) {
            cws_apply_detail($listings[$i], $cached['v']);
        } else {
            $queue[] = $i;
        }
    }
    if (!$queue) {
        return $listings;
    }

    $deadline = microtime(true) + CWS_ENRICH_BUDGET;
    $mh = curl_multi_init();
    $active = []; // (int)handle id => [index, handle, bodyHolder]

    $launch = static function () use (&$queue, &$active, $mh, $listings): void {
        while ($queue && count($active) < CWS_ENRICH_CONCURRENCY) {
            $i = array_shift($queue);
            $holder = new stdClass();
            $holder->body = '';
            $ch = curl_new(
                'https://chromewebstore.google.com/detail/' . $listings[$i]['id'] . '?hl=en-US',
                CWS_DETAIL_TIMEOUT,
                [
                    'Accept: text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8',
                    'Accept-Language: en-US,en;q=0.8',
                ]
            );
            curl_setopt($ch, CURLOPT_ENCODING, '');
            curl_setopt($ch, CURLOPT_WRITEFUNCTION, static function ($c, string $data) use ($holder): int {
                if (strlen($holder->body) < SEARCH_BODY_LIMIT) {
                    $holder->body .= $data;
                }
                return strlen($data);
            });
            curl_multi_add_handle($mh, $ch);
            $active[spl_object_id($ch)] = [$i, $ch, $holder];
        }
    };

    $launch();
    while ($active && microtime(true) < $deadline) {
        $running = 0;
        curl_multi_exec($mh, $running);
        while (($info = curl_multi_info_read($mh)) !== false) {
            $key = spl_object_id($info['handle']);
            if (!isset($active[$key])) {
                continue;
            }
            [$i, $ch, $holder] = $active[$key];
            $code = (int)curl_getinfo($ch, CURLINFO_RESPONSE_CODE);
            if ($info['result'] === CURLE_OK && $code >= 200 && $code < 300) {
                $detail = cws_parse_detail_page(to_utf8($holder->body));
                cache_put('detail', $listings[$i]['id'], $detail, CWS_MAX_CACHED_DETAILS);
                cws_apply_detail($listings[$i], $detail);
            }
            curl_multi_remove_handle($mh, $ch);
            curl_close($ch);
            unset($active[$key]);
        }
        $launch();
        if ($active) {
            curl_multi_select($mh, 0.2);
        }
    }
    foreach ($active as [, $ch]) {
        curl_multi_remove_handle($mh, $ch);
        curl_close($ch);
    }
    curl_multi_close($mh);
    return $listings;
}

/** The listing's own page is authoritative for text fields. */
function cws_apply_detail(array &$l, array $d): void
{
    if (($d['name'] ?? '') !== '') {
        $l['name'] = $d['name'];
    }
    if (($d['description'] ?? '') !== '') {
        $l['description'] = truncate_runes($d['description'], 360);
    }
    if (($d['publisher'] ?? '') !== '') {
        $l['publisher'] = $d['publisher'];
    }
    if ($l['icon'] === '' && ($d['icon'] ?? '') !== '' && cws_is_safe_remote_image($d['icon'])) {
        $l['icon'] = $d['icon'];
    }
}

/**
 * Reads a listing page using several independent sources; earlier sources win,
 * later ones only fill what is still empty.
 */
function cws_parse_detail_page(string $s): array
{
    $d = ['name' => '', 'publisher' => '', 'description' => '', 'icon' => ''];

    // 1. JSON-LD
    if (preg_match_all(CWS_JSONLD_RE, $s, $blocks, PREG_SET_ORDER)) {
        foreach ($blocks as $m) {
            $v = json_decode(trim($m[1]), true);
            if ($v === null) {
                continue;
            }
            cws_merge_json_ld($d, $v, 0);
        }
    }

    // 2. Open Graph / standard meta tags
    $meta = [];
    if (preg_match_all(CWS_META_RE, $s, $tags, PREG_SET_ORDER)) {
        foreach ($tags as $m) {
            $a = cws_parse_attributes($m[1]);
            $key = strtolower(first_non_empty($a['property'] ?? '', $a['name'] ?? '', $a['itemprop'] ?? ''));
            if ($key !== '' && ($a['content'] ?? '') !== '' && !isset($meta[$key])) {
                $meta[$key] = trim($a['content']);
            }
        }
    }
    if ($d['name'] === '') {
        $d['name'] = cws_clean_title(first_non_empty($meta['og:title'] ?? '', $meta['twitter:title'] ?? '', $meta['name'] ?? ''));
    }
    if ($d['description'] === '') {
        $d['description'] = cws_clean_text(first_non_empty($meta['og:description'] ?? '', $meta['description'] ?? '', $meta['twitter:description'] ?? ''));
    }
    if ($d['publisher'] === '') {
        $d['publisher'] = cws_clean_text($meta['author'] ?? '');
    }
    if ($d['icon'] === '') {
        $d['icon'] = first_non_empty($meta['og:image'] ?? '', $meta['image'] ?? '');
    }
    if ($d['name'] === '' && preg_match(CWS_TITLE_RE, $s, $m)) {
        $d['name'] = cws_clean_title($m[1]);
    }

    // 3. "Offered by" text on the rendered page
    if ($d['publisher'] === '') {
        $lines = cws_visible_lines($s);
        foreach ($lines as $i => $line) {
            if (preg_match(CWS_OFFERED_BY_RE, $line, $m)) {
                $candidate = trim($m[1]);
                if ($candidate === '' && isset($lines[$i + 1])) {
                    $candidate = $lines[$i + 1];
                }
                if ($candidate !== '' && mb_strlen($candidate) <= 80) {
                    $d['publisher'] = $candidate;
                    break;
                }
            }
        }
    }

    $d['publisher'] = cws_trim_publisher($d['publisher']);
    if (strcasecmp($d['name'], 'chrome web store') === 0) {
        $d['name'] = '';
    }
    return $d;
}

function cws_merge_json_ld(array &$d, mixed $v, int $depth): void
{
    if ($depth > 6 || !is_array($v)) {
        return;
    }
    if (cws_is_list($v)) {
        foreach ($v as $c) {
            cws_merge_json_ld($d, $c, $depth + 1);
        }
        return;
    }
    if (isset($v['@graph'])) {
        cws_merge_json_ld($d, $v['@graph'], $depth + 1);
    }
    $name = is_string($v['name'] ?? null) ? $v['name'] : '';
    $desc = is_string($v['description'] ?? null) ? $v['description'] : '';
    if ($name === '' && $desc === '') {
        return;
    }
    if ($d['name'] === '') {
        $d['name'] = cws_clean_title($name);
    }
    if ($d['description'] === '') {
        $d['description'] = cws_clean_text($desc);
    }
    if ($d['publisher'] === '') {
        $d['publisher'] = cws_clean_text(first_non_empty(
            cws_json_name($v['author'] ?? null),
            cws_json_name($v['publisher'] ?? null),
            cws_json_name($v['creator'] ?? null),
            cws_json_name($v['provider'] ?? null)
        ));
    }
    if ($d['icon'] === '') {
        $d['icon'] = cws_json_name($v['image'] ?? null);
        $img = $v['image'] ?? null;
        if (is_array($img) && !cws_is_list($img)) {
            $d['icon'] = is_string($img['url'] ?? null) ? $img['url'] : '';
        }
    }
}

/** Reads a schema.org "Thing or Text" value. */
function cws_json_name(mixed $v): string
{
    if (is_string($v)) {
        return $v;
    }
    if (is_array($v)) {
        if (!cws_is_list($v)) {
            return is_string($v['name'] ?? null) ? $v['name'] : '';
        }
        foreach ($v as $c) {
            $n = cws_json_name($c);
            if ($n !== '') {
                return $n;
            }
        }
    }
    return '';
}

function cws_clean_text(string $v): string
{
    return implode(' ', cws_visible_lines($v));
}

function cws_clean_title(string $v): string
{
    return cws_trim_listing_name(preg_replace(CWS_STORE_SUFFIX_RE, '', cws_clean_text($v)) ?? '');
}

function cws_trim_publisher(string $v): string
{
    $v = trim(cws_clean_text($v));
    if (preg_match(CWS_BYLINE_RE, $v, $m)) {
        $v = trim($m[1]);
    }
    return truncate_runes($v, 80);
}

// ---------------------------------------------------------------------------
// URL handling
// ---------------------------------------------------------------------------

function cws_parse_attributes(string $raw): array
{
    $attrs = [];
    if (preg_match_all(CWS_ATTRIBUTE_RE, $raw, $matches, PREG_SET_ORDER)) {
        foreach ($matches as $m) {
            $value = $m[2] ?? '';
            if ($value === '') {
                $value = $m[3] ?? '';
            }
            if ($value === '') {
                $value = $m[4] ?? '';
            }
            $attrs[strtolower($m[1])] = html_entity_decode($value, ENT_QUOTES | ENT_HTML5, 'UTF-8');
        }
    }
    return $attrs;
}

/** Rebuilds a URL from parse_url() parts. */
function cws_unparse(array $p): string
{
    $url = '';
    if (isset($p['scheme'])) {
        $url .= $p['scheme'] . ':';
    }
    if (isset($p['host'])) {
        $url .= '//';
        if (isset($p['user'])) {
            $url .= $p['user'] . (isset($p['pass']) ? ':' . $p['pass'] : '') . '@';
        }
        $url .= $p['host'] . (isset($p['port']) ? ':' . $p['port'] : '');
    }
    $url .= $p['path'] ?? '';
    if (isset($p['query'])) {
        $url .= '?' . $p['query'];
    }
    if (isset($p['fragment'])) {
        $url .= '#' . $p['fragment'];
    }
    return $url;
}

/** Resolves $ref against $base (RFC 3986 enough for scraped links). */
function cws_resolve_url(string $ref, string $base): string
{
    $r = parse_url($ref);
    $b = parse_url($base);
    if ($r === false || $b === false || !isset($b['scheme'], $b['host'])) {
        return '';
    }
    if (isset($r['scheme'])) {
        return $ref;
    }
    $out = ['scheme' => $b['scheme'], 'host' => $b['host']];
    if (isset($b['port'])) {
        $out['port'] = $b['port'];
    }
    if (isset($r['host'])) {
        $out['host'] = $r['host'];
        unset($out['port']);
        if (isset($r['port'])) {
            $out['port'] = $r['port'];
        }
        $out['path'] = $r['path'] ?? '';
    } else {
        $path = $r['path'] ?? '';
        if ($path === '') {
            $out['path'] = $b['path'] ?? '';
            if (!isset($r['query']) && isset($b['query'])) {
                $out['query'] = $b['query'];
            }
        } elseif ($path[0] === '/') {
            $out['path'] = $path;
        } else {
            $dir = substr($b['path'] ?? '/', 0, (int)strrpos($b['path'] ?? '/', '/') + 1);
            $out['path'] = $dir . $path;
        }
    }
    if (isset($r['query'])) {
        $out['query'] = $r['query'];
    }
    if (isset($r['fragment'])) {
        $out['fragment'] = $r['fragment'];
    }
    // Collapse "." and ".." segments.
    $segments = [];
    foreach (explode('/', $out['path']) as $seg) {
        if ($seg === '..') {
            if (count($segments) > 1) {
                array_pop($segments);
            }
        } elseif ($seg !== '.') {
            $segments[] = $seg;
        }
    }
    $out['path'] = implode('/', $segments);
    return cws_unparse($out);
}

/** Unwraps search-engine redirect links and keeps only Web Store URLs ('' otherwise). */
function cws_normalize_store_href(string $href, string $pageUrl): string
{
    $cur = html_entity_decode($href, ENT_QUOTES | ENT_HTML5, 'UTF-8');
    $parsed = parse_url($cur);
    if ($parsed === false) {
        return '';
    }
    $host = strtolower($parsed['host'] ?? '');
    $path = $parsed['path'] ?? '';

    // DuckDuckGo: //duckduckgo.com/l/?uddg=<real url>
    if ($path === '/l/' && ($host === '' || $host === 'duckduckgo.com' || str_ends_with($host, '.duckduckgo.com'))) {
        parse_str($parsed['query'] ?? '', $q);
        if (!empty($q['uddg']) && is_string($q['uddg'])) {
            $cur = $q['uddg'];
            $parsed = parse_url($cur);
            if ($parsed === false) {
                return '';
            }
        }
    }

    // Bing: bing.com/ck/a?u=a1<base64 url>
    $host = strtolower($parsed['host'] ?? '');
    if (str_ends_with($host, 'bing.com') && ($parsed['path'] ?? '') === '/ck/a') {
        parse_str($parsed['query'] ?? '', $q);
        $wrapped = is_string($q['u'] ?? null) ? $q['u'] : '';
        if (str_starts_with($wrapped, 'a1')) {
            $token = strtr(substr($wrapped, 2), '-_', '+/');
            $token .= str_repeat('=', (4 - strlen($token) % 4) % 4);
            $decoded = base64_decode($token, true);
            if ($decoded !== false && str_starts_with($decoded, 'http')) {
                $cur = $decoded;
                $parsed = parse_url($cur);
                if ($parsed === false) {
                    return '';
                }
            }
        }
    }

    if (!isset($parsed['scheme'])) {
        $cur = cws_resolve_url($cur, $pageUrl);
        $parsed = $cur === '' ? false : parse_url($cur);
        if ($parsed === false) {
            return '';
        }
    }
    $host = strtolower($parsed['host'] ?? '');
    $isStore = preg_match(CWS_STORE_HOST_RE, $host)
        || (preg_match(CWS_LEGACY_HOST_RE, $host) && str_contains($parsed['path'] ?? '', '/webstore/'));
    return $isStore ? $cur : '';
}

function cws_extract_id_from_url(string $rawUrl): string
{
    if ($rawUrl === '') {
        return '';
    }
    $parsed = parse_url($rawUrl);
    if ($parsed === false) {
        return '';
    }
    $host = strtolower($parsed['host'] ?? '');
    if (!preg_match(CWS_STORE_HOST_RE, $host) && !preg_match(CWS_LEGACY_HOST_RE, $host)) {
        return '';
    }
    if (!preg_match(CWS_STORE_ID_RE, $parsed['path'] ?? '', $m)) {
        if (!preg_match(CWS_STORE_ID_RE, '?' . ($parsed['query'] ?? ''), $m)) {
            return '';
        }
    }
    $id = strtolower($m[1]);
    return preg_match(CWS_BARE_ID_RE, $id) ? $id : '';
}

// ---------------------------------------------------------------------------
// Text helpers
// ---------------------------------------------------------------------------

function cws_visible_lines(string $fragment): array
{
    $fragment = preg_replace(CWS_SCRIPT_STYLE_RE, ' ', $fragment) ?? $fragment;
    $fragment = preg_replace(CWS_BLOCK_END_RE, "\n", $fragment) ?? $fragment;
    $fragment = preg_replace(CWS_TAG_RE, ' ', $fragment) ?? $fragment;
    $fragment = html_entity_decode($fragment, ENT_QUOTES | ENT_HTML5, 'UTF-8');
    $fragment = str_replace(["\u{200B}", "\u{00A0}"], ['', ' '], $fragment);
    $out = [];
    foreach (explode("\n", $fragment) as $line) {
        $line = trim(preg_replace(CWS_SPACE_RE, ' ', $line) ?? $line);
        if ($line !== '') {
            $out[] = $line;
        }
    }
    return $out;
}

/** UI chrome that is neither a name nor a description: badges, buttons, ratings, user counts. */
function cws_is_noise_line(string $line): bool
{
    $lower = mb_strtolower(trim($line));
    if ($lower === '' || in_array($lower, CWS_NOISE_LINES, true)) {
        return true;
    }
    if (preg_match(CWS_RATING_LINE_RE, $lower) || preg_match(CWS_RATED_LINE_RE, $lower)) {
        return true;
    }
    if (str_contains($lower, 'add to chrome') || str_contains($lower, 'add to chromium')) {
        return true;
    }
    if (count(cws_fields($lower)) < 4 && (str_contains($lower, ' users') || str_contains($lower, 'rating'))) {
        return true;
    }
    return false;
}

function cws_choose_name(array $lines): string
{
    foreach ($lines as $line) {
        if (cws_is_noise_line($line) || mb_strlen($line) > 140) {
            continue;
        }
        return $line;
    }
    return '';
}

function cws_choose_description(array $lines, string $name): string
{
    $parts = [];
    foreach ($lines as $line) {
        if (strcasecmp(trim($line), trim($name)) === 0 || cws_is_noise_line($line)) {
            continue;
        }
        $parts[] = truncate_runes($line, 260);
        if (count($parts) === 2) {
            break;
        }
    }
    return implode(' ', $parts);
}

function cws_trim_listing_name(string $s): string
{
    $s = preg_replace(CWS_STORE_SUFFIX_RE, '', trim($s)) ?? $s;
    $s = trim(preg_replace('~^[|·•\-–—]+|[|·•\-–—]+$~u', '', trim($s)) ?? $s);
    foreach (["\n", ' | ', ' · ', ' — ', ' – '] as $sep) {
        $i = strpos($s, $sep);
        if ($i !== false && $i > 0 && $i < 140) {
            $s = substr($s, 0, $i);
        }
    }
    return truncate_runes($s, 100);
}

function cws_nearby_snippet(string $body, int $pos, int $radius): string
{
    $pos = max(0, $pos);
    $context = substr($body, $pos, $radius);
    foreach (CWS_SNIPPET_BLOCK_RES as $re) {
        if (preg_match($re, $context, $m)) {
            $lines = cws_visible_lines($m[1]);
            if (count($lines) > 0) {
                return truncate_runes(implode(' ', $lines), 360);
            }
        }
    }
    return '';
}

function cws_extract_icon(string $inner): string
{
    if (preg_match_all(CWS_IMG_RE, $inner, $matches, PREG_SET_ORDER)) {
        foreach ($matches as $m) {
            $attrs = cws_parse_attributes($m[1]);
            foreach (['src', 'data-src', 'data-lazy-src'] as $key) {
                $value = trim($attrs[$key] ?? '');
                if ($value === '') {
                    continue;
                }
                if (str_starts_with($value, '//')) {
                    return 'https:' . $value;
                }
                if (str_starts_with($value, 'https://')) {
                    return $value;
                }
            }
        }
    }
    return '';
}

function cws_is_safe_remote_image(string $s): bool
{
    $u = parse_url($s);
    return $u !== false && ($u['scheme'] ?? '') === 'https' && ($u['host'] ?? '') !== '';
}

// ---------------------------------------------------------------------------
// CRX download
// ---------------------------------------------------------------------------

/** Streams the original CRX package for ?id= from Google's update endpoint. */
function cws_download(): void
{
    $id = strtolower(trim((string)($_GET['id'] ?? '')));
    if (!preg_match(CWS_BARE_ID_RE, $id)) {
        plain_error(400, 'Invalid Chrome Web Store extension ID.');
        return;
    }
    $kind = strtolower(trim((string)($_GET['type'] ?? ''))) === 'themes' ? 'themes' : 'extensions';
    $name = trim((string)($_GET['name'] ?? ''));
    if ($name === '') {
        $name = $id;
    }
    $filename = safe_filename($name) . '.crx';

    $prod = trim((string)getenv('CWS_PRODVERSION'));
    if ($prod === '') {
        if (preg_match('~(?:Chrome|Chromium)/([0-9]+\.[0-9]+\.[0-9]+\.[0-9]+)~i', $_SERVER['HTTP_USER_AGENT'] ?? '', $m)) {
            $prod = $m[1];
        } else {
            $prod = CWS_DEFAULT_PRODVERSION;
        }
    }
    if (!preg_match('~^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+\z~', $prod)) {
        $prod = CWS_DEFAULT_PRODVERSION;
    }

    $endpoint = 'https://clients2.google.com/service/update2/crx?' . http_build_query([
        'acceptformat' => 'crx3,crx2',
        'prodversion' => $prod,
        'response' => 'redirect',
        'x' => 'id=' . $id . '&installsource=ondemand&uc',
    ]);

    set_time_limit(60);
    $r = stream_package($endpoint, [
        'magic' => 'Cr24',
        'limit' => 200 << 20,
        'timeout' => 30.0,
        'accept' => 'application/x-chrome-extension,application/octet-stream,*/*',
        'start' => static function () use ($filename, $kind): void {
            header('Content-Type: application/x-chrome-extension');
            header('Content-Disposition: inline; filename="' . $filename . '"');
            header('Cache-Control: no-store');
            header('X-Content-Type-Options: nosniff');
            header('X-CWS-Kind: ' . $kind);
        },
    ]);

    switch ($r['status']) {
        case 'ok':
            return;
        case 'net':
        case 'redirect':
            plain_error(502, "Download failed while contacting Google's package endpoint. Check your connection, proxy, or try again later.");
            return;
        case 'http':
            plain_error(502, "Google's package endpoint returned HTTP " . $r['code'] . '. The listing may be removed, restricted, or temporarily unavailable.');
            return;
        default:
            $message = 'Google did not return a valid CRX package. The extension may be unavailable for this browser version.';
            if ($r['preview'] !== '' && str_contains(strtolower($r['preview']), 'not found')) {
                $message = 'Google reports that this package was not found or is no longer available.';
            }
            plain_error(502, $message);
    }
}
