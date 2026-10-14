<?php
// Shared constants, helpers and the search/cache glue used by the front
// controller and both store back ends (google.php, firefox.php).

declare(strict_types=1);

const TROLLER_VERSION = '1.1.1';

const SEARCH_TIMEOUT = 9.0;           // seconds per upstream request
const SEARCH_BODY_LIMIT = 6 << 20;    // bytes read from any upstream page
const MAX_RESULTS = 24;
const MAX_QUERY_CHARS = 120;
const SEARCH_CACHE_TTL = 240;         // 4 minutes
const SEARCH_CACHE_MAX = 128;

const USER_AGENT = 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36';

const KIND_EXTENSIONS = 'extensions';
const KIND_THEMES = 'themes';
const DEFAULT_STORE = 'google';

// The scrapers run big regexes over multi-megabyte pages.
ini_set('pcre.jit', '0');
ini_set('pcre.backtrack_limit', '200000000');
ini_set('pcre.recursion_limit', '200000');

/** A search result, identical for every store. */
function new_listing(string $id, string $kind): array
{
    return [
        'id' => $id,
        'name' => '',
        'publisher' => '',
        'description' => '',
        'icon' => '',
        'url' => '',
        'kind' => $kind,
    ];
}

/** Empty string fields are omitted from JSON, like Go's omitempty. */
function listing_for_json(array $l): array
{
    foreach (['publisher', 'description', 'icon'] as $k) {
        if (($l[$k] ?? '') === '') {
            unset($l[$k]);
        }
    }
    return $l;
}

function new_search_response(string $query, string $store, string $kind): array
{
    return ['query' => $query, 'store' => $store, 'kind' => $kind, 'source' => '', 'note' => '', 'results' => []];
}

function response_for_json(array $r): array
{
    foreach (['source', 'note'] as $k) {
        if (($r[$k] ?? '') === '') {
            unset($r[$k]);
        }
    }
    $r['results'] = array_map('listing_for_json', $r['results']);
    return $r;
}

/** Turns an arbitrary title into a safe download file stem. */
function safe_filename(string $value): string
{
    $value = trim($value);
    $value = str_replace(' ', '-', $value);
    $value = preg_replace('/[^a-zA-Z0-9._-]+/', '-', $value) ?? '';
    $value = trim($value, '.-_ ');
    if ($value === '') {
        return 'store-item';
    }
    return mb_substr($value, 0, 72);
}

/** Trims and shortens a string to at most $max characters, adding an ellipsis. */
function truncate_runes(string $value, int $max): string
{
    $value = trim($value);
    if (mb_strlen($value) <= $max) {
        return $value;
    }
    return mb_substr($value, 0, $max - 1) . '…';
}

/** First value that is not blank after trimming. */
function first_non_empty(string ...$values): string
{
    foreach ($values as $value) {
        $value = trim($value);
        if ($value !== '') {
            return $value;
        }
    }
    return '';
}

/** Makes upstream bytes safe to hand to the (UTF-8 mode) regexes. */
function to_utf8(string $s): string
{
    return mb_scrub($s, 'UTF-8');
}

// ---------------------------------------------------------------------------
// Responses
// ---------------------------------------------------------------------------

function send_security_headers(): void
{
    header('X-Content-Type-Options: nosniff');
    header('Referrer-Policy: no-referrer');
    header('X-Frame-Options: DENY');
    header('Permissions-Policy: camera=(), microphone=(), geolocation=()');
}

function write_json(int $status, array $payload): void
{
    http_response_code($status);
    header('Content-Type: application/json; charset=utf-8');
    header('Cache-Control: no-store');
    echo json_encode(
        $payload,
        JSON_HEX_TAG | JSON_HEX_AMP | JSON_UNESCAPED_SLASHES | JSON_INVALID_UTF8_SUBSTITUTE
    ), "\n";
}

function plain_error(int $status, string $message): void
{
    http_response_code($status);
    header('Content-Type: text/plain; charset=utf-8');
    echo $message, "\n";
}

// ---------------------------------------------------------------------------
// Search dispatch (shared by /api/search and the HTML page)
// ---------------------------------------------------------------------------

/**
 * Validates and runs a search. Returns [httpStatus, payload]; on failure the
 * payload is ['error' => message].
 */
function run_search(string $q, string $kind, string $store): array
{
    $q = trim($q);
    $kind = strtolower(trim($kind));
    if ($kind === '') {
        $kind = KIND_EXTENSIONS;
    }
    $store = strtolower(trim($store));
    if ($store === '') {
        $store = DEFAULT_STORE;
    }

    if ($store !== 'google' && $store !== 'firefox') {
        return [400, ['error' => 'store must be google or firefox']];
    }
    if ($kind !== KIND_EXTENSIONS && $kind !== KIND_THEMES) {
        return [400, ['error' => 'type must be extensions or themes']];
    }
    if (mb_strlen($q) > MAX_QUERY_CHARS) {
        return [400, ['error' => 'Search is limited to 120 characters.']];
    }
    if ($q === '') {
        return [400, ['error' => 'Enter a search term.']];
    }

    $key = $store . "\0" . $kind . "\0" . mb_strtolower($q);
    $cached = cache_get('search', $key, SEARCH_CACHE_TTL);
    if ($cached !== null) {
        return [200, $cached['v']];
    }

    set_time_limit(60);
    $response = $store === 'firefox' ? amo_search($q, $kind) : cws_search($q, $kind);
    if (count($response['results']) > 0) { // never pin a failed/empty lookup in the cache
        cache_put('search', $key, $response, SEARCH_CACHE_MAX);
    }
    return [200, $response];
}
