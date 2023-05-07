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

function callCoubApi($hash) {
    $url = "https://coub.com/api/v2/coubs/".$hash;
    $content = file_get_contents($url);
    return json_decode($content, true);
}

function handleCoubRequest($hash) {
    $coubData = callCoubApi($hash);
    if (isset($coubData['picture'])) {
        header('Location: '.$coubData['picture']);
        exit();
    } else {
        if (isset($coubData['error'])) {
            exit(json_encode(array('status' => 'err', 'reason' => $coubData['error'])));
        } else {
            exit(json_encode(array('status' => 'err', 'reason' => 'Picture not found')));
        }
    }
}

if (isset($_REQUEST['hash']) && preg_match('/^(\w{3,})$/', $_REQUEST['hash'], $matches)) {
    handleCoubRequest($matches[1]);
} else {
    exit(json_encode(array('status' => 'err', 'reason' => 'Invalid URL')));
}