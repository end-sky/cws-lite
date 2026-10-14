<?php
// Outbound HTTP helpers (curl): page fetches, JSON fetches, and a streaming
// package proxy with magic-byte validation and an optional redirect allowlist.

declare(strict_types=1);

/** Configured cURL handle with the shared defaults. */
function curl_new(string $url, float $timeout, array $headers = [], bool $follow = true): CurlHandle
{
    $ch = curl_init($url);
    curl_setopt_array($ch, [
        CURLOPT_USERAGENT => USER_AGENT,
        CURLOPT_HTTPHEADER => $headers,
        CURLOPT_FOLLOWLOCATION => $follow,
        CURLOPT_MAXREDIRS => $follow ? 8 : 0,
        CURLOPT_TIMEOUT_MS => (int)($timeout * 1000),
        CURLOPT_CONNECTTIMEOUT => 6,
    ]);
    return $ch;
}

/**
 * GET a URL into memory, reading at most $limit bytes.
 * Returns ['body' => string, 'url' => finalUrl, 'code' => int, 'error' => ?string].
 */
function http_get(string $url, float $timeout, array $headers = [], int $limit = SEARCH_BODY_LIMIT): array
{
    $buf = '';
    $full = false;
    $ch = curl_new($url, $timeout, $headers);
    curl_setopt($ch, CURLOPT_ENCODING, '');
    curl_setopt($ch, CURLOPT_WRITEFUNCTION, static function ($c, string $data) use (&$buf, &$full, $limit): int {
        $room = $limit - strlen($buf);
        if ($room <= 0) {
            $full = true;
            return 0;
        }
        $buf .= substr($data, 0, $room);
        if (strlen($data) > $room) {
            $full = true;
            return 0;
        }
        return strlen($data);
    });
    $ok = curl_exec($ch);
    $result = [
        'body' => $buf,
        'url' => (string)curl_getinfo($ch, CURLINFO_EFFECTIVE_URL),
        'code' => (int)curl_getinfo($ch, CURLINFO_RESPONSE_CODE),
        'error' => ($ok === false && !$full) ? curl_error($ch) : null,
    ];
    curl_close($ch);
    return $result;
}

/** Fetches an HTML page. Returns [body, finalUrl, error|null]. */
function fetch_html(string $url, float $timeout = SEARCH_TIMEOUT): array
{
    $r = http_get($url, $timeout, [
        'Accept: text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8',
        'Accept-Language: en-US,en;q=0.8',
    ]);
    if ($r['error'] !== null) {
        return ['', $r['url'], $r['error']];
    }
    if ($r['code'] < 200 || $r['code'] >= 300) {
        return ['', $r['url'], 'upstream returned HTTP ' . $r['code']];
    }
    return [to_utf8($r['body']), $r['url'], null];
}

/** Fetches JSON. Returns [decoded|null, error|null]. */
function fetch_json(string $url, float $timeout = SEARCH_TIMEOUT): array
{
    $r = http_get($url, $timeout, ['Accept: application/json']);
    if ($r['error'] !== null) {
        return [null, $r['error']];
    }
    if ($r['code'] !== 200) {
        return [null, 'HTTP ' . $r['code']];
    }
    $data = json_decode($r['body'], true);
    if (!is_array($data)) {
        return [null, 'invalid JSON from upstream'];
    }
    return [$data, null];
}

/**
 * Streams a package to the client once it has proven to be what we expect.
 *
 * Options:
 *   magic    string   bytes the body must start with
 *   limit    int      max bytes to forward
 *   timeout  float    seconds
 *   accept   string   Accept header
 *   allowed  ?callable(string $url): bool  checked on every redirect hop
 *   start    callable called right before the first byte is sent (emit headers)
 *
 * Returns ['status' => 'ok'|'net'|'http'|'magic'|'redirect', 'code' => int, 'preview' => string].
 * Nothing has been sent to the client unless status is 'ok'.
 */
function stream_package(string $url, array $o): array
{
    $magic = $o['magic'];
    $limit = $o['limit'];
    $allowed = $o['allowed'] ?? null;

    for ($hop = 0; $hop <= 5; $hop++) {
        $st = ['buf' => '', 'started' => false, 'bad' => false, 'sent' => 0, 'code' => 0, 'preview' => ''];
        $ch = curl_new($url, $o['timeout'], ['Accept: ' . $o['accept']], false);
        curl_setopt($ch, CURLOPT_WRITEFUNCTION, static function ($c, string $data) use (&$st, $magic, $limit, $o): int {
            $len = strlen($data);
            $code = (int)curl_getinfo($c, CURLINFO_RESPONSE_CODE);
            $st['code'] = $code;
            if ($code !== 200) {
                return $len; // redirects and errors: discard the body
            }
            if (!$st['started']) {
                $st['buf'] .= $data;
                if (strlen($st['buf']) < strlen($magic)) {
                    return $len; // wait for more bytes
                }
                if (!str_starts_with($st['buf'], $magic)) {
                    $st['bad'] = true;
                    $st['preview'] = substr($st['buf'], 0, 512);
                    return 0;
                }
                ($o['start'])();
                $st['started'] = true;
                $data = $st['buf'];
                $st['buf'] = '';
            }
            $room = $limit - $st['sent'];
            if ($room <= 0) {
                return 0;
            }
            $chunk = substr($data, 0, $room);
            echo $chunk;
            flush();
            $st['sent'] += strlen($chunk);
            return strlen($chunk) < strlen($data) ? 0 : $len;
        });
        $ok = curl_exec($ch);
        $code = (int)curl_getinfo($ch, CURLINFO_RESPONSE_CODE);
        $redirect = (string)curl_getinfo($ch, CURLINFO_REDIRECT_URL);
        $err = curl_error($ch);
        curl_close($ch);

        if ($code >= 300 && $code < 400 && $redirect !== '') {
            if ($allowed !== null && !$allowed($redirect)) {
                return ['status' => 'redirect', 'code' => $code, 'preview' => ''];
            }
            $url = $redirect;
            continue;
        }
        if ($st['started']) {
            return ['status' => 'ok', 'code' => 200, 'preview' => ''];
        }
        if ($st['bad']) {
            return ['status' => 'magic', 'code' => 200, 'preview' => $st['preview']];
        }
        if ($code === 200) {
            // Body shorter than the signature, or an empty body.
            return ['status' => 'magic', 'code' => 200, 'preview' => $st['buf']];
        }
        if ($ok === false && $code === 0) {
            return ['status' => 'net', 'code' => 0, 'preview' => $err];
        }
        return ['status' => 'http', 'code' => $code, 'preview' => ''];
    }
    return ['status' => 'redirect', 'code' => 0, 'preview' => ''];
}
