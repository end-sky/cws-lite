<?php
// Firefox Add-ons (addons.mozilla.org) through Mozilla's public v5 JSON API.
// No HTML scraping: titles, publishers, descriptions and icons come straight
// from the API response.

declare(strict_types=1);

const AMO_ID_RE = '/^[A-Za-z0-9_.-]{1,100}\z/';

function amo_api_base(): string
{
    return getenv('TROLLER_AMO_BASE') ?: 'https://addons.mozilla.org';
}

/** Accepts both "text" and {"en-US": "text", "de": "..."}. */
function amo_localized(mixed $v): string
{
    if (is_string($v)) {
        return $v;
    }
    if (!is_array($v)) {
        return '';
    }
    foreach (['en-US', 'en-GB', 'en'] as $key) {
        if (isset($v[$key]) && is_string($v[$key]) && $v[$key] !== '') {
            return $v[$key];
        }
    }
    foreach ($v as $item) {
        if (is_string($item) && $item !== '') {
            return $item;
        }
    }
    return '';
}

/** Strips markup and entities from API strings (summaries may contain HTML). */
function amo_clean_text(string $s): string
{
    $s = preg_replace('/<[^>]*>/s', ' ', $s) ?? $s;
    $s = html_entity_decode($s, ENT_QUOTES | ENT_HTML5, 'UTF-8');
    return trim(preg_replace('/\s+/u', ' ', $s) ?? $s);
}

/** Search addons.mozilla.org. $kind is "extensions" or "themes". */
function amo_search(string $query, string $kind): array
{
    $response = new_search_response($query, 'firefox', $kind);

    $params = http_build_query([
        'q' => $query,
        'type' => $kind === KIND_THEMES ? 'statictheme' : 'extension',
        'app' => 'firefox',
        'lang' => 'en-US',
        'page_size' => MAX_RESULTS,
    ]);
    [$payload, $err] = fetch_json(amo_api_base() . '/api/v5/addons/search/?' . $params);
    if ($err !== null) {
        $response['note'] = 'Firefox Add-ons could not be reached (' . $err . '). Try again in a moment or open addons.mozilla.org directly.';
        return $response;
    }
    $response['results'] = amo_listings_from($payload['results'] ?? [], $kind);
    $response['source'] = 'Firefox Add-ons';
    if (count($response['results']) === 0) {
        $response['note'] = 'Firefox Add-ons returned no matches. Try a different or shorter search term.';
    }
    return $response;
}

function amo_listings_from(mixed $addons, string $kind): array
{
    $out = [];
    $seen = [];
    if (!is_array($addons)) {
        return $out;
    }
    foreach ($addons as $a) {
        if (!is_array($a)) {
            continue;
        }
        $id = is_string($a['slug'] ?? null) ? $a['slug'] : '';
        if ($id === '' && !empty($a['id']) && is_scalar($a['id'])) {
            $id = (string)$a['id'];
        }
        if (!preg_match(AMO_ID_RE, $id) || isset($seen[$id])) {
            continue;
        }
        $seen[$id] = true;

        $name = amo_clean_text(amo_localized($a['name'] ?? ''));
        if ($name === '') {
            $name = $id;
        }
        $publisher = '';
        $author = $a['authors'][0] ?? null;
        if (is_array($author)) {
            $publisher = first_non_empty((string)($author['name'] ?? ''), (string)($author['username'] ?? ''));
        }

        $listingUrl = is_string($a['url'] ?? null) ? $a['url'] : '';
        $u = parse_url($listingUrl);
        if ($u === false || ($u['scheme'] ?? '') !== 'https' || ($u['host'] ?? '') !== 'addons.mozilla.org') {
            $listingUrl = 'https://addons.mozilla.org/en-US/firefox/addon/' . rawurlencode($id) . '/';
        }
        $icon = is_string($a['icon_url'] ?? null) ? $a['icon_url'] : '';
        $iu = parse_url($icon);
        if ($iu === false || ($iu['scheme'] ?? '') !== 'https' || ($iu['host'] ?? '') === '') {
            $icon = '';
        }

        $l = new_listing($id, $kind);
        $l['name'] = truncate_runes($name, 100);
        $l['publisher'] = truncate_runes(amo_clean_text($publisher), 80);
        $l['description'] = truncate_runes(amo_clean_text(amo_localized($a['summary'] ?? '')), 360);
        $l['icon'] = $icon;
        $l['url'] = $listingUrl;
        $out[] = $l;
    }
    return $out;
}

// ---------------------------------------------------------------------------
// Download
// ---------------------------------------------------------------------------

/** Where an .xpi may be fetched from. */
function amo_download_url_allowed(string $url): bool
{
    $u = parse_url($url);
    if ($u === false) {
        return false;
    }
    $host = strtolower($u['host'] ?? '');
    $override = getenv('TROLLER_AMO_BASE');
    if ($override) {
        $ou = parse_url($override);
        if ($ou && strtolower($ou['host'] ?? '') === $host) {
            return true;
        }
    }
    return ($u['scheme'] ?? '') === 'https' && ($host === 'addons.mozilla.org' || $host === 'addons.cdn.mozilla.net');
}

/**
 * Streams the current signed .xpi of the add-on named by ?id= (slug or numeric
 * id) as application/x-xpinstall so Firefox offers to install it.
 */
function amo_download(): void
{
    $id = trim((string)($_GET['id'] ?? ''));
    if (!preg_match(AMO_ID_RE, $id)) {
        plain_error(400, 'Invalid Firefox add-on ID.');
        return;
    }
    set_time_limit(90);

    [$detail, $err] = fetch_json(amo_api_base() . '/api/v5/addons/addon/' . rawurlencode($id) . '/?lang=en-US', 60.0);
    if ($err !== null) {
        plain_error(502, 'Could not look up that add-on on addons.mozilla.org (' . $err . ').');
        return;
    }
    $fileUrl = $detail['current_version']['file']['url'] ?? '';
    if (!is_string($fileUrl) || $fileUrl === '' || !amo_download_url_allowed($fileUrl)) {
        plain_error(502, 'This add-on has no downloadable package.');
        return;
    }

    $name = safe_filename(first_non_empty((string)($_GET['name'] ?? ''), $id));
    $r = stream_package($fileUrl, [
        'magic' => 'PK',
        'limit' => 200 << 20,
        'timeout' => 60.0,
        'accept' => '*/*',
        'allowed' => 'amo_download_url_allowed',
        'start' => static function () use ($name): void {
            header('Content-Type: application/x-xpinstall');
            header('Content-Disposition: inline; filename="' . $name . '.xpi"');
            header('Cache-Control: no-store');
            header('X-Content-Type-Options: nosniff');
        },
    ]);

    switch ($r['status']) {
        case 'ok':
            return;
        case 'net':
        case 'redirect':
            plain_error(502, 'Download failed while contacting Mozilla. Check your connection or try again later.');
            return;
        case 'http':
            plain_error(502, 'Mozilla returned HTTP ' . $r['code'] . ' for this package.');
            return;
        default:
            plain_error(502, 'Mozilla did not return a valid .xpi package.');
    }
}
