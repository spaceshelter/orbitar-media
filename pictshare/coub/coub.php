<?php
// basic path definitions
define('DS', DIRECTORY_SEPARATOR);
define('ROOT', dirname(__FILE__).'/..');

//loading default settings if exist
if(!file_exists(ROOT.DS.'inc'.DS.'config.inc.php'))
    exit('Rename /inc/example.config.inc.php to /inc/config.inc.php first!');
include_once(ROOT.DS.'inc'.DS.'config.inc.php');

//loading core and controllers
include_once(ROOT . DS . 'inc' . DS. 'core.php');
loadAllContentControllers();

define('COUB_CACHE_PREFIX', 'coub_');
define('COUB_SEMAPHORE_SLOTS', 5);

function getCoubCacheHash($hash) {
    return COUB_CACHE_PREFIX . $hash . '.jpg';
}

function getCoubCachePath($hash) {
    $cacheHash = getCoubCacheHash($hash);
    return ROOT . DS . 'data' . DS . $cacheHash . DS . $cacheHash;
}

function coubCacheExists($hash) {
    return file_exists(getCoubCachePath($hash));
}

function tryPullFromStorage($hash) {
    $cacheHash = getCoubCacheHash($hash);
    $sc = getStorageControllers();
    foreach($sc as $contr) {
        $c = new $contr();
        if($c->isEnabled() && $c->hashExists($cacheHash)) {
            $tmpFile = ROOT . DS . 'tmp' . DS . $cacheHash;
            $c->pullFile($cacheHash, $tmpFile);
            storeCoubCache($hash, $tmpFile);
            unlink($tmpFile);
            return true;
        }
    }
    return false;
}

function storeCoubCache($hash, $sourceFile) {
    $cacheHash = getCoubCacheHash($hash);
    $dir = ROOT . DS . 'data' . DS . $cacheHash;
    if(!is_dir($dir)) mkdir($dir, 0777, true);
    copy($sourceFile, $dir . DS . $cacheHash);

    // Sync to S3 storage controllers
    storageControllerUpload($cacheHash);
}

function acquireSemaphore($hash) {
    $slot = crc32($hash) % COUB_SEMAPHORE_SLOTS;
    $fp = fopen("/tmp/coub_sem_{$slot}.lock", "c");
    flock($fp, LOCK_EX);
    return $fp;
}

function releaseSemaphore($fp) {
    flock($fp, LOCK_UN);
    fclose($fp);
}

function callCoubApi($hash) {
    $url = "https://coub.com/api/v2/coubs/".$hash;
    $content = file_get_contents($url);
    return json_decode($content, true);
}

function fetchAndCacheCoub($hash) {
    $sem = acquireSemaphore($hash);
    try {
        // Double-check after acquiring lock (another process may have cached it)
        if(coubCacheExists($hash)) {
            return true;
        }

        // Fetch from Coub API
        $coubData = callCoubApi($hash);
        if(!isset($coubData['picture'])) {
            return false;
        }

        // Download thumbnail
        $tmpFile = ROOT . DS . 'tmp' . DS . 'coub_' . $hash . '_' . uniqid() . '.jpg';
        $imageData = file_get_contents($coubData['picture']);
        if($imageData === false) {
            return false;
        }
        file_put_contents($tmpFile, $imageData);

        // Store locally and sync to S3
        storeCoubCache($hash, $tmpFile);
        unlink($tmpFile);

        return true;
    } finally {
        releaseSemaphore($sem);
    }
}

function serveCoubImage($hash) {
    $path = getCoubCachePath($hash);
    header('Content-Type: image/jpeg');
    header('Cache-Control: public, max-age=31536000');
    readfile($path);
    exit();
}

function handleCoubRequest($hash) {
    // 1. Check local cache
    if(coubCacheExists($hash)) {
        serveCoubImage($hash);
    }

    // 2. Check S3 storage
    if(tryPullFromStorage($hash)) {
        serveCoubImage($hash);
    }

    // 3. Fetch from Coub API (with semaphore)
    if(fetchAndCacheCoub($hash)) {
        serveCoubImage($hash);
    }

    // 4. Error - return JSON error
    header('HTTP/1.1 404 Not Found');
    header('Content-Type: application/json');
    exit(json_encode(['status' => 'err', 'reason' => 'Coub not found']));
}

if (isset($_REQUEST['hash']) && preg_match('/^(\w{3,})$/', $_REQUEST['hash'], $matches)) {
    handleCoubRequest($matches[1]);
} else {
    header('Content-Type: application/json');
    exit(json_encode(array('status' => 'err', 'reason' => 'Invalid URL')));
}
