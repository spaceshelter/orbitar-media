<?php 

/*
* MP4 normaliser
*
* Uploads can come from anywhere, so every MP4 is checked against what browsers and
* phone decoders actually play (see VideoController::planNormalization). Files that
* fall short are rewritten in place: stream copy when only the container or the audio
* is at fault, H.264 transcode otherwise. Compliant files are left untouched.
*
* usage: php re-encode_mp4.php <hash> [<hash> ...]   normalise the given hashes
*        php re-encode_mp4.php all [dryrun]           scan data/ and normalise every mp4
*
* Params:
* all       => Required for the full scan. Every rewritten file gets a new ETag, and a CDN in
*              front may still hold chunks of the old bytes, so this is deliberately not the default.
* altfolder => Check ALT_FOLDER (if defined) instead of data/
* dryrun    => Only report what would be done
*
* Concurrency with uploads is bounded by the shared conversion slots (MAX_CONCURRENT_VIDEO_HANDLERS).
*/ 


if(php_sapi_name() !== 'cli') exit('This script can only be called via CLI');
error_reporting(E_ALL & ~E_DEPRECATED & ~E_STRICT & ~E_NOTICE);

// basic path definitions
define('DS', DIRECTORY_SEPARATOR);
define('ROOT', dirname(__FILE__).'/..');

//loading default settings if exist
if(!file_exists(ROOT.DS.'inc'.DS.'config.inc.php'))
	exit('Rename /inc/example.config.inc.php to /inc/config.inc.php first!');
include_once(ROOT.DS.'inc'.DS.'config.inc.php');

//loading core and controllers
include_once(ROOT.DS.'inc'.DS.'core.php');
require_once(ROOT . DS . 'content-controllers' . DS. 'video'. DS . 'video.controller.php');

if(!defined('FFMPEG_BINARY')||FFMPEG_BINARY=='' || !FFMPEG_BINARY) exit('Error: FFMPEG_BINARY not defined, no clue where to look');

$vc = new VideoController();
$flags = array('dryrun', 'altfolder', 'all', 'force');
$dryrun = in_array('dryrun',$argv);
$scanAll = in_array('all',$argv);
$hashArgs = array_values(array_diff(array_slice($argv, 1), $flags));
$files = array(); // path => hash (or filename for the alt folder)

if(in_array('altfolder',$argv) && defined('ALT_FOLDER') && ALT_FOLDER && is_dir(ALT_FOLDER))
{
    echo "[i] Checking only the alt folder\n";
    foreach(scandir(ALT_FOLDER) as $filename)
    {
        $vid = ALT_FOLDER.DS.$filename;
        if(!is_file($vid)) continue;
        if(in_array(strtolower(pathinfo($vid, PATHINFO_EXTENSION)),$vc->getRegisteredExtensions()))
            $files[$vid] = $filename;
    }
}
else
{
    $dir = ROOT.DS.'data'.DS;
    foreach($hashArgs as $arg)
    {
        if(isExistingHash($arg) && in_array(strtolower(pathinfo($dir.$arg, PATHINFO_EXTENSION)),$vc->getRegisteredExtensions()))
            $files[$dir.$arg.DS.$arg] = $arg;
        else
            echo " [!] $arg: not an existing mp4 hash, ignored\n";
    }

    // A typo'd or already-deleted hash must never silently turn into "rewrite the whole corpus"
    if(count($files)==0 && count($hashArgs)>0)
        exit("[!] None of the given hashes resolved to an mp4; refusing to scan data/\n");

    if(count($hashArgs)==0)
    {
        if(!$scanAll)
            exit("usage: re-encode_mp4.php <hash> [<hash> ...] | all [dryrun]\n"
                ."[!] Scanning all of data/ rewrites every non-compliant file (new bytes, new ETag) while a CDN may\n"
                ."    still cache the old ones. Pass 'all' explicitly, ideally after 'all dryrun'.\n");
        echo "[i] Finding local mp4 files\n";
        foreach(scandir($dir) as $filename)
        {
            $vid = $dir.$filename.DS.$filename;
            if(!is_file($vid)) continue;
            if(in_array(strtolower(pathinfo($vid, PATHINFO_EXTENSION)),$vc->getRegisteredExtensions()))
                $files[$vid] = $filename;
        }
    }
}

if(count($files)==0) exit('No MP4 files found'."\n");

echo "[i] Got ".count($files)." files".($dryrun ? " (dry run)" : "")."\n";

$isAlt = in_array('altfolder',$argv);
foreach($files as $path => $hash)
{
    $plan = $vc->planNormalization($path);
    if($plan === false)
    {
        echo " [!] $hash: cannot be probed, skipping\n";
        continue;
    }
    if(!$plan['needed'])
    {
        echo " [i] $hash: already fine\n";
        continue;
    }

    $what = $plan['video']==='transcode' ? 'transcode' : 'remux';
    echo " [i] $hash: $what (".implode('; ',$plan['reasons']).")";
    if($dryrun) { echo " [dry run]\n"; continue; }

    $slot = $vc->acquireConversionSlot(true); // shares the cap with uploads; waits for a free slot
    try
    {
        $ok = $vc->normalize($path,$plan);
    }
    finally
    {
        $vc->releaseConversionSlot($slot);
    }
    if(!$ok)
    {
        echo "\tFAILED\n";
        continue;
    }

    if(!$isAlt)
    {
        if(defined('ALT_FOLDER') && ALT_FOLDER && is_dir(ALT_FOLDER))
            copy($path,ALT_FOLDER.DS.$hash);

        //file got new content so register its checksum and refresh the remote copy
        addSha1($hash,sha1_file($path));
        storageControllerUpload($hash);
    }

    echo "\tdone\n";
}
