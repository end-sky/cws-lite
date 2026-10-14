<?php
// Mock addons.mozilla.org for end-to-end tests.
//   php -S 127.0.0.1:8099 tests/mock_amo.php
$path = parse_url($_SERVER['REQUEST_URI'], PHP_URL_PATH);
$base = 'http://' . $_SERVER['HTTP_HOST'];

if ($path === '/api/v5/addons/search/') {
    header('Content-Type: application/json');
    $q = $_GET['q'] ?? '';
    $results = $q === 'none' ? [] : [[
        'id' => 1, 'slug' => 'mock-addon',
        'name' => ['en-US' => 'Mock <i>Add-on</i>'], 'summary' => ['en-US' => 'A mock &amp; test add-on.'],
        'icon_url' => 'https://addons.cdn.mozilla.net/icon.png',
        'url' => 'https://addons.mozilla.org/en-US/firefox/addon/mock-addon/',
        'authors' => [['name' => 'Tester', 'username' => 't']],
        'type' => $_GET['type'] ?? '',
    ]];
    echo json_encode(['results' => $results]);
} elseif ($path === '/api/v5/addons/addon/mock-addon/') {
    header('Content-Type: application/json');
    echo json_encode(['current_version' => ['file' => ['url' => $base . '/files/mock.xpi']]]);
} elseif ($path === '/api/v5/addons/addon/redirect/') {
    header('Content-Type: application/json');
    echo json_encode(['current_version' => ['file' => ['url' => $base . '/redir']]]);
} elseif ($path === '/api/v5/addons/addon/evil/') {
    header('Content-Type: application/json');
    echo json_encode(['current_version' => ['file' => ['url' => 'https://evil.example/x.xpi']]]);
} elseif ($path === '/api/v5/addons/addon/html/') {
    header('Content-Type: application/json');
    echo json_encode(['current_version' => ['file' => ['url' => $base . '/files/not-xpi']]]);
} elseif ($path === '/redir') {
    header('Location: ' . $base . '/files/mock.xpi', true, 302);
} elseif ($path === '/files/mock.xpi') {
    header('Content-Type: application/octet-stream');
    echo "PK\x03\x04" . str_repeat('A', 300000); // spans several write chunks
} elseif ($path === '/files/not-xpi') {
    echo '<html>captcha</html>';
} else {
    http_response_code(404);
    echo 'nope';
}
