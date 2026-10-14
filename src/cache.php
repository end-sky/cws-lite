<?php
// Tiny file-backed cache (PHP has no shared memory between requests by
// default). Entries live in the system temp directory.

declare(strict_types=1);

function cache_dir(): string
{
    $dir = sys_get_temp_dir() . '/troller-cache';
    if (!is_dir($dir)) {
        @mkdir($dir, 0700, true);
    }
    return $dir;
}

function cache_file(string $ns, string $key): string
{
    return cache_dir() . '/' . $ns . '-' . sha1($key) . '.json';
}

/** Returns ['v' => value] on a fresh hit, null otherwise. */
function cache_get(string $ns, string $key, int $ttl): ?array
{
    $file = cache_file($ns, $key);
    $mtime = @filemtime($file);
    if ($mtime === false) {
        return null;
    }
    if (time() - $mtime > $ttl) {
        @unlink($file);
        return null;
    }
    $raw = @file_get_contents($file);
    $data = $raw === false ? null : json_decode($raw, true);
    return is_array($data) && array_key_exists('v', $data) ? $data : null;
}

function cache_put(string $ns, string $key, mixed $value, int $max): void
{
    $dir = cache_dir();
    $files = glob($dir . '/' . $ns . '-*.json') ?: [];
    if (count($files) >= $max) {
        usort($files, static fn($a, $b) => (@filemtime($a) ?: 0) <=> (@filemtime($b) ?: 0));
        foreach (array_slice($files, 0, count($files) - ($max - 1)) as $old) {
            @unlink($old);
        }
    }
    $file = cache_file($ns, $key);
    $tmp = $file . '.' . bin2hex(random_bytes(4)) . '.tmp';
    $json = json_encode(['v' => $value], JSON_INVALID_UTF8_SUBSTITUTE);
    if ($json !== false && @file_put_contents($tmp, $json) !== false) {
        @rename($tmp, $file);
    } else {
        @unlink($tmp);
    }
}
