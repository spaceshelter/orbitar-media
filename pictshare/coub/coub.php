<?php
define('DS', DIRECTORY_SEPARATOR);
define('ROOT', dirname(__FILE__).'/..');

define('COUB_CACHE_DIR', ROOT . DS . 'data' . DS . 'coub_cache');
define('COUB_TIMEOUT_TTL', 20 * 60);       // 20 minutes
define('COUB_NOT_FOUND_TTL', 30 * 24 * 3600); // 1 month

function getCoubCachePath($hash) {
    return COUB_CACHE_DIR . DS . $hash . '.txt';
}

function getCachedCoub($hash) {
    $path = getCoubCachePath($hash);
    if (!file_exists($path)) {
        return false;
    }

    $content = trim(file_get_contents($path));
    $age = time() - filemtime($path);

    // Check if negative cache has expired
    if ($content === 'TIMEOUT' && $age > COUB_TIMEOUT_TTL) {
        unlink($path);
        return false;
    }
    if ($content === '404' && $age > COUB_NOT_FOUND_TTL) {
        unlink($path);
        return false;
    }

    return $content;
}

function cacheCoubResult($hash, $result) {
    if (!is_dir(COUB_CACHE_DIR)) {
        mkdir(COUB_CACHE_DIR, 0777, true);
    }
    file_put_contents(getCoubCachePath($hash), $result);
}

function fetchCoubThumbnailUrl($hash) {
    $proxyUrl = getenv('COUB_PROXY_URL');
    if ($proxyUrl) {
        $url = rtrim($proxyUrl, '/') . '/' . $hash;
    } else {
        $url = "https://coub.com/api/v2/coubs/" . $hash;
    }

    $ctx = stream_context_create([
        'http' => [
            'timeout' => 10,
            'ignore_errors' => true
        ]
    ]);

    $content = @file_get_contents($url, false, $ctx);

    // Check for timeout/unreachable
    if ($content === false) {
        return ['type' => 'timeout'];
    }

    // Check HTTP status from response headers
    $status = 200;
    if (isset($http_response_header[0])) {
        preg_match('/\d{3}/', $http_response_header[0], $matches);
        $status = (int)($matches[0] ?? 200);
    }

    if ($status === 404) {
        return ['type' => '404'];
    }

    if ($status !== 200) {
        return ['type' => 'timeout']; // Treat other errors as temporary
    }

    $data = json_decode($content, true);
    $picture = $data['picture'] ?? null;

    if ($picture) {
        return ['type' => 'success', 'url' => $picture];
    }

    return ['type' => '404']; // No picture field = treat as not found
}

function handleCoubRequest($hash) {
    // Check cache first
    $cached = getCachedCoub($hash);
    if ($cached !== false) {
        if ($cached === 'TIMEOUT') {
            header("X-Coub-Cache: HIT-TIMEOUT");
            header('HTTP/1.1 504 Gateway Timeout');
            header('Content-Type: application/json');
            exit(json_encode(['status' => 'err', 'reason' => 'Coub temporarily unavailable']));
        }
        if ($cached === '404') {
            header("X-Coub-Cache: HIT-404");
            header('HTTP/1.1 404 Not Found');
            header('Content-Type: application/json');
            exit(json_encode(['status' => 'err', 'reason' => 'Coub not found']));
        }
        // Success - redirect to cached URL
        header("X-Coub-Cache: HIT");
        header("Location: " . $cached);
        exit();
    }

    // Fetch from Coub API
    $result = fetchCoubThumbnailUrl($hash);

    switch ($result['type']) {
        case 'success':
            cacheCoubResult($hash, $result['url']);
            header("X-Coub-Cache: MISS");
            header("Location: " . $result['url']);
            exit();

        case '404':
            cacheCoubResult($hash, '404');
            header("X-Coub-Cache: MISS-404");
            header('HTTP/1.1 404 Not Found');
            header('Content-Type: application/json');
            exit(json_encode(['status' => 'err', 'reason' => 'Coub not found']));

        case 'timeout':
            cacheCoubResult($hash, 'TIMEOUT');
            header("X-Coub-Cache: MISS-TIMEOUT");
            header('HTTP/1.1 504 Gateway Timeout');
            header('Content-Type: application/json');
            exit(json_encode(['status' => 'err', 'reason' => 'Coub temporarily unavailable']));
    }
}

if (isset($_REQUEST['hash']) && preg_match('/^(\w{3,})$/', $_REQUEST['hash'], $matches)) {
    handleCoubRequest($matches[1]);
} else {
    header('Content-Type: application/json');
    exit(json_encode(['status' => 'err', 'reason' => 'Invalid URL']));
}
