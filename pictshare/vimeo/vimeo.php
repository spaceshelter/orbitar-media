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

function callVimeoApi($hash) {
    $content = file_get_contents("https://vimeo.com/api/v2/video/{$hash}.json");
    return json_decode($content, true);
}

function handleVimeoRequest($hash) {
    $vimeoData = callVimeoApi($hash);
    $vimeo = reset($vimeoData);
    if (isset($vimeo) && isset($vimeo['thumbnail_large'])) {
        header("Location: {$vimeo['thumbnail_large']}");
        exit();
    } else {
        exit(json_encode(array('status' => 'err', 'reason' => 'Picture not found')));
    }
}

if (isset($_REQUEST['hash']) && preg_match('/^(\w{3,})$/', $_REQUEST['hash'], $matches)) {
    handleVimeoRequest($matches[1]);
} else {
    exit(json_encode(array('status' => 'err', 'reason' => 'Invalid URL')));
}