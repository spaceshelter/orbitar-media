<?php
define('DS', DIRECTORY_SEPARATOR);
define('ROOT', dirname(__FILE__).'/..');

define('COUB_CACHE_DIR', ROOT . DS . 'data' . DS . 'coub_cache');

function getCoubCachePath($hash) {
    return COUB_CACHE_DIR . DS . $hash . '.txt';
}

function getCachedCoubUrl($hash) {
    $path = getCoubCachePath($hash);
    if (file_exists($path)) {
        return trim(file_get_contents($path));
    }
    return false;
}

function cacheCoubUrl($hash, $url) {
    if (!is_dir(COUB_CACHE_DIR)) {
        mkdir(COUB_CACHE_DIR, 0777, true);
    }
    file_put_contents(getCoubCachePath($hash), $url);
}

function fetchCoubThumbnailUrl($hash) {
    $url = "https://coub.com/api/v2/coubs/" . $hash;
    $content = @file_get_contents($url);
    if ($content === false) {
        return false;
    }
    $data = json_decode($content, true);
    return $data['picture'] ?? false;
}

function handleCoubRequest($hash) {
    // Check cache first
    $cachedUrl = getCachedCoubUrl($hash);
    if ($cachedUrl) {
        header("X-Coub-Cache: HIT");
        header("Location: " . $cachedUrl);
        exit();
    }

    // Fetch from Coub API
    $thumbnailUrl = fetchCoubThumbnailUrl($hash);
    if ($thumbnailUrl) {
        cacheCoubUrl($hash, $thumbnailUrl);
        header("X-Coub-Cache: MISS");
        header("Location: " . $thumbnailUrl);
        exit();
    }

    // Error
    header('HTTP/1.1 404 Not Found');
    header('Content-Type: application/json');
    exit(json_encode(['status' => 'err', 'reason' => 'Coub not found']));
}

if (isset($_REQUEST['hash']) && preg_match('/^(\w{3,})$/', $_REQUEST['hash'], $matches)) {
    handleCoubRequest($matches[1]);
} else {
    header('Content-Type: application/json');
    exit(json_encode(['status' => 'err', 'reason' => 'Invalid URL']));
}
