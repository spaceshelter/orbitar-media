<?php

define('MAX_CONCURRENT_VIDEO_HANDLERS',
    getenv('MAX_CONCURRENT_VIDEO_HANDLERS') !== false ? (int) getenv('MAX_CONCURRENT_VIDEO_HANDLERS') : 4
);

/**
 * MP4 handling.
 *
 * Every upload is probed with ffprobe and compared against what browsers and phone
 * hardware decoders actually play: H.264 up to High profile / level 5.2, 8-bit 4:2:0,
 * progressive, even dimensions, AAC or MP3 audio, in an MP4 whose moov atom precedes
 * mdat (so playback starts before the whole file is downloaded).
 *
 * - Compliant files are stored untouched.
 * - Files that only need a container fix (moov at the end, extra streams) or an audio
 *   re-encode are remuxed synchronously during the upload: the video stream is copied
 *   bit for bit, so this takes about as long as copying the file.
 * - Files whose video stream needs transcoding are converted in the background,
 *   as before, capped by MAX_CONCURRENT_VIDEO_HANDLERS.
 */
class VideoController implements ContentController
{
    // H.264 profiles that every relevant decoder handles (ffprobe spelling, lower-cased)
    private static $compatibleVideoProfiles = array('constrained baseline', 'baseline', 'main', 'high');
    // Level 5.2 covers 4K@60; anything above is exotic and worth normalising
    private static $maxH264Level = 52;
    private static $compatiblePixelFormats = array('yuv420p', 'yuvj420p');
    private static $compatibleAudioCodecs = array('aac', 'mp3');
    private static $interlacedFieldOrders = array('tt', 'bb', 'tb', 'bt');
    // Level 5.2 ceiling expressed as what we downscale to when a transcode is needed anyway
    private static $maxPixels = 3840 * 2160;
    private static $maxSide = 4096;
    private static $maxFps = 60;

    //returns all extensions registered by this type of content
    public function getRegisteredExtensions(){return array('mp4');}

    public function handleHash($hash,$url)
    {
        $path = ROOT.DS.'data'.DS.$hash.DS.$hash;

        //check if video should be resized
        foreach($url as $u)
            if(isSize($u)==true)
                $size = $u;
        if($size)
        {
            $s = sizeStringToWidthHeight($size);
            $width = $s['width'];
            $newpath = ROOT.DS.'data'.DS.$hash.DS.$width.'_'.$hash;
            if(!file_exists($newpath))
                $this->resize($path,$newpath,$width);
            $path = $newpath;
        }


        if(in_array('raw',$url))
            $this->serveMP4($path,$hash);
        else if(in_array('preview',$url))
        {
            $preview = $path.'_preview.jpg';
            if(!file_exists($preview))
            {
                $this->saveFirstFrameOfMP4($path,$preview);
            }

            sendFileValidators($preview);
            header ("Content-type: image/jpeg");
            header('Cache-control: public, max-age=31536000');

            readfile($preview);

        }
        else if(in_array('download',$url))
        {
            if (file_exists($path)) {
                header('Content-Description: File Transfer');
                header('Content-Type: application/octet-stream');
                header('Content-Disposition: attachment; filename="'.basename($path).'"');
                header('Expires: 0');
                header('Cache-Control: must-revalidate');
                header('Pragma: public');
                header('Content-Length: ' . filesize($path));
                readfile($path);
                exit;
            }
        }
        else
        {
            $data = array('url'=>implode('/',$url),'hash'=>$hash,'filesize'=>renderSize(filesize($path)));
            renderTemplate('video',$data);
        }
    }

    public function handleUpload($tmpfile,$hash=false)
    {
        if($hash===false)
            if (defined('HASH_DIMS_AES_KEY')) {
                $dimensions = $this->getVideoDimensions($tmpfile);
                if (!$dimensions) {
                    return array('status' => 'err', 'hash' => $hash, 'reason' => 'Can\'t get video dimensions');
                }
                $w = $dimensions['width'];
                $h = $dimensions['height'];
                $hash = getNewCryptoHash(HASH_DIMS_AES_KEY, 'mp4', $w, $h);
            } else {
                $hash = getNewHash('mp4',6);
            }
        else
        {
            $hash.='.mp4';
            if(isExistingHash($hash))
                return array('status'=>'err','hash'=>$hash,'reason'=>'Custom hash already exists');
        }

        $file = storeFile($tmpfile,$hash,true);

        $plan = $this->planNormalization($file);
        if($plan === false || $plan['video'] === 'transcode')
        {
            // Real transcode: too slow for the request, hand it to the background worker.
            $conversionsRunningNow = $this->getCurrentNumberOfRunningConversions();
            if ($conversionsRunningNow >= MAX_CONCURRENT_VIDEO_HANDLERS) {
                return array('status'=>'err','hash'=>$hash,'reason'=>'System is busy, try again later');
            }
            system("nohup php ".ROOT.DS.'tools'.DS.'re-encode_mp4.php '.escapeshellarg($hash)." > /dev/null 2> /dev/null &");
        }
        else if($plan['needed'])
        {
            // Stream copy only: finish before the URL is handed out so the bytes never change afterwards.
            if($this->normalize($file, $plan))
                addSha1($hash, sha1_file($file)); // re-uploads of the normalised file must dedupe too
            else
                error_log("pictshare: remux of $hash failed, serving the original (".implode('; ', $plan['reasons']).")");
        }

        return array('status'=>'ok','hash'=>$hash,'url'=>URL.$hash);
    }

    function getCurrentNumberOfRunningConversions()
    {
        $command = "ps aux | grep 're-encode_mp4.php' | grep -v grep | wc -l";
        $output = null;
        $return_var = null;
        exec($command, $output, $return_var);
        return intval($output[0]);
    }

    /**
     * Streams an MP4 with byte-range support and cache validators.
     * Handles If-None-Match / If-Modified-Since (304) and If-Range (falls back to the
     * whole file when the client's validator is stale), so a client holding pieces of an
     * older version of the file can never splice them together with the new one.
     */
    function serveMP4($path,$hash)
    {
        if(!is_file($path) || !is_readable($path))
        {
            header('HTTP/1.1 404 Not Found');
            die('file not found');
        }

        // Open first and describe what was opened: a background normalisation may rename a
        // new file over $path at any moment, and validators must never describe another body.
        $fp = fopen($path, 'rb');
        if(!$fp)
        {
            header('HTTP/1.1 500 Internal Server Error');
            die('file not readable');
        }
        $st = fstat($fp);
        $size = $st['size'];
        $etag = sendFileValidators($path, $st); // exits with 304 when the client is up to date

        header('Content-Type: video/mp4');
        header('Cache-Control: public, max-age=31536000');
        header('Accept-Ranges: bytes');

        $start = 0;
        $end = $size - 1;
        if(isset($_SERVER['HTTP_RANGE']) && ifRangeMatches($etag, $st['mtime']))
        {
            $range = parseByteRange($_SERVER['HTTP_RANGE'], $size);
            if($range === false)
            {
                header('HTTP/1.1 416 Range Not Satisfiable');
                header("Content-Range: bytes */$size");
                exit;
            }
            if($range !== null)
            {
                list($start, $end) = $range;
                header('HTTP/1.1 206 Partial Content');
                header("Content-Range: bytes $start-$end/$size");
            }
        }

        $length = $end - $start + 1;
        header('Content-Length: '.$length);
        if($_SERVER['REQUEST_METHOD'] === 'HEAD' || $length <= 0)
        {
            fclose($fp);
            exit;
        }

        set_time_limit(0);
        fseek($fp, $start);
        $remaining = $length;
        while($remaining > 0 && !feof($fp))
        {
            $chunk = fread($fp, min(8192, $remaining));
            if($chunk === false || $chunk === '') break;
            echo $chunk;
            flush();
            $remaining -= strlen($chunk);
        }
        fclose($fp);
        exit;
    }

    /**
     * Runs ffprobe and returns the decoded JSON (format + streams), or false.
     */
    function probe($file)
    {
        $bin = escapeshellcmd(preg_replace('/ffmpeg$/', 'ffprobe', FFMPEG_BINARY));
        $cmd = "$bin -v error -print_format json -show_format -show_streams ".escapeshellarg($file)." 2>/dev/null";
        $json = shell_exec($cmd);
        if(!$json) return false;
        $data = json_decode($json, true);
        if(!is_array($data) || empty($data['streams'])) return false;
        return $data;
    }

    /**
     * The stream we would play: first video stream that is not cover art.
     */
    function primaryVideoStream($probe)
    {
        foreach($probe['streams'] as $s)
        {
            if(($s['codec_type'] ?? '') !== 'video') continue;
            if(!empty($s['disposition']['attached_pic'])) continue;
            return $s;
        }
        return null;
    }

    function primaryAudioStream($probe)
    {
        foreach($probe['streams'] as $s)
            if(($s['codec_type'] ?? '') === 'audio')
                return $s;
        return null;
    }

    /**
     * Frame rate a transcode of this stream would run at: the higher of the average and
     * the nominal (timebase) rate. ffmpeg 4.4 pads variable-rate input up to r_frame_rate
     * when writing MP4, so a VFR clip averaging 40 fps with r_frame_rate 120 comes out at 120.
     * Returns 0 when unknown.
     */
    function frameRate($stream)
    {
        $max = 0;
        foreach(array('avg_frame_rate', 'r_frame_rate') as $k)
        {
            if(empty($stream[$k]) || strpos($stream[$k], '/') === false) continue;
            list($n, $d) = explode('/', $stream[$k], 2);
            if((float)$d > 0 && (float)$n > 0) $max = max($max, (float)$n / (float)$d);
        }
        return $max;
    }

    /**
     * Rotation in degrees from the display matrix / rotate tag (0 when absent).
     */
    function streamRotation($stream)
    {
        if(isset($stream['tags']['rotate']) && is_numeric($stream['tags']['rotate']))
            return ((int)$stream['tags']['rotate']) % 360;
        if(!empty($stream['side_data_list']))
            foreach($stream['side_data_list'] as $sd)
                if(isset($sd['rotation']) && is_numeric($sd['rotation']))
                    return ((int)round($sd['rotation'])) % 360;
        return 0;
    }

    /**
     * Interlaced content renders with combing in browsers (no deinterlacer), so it
     * gets transcoded through yadif. Container/stream headers rarely say (ffprobe
     * reports field_order "unknown" for most H.264), so when they are silent the
     * first few frames are decoded and their interlaced flag inspected.
     */
    function isInterlaced($file, $streamIndex, $fieldOrder)
    {
        if(in_array($fieldOrder, self::$interlacedFieldOrders)) return true;
        if($fieldOrder === 'progressive') return false;

        $bin = escapeshellcmd(preg_replace('/ffmpeg$/', 'ffprobe', FFMPEG_BINARY));
        $cmd = "$bin -v error -select_streams ".(int)$streamIndex." -read_intervals ".escapeshellarg('%+#5')
             ." -show_entries frame=interlaced_frame -of csv=p=0 ".escapeshellarg($file)." 2>/dev/null";
        $out = shell_exec($cmd);
        if(!$out) return false;
        foreach(explode("\n", trim($out)) as $line)
            if(trim($line) === '1') return true;
        return false;
    }

    /**
     * True when the moov atom precedes mdat (progressive download friendly),
     * false when it trails it, null when the top-level box structure can't be parsed.
     */
    function moovBeforeMdat($file)
    {
        $size = filesize($file);
        $fh = fopen($file, 'rb');
        if(!$fh) return null;
        $pos = 0;
        $seenMdat = false;
        $result = null;
        while($pos + 8 <= $size)
        {
            fseek($fh, $pos);
            $hdr = fread($fh, 8);
            if(strlen($hdr) < 8) break;
            $u = unpack('Nsize/a4type', $hdr);
            $boxSize = $u['size'];
            $type = $u['type'];
            if($boxSize == 1)
            {
                $ext = fread($fh, 8);
                if(strlen($ext) < 8) break;
                $u2 = unpack('Jsize', $ext);
                $boxSize = $u2['size'];
                if($boxSize < 16) break;
            }
            else if($boxSize == 0)
                $boxSize = $size - $pos;
            else if($boxSize < 8)
                break;

            if($type === 'moov') { $result = !$seenMdat; break; }
            if($type === 'mdat') $seenMdat = true;
            $pos += $boxSize;
        }
        fclose($fh);
        return $result;
    }

    /**
     * Decides what (if anything) has to be done to make $file play everywhere.
     *
     * Returns false when the file can't be probed, otherwise:
     *   video    => 'copy' | 'transcode'
     *   audio    => 'copy' | 'transcode' | 'none'
     *   remux    => bool   (container needs rewriting: moov placement, stray streams)
     *   needed   => bool   (anything at all to do)
     *   reasons  => string[] human readable, for logs
     *   vindex / aindex / interlaced / channels => details for the ffmpeg command
     */
    function planNormalization($file)
    {
        $probe = $this->probe($file);
        if($probe === false) return false;

        $v = $this->primaryVideoStream($probe);
        if($v === null) return false;
        $a = $this->primaryAudioStream($probe);

        $plan = array(
            'video' => 'copy', 'audio' => $a ? 'copy' : 'none', 'remux' => false,
            'reasons' => array(), 'vindex' => $v['index'], 'aindex' => $a ? $a['index'] : null,
            'interlaced' => false, 'cap_size' => false, 'cap_fps' => false, 'channels' => $a ? (int)($a['channels'] ?? 2) : 0,
        );

        // --- video stream
        $codec = strtolower($v['codec_name'] ?? '');
        if($codec !== 'h264')
            $plan['reasons'][] = "video codec $codec";

        $pix = strtolower($v['pix_fmt'] ?? '');
        if(!in_array($pix, self::$compatiblePixelFormats))
            $plan['reasons'][] = "pixel format $pix";

        $profile = strtolower(trim($v['profile'] ?? ''));
        if($codec === 'h264' && $profile !== '' && $profile !== 'unknown' && !in_array($profile, self::$compatibleVideoProfiles))
            $plan['reasons'][] = "profile $profile";

        $level = isset($v['level']) ? (int)$v['level'] : 0;
        if($codec === 'h264' && $level > self::$maxH264Level)
        {
            $plan['reasons'][] = "level $level";
            $plan['cap_size'] = true; // whatever pushed the level up, fitting into 4K brings it back
        }

        $w = (int)($v['width'] ?? 0);
        $h = (int)($v['height'] ?? 0);
        if($w <= 0 || $h <= 0 || ($w % 2) || ($h % 2))
            $plan['reasons'][] = "dimensions {$w}x{$h}";

        // Anything beyond 4K@60 lands above level 5.2 whatever the source codec says.
        // Level 5.2 also bounds each dimension on its own (a 11520x720 strip is level 6),
        // hence the per-side limits next to the pixel budget.
        $fps = $this->frameRate($v);
        if(($w * $h) > self::$maxPixels || $w > self::$maxSide || $h > self::$maxSide)
        {
            $plan['reasons'][] = "size {$w}x{$h}";
            $plan['cap_size'] = true;
        }
        if($fps > self::$maxFps)
        {
            $plan['reasons'][] = "frame rate ".round($fps, 2);
            $plan['cap_fps'] = true;
        }

        $fieldOrder = strtolower($v['field_order'] ?? 'unknown');
        if($this->isInterlaced($file, $v['index'], $fieldOrder))
        {
            $plan['interlaced'] = true;
            $plan['reasons'][] = "interlaced ($fieldOrder)";
        }

        if(count($plan['reasons']) > 0)
            $plan['video'] = 'transcode';

        // --- audio stream
        if($a)
        {
            $acodec = strtolower($a['codec_name'] ?? '');
            if(!in_array($acodec, self::$compatibleAudioCodecs))
            {
                $plan['audio'] = 'transcode';
                $plan['reasons'][] = "audio codec $acodec";
            }
        }

        // --- container
        $format = strtolower($probe['format']['format_name'] ?? '');
        if(strpos($format, 'mp4') === false && strpos($format, 'mov') === false)
        {
            $plan['remux'] = true;
            $plan['reasons'][] = "container $format";
        }

        $streamCount = count($probe['streams']);
        $wanted = 1 + ($a ? 1 : 0);
        if($streamCount > $wanted)
        {
            $plan['remux'] = true;
            $plan['reasons'][] = "$streamCount streams";
        }

        $moovFirst = $this->moovBeforeMdat($file);
        if($moovFirst !== true)
        {
            $plan['remux'] = true;
            $plan['reasons'][] = $moovFirst === false ? 'moov after mdat' : 'unparseable atom layout';
        }

        $plan['needed'] = $plan['video'] === 'transcode' || $plan['audio'] === 'transcode' || $plan['remux'];
        return $plan;
    }

    /**
     * ffmpeg command line that produces a browser-friendly MP4 at $out according to $plan.
     */
    function normalizationCommand($file, $plan, $out)
    {
        $bin = escapeshellcmd(FFMPEG_BINARY);
        $cmd = "$bin -y -nostdin -loglevel error -i ".escapeshellarg($file);
        $cmd .= " -map 0:".(int)$plan['vindex'];
        if($plan['aindex'] !== null)
            $cmd .= " -map 0:".(int)$plan['aindex'];

        if($plan['video'] === 'transcode')
        {
            $filters = array();
            if($plan['interlaced']) $filters[] = 'yadif';
            // fit inside 4K and 60 fps so the resulting level stays <= 5.2
            if(!empty($plan['cap_fps']))
                $filters[] = 'fps='.self::$maxFps;
            if(!empty($plan['cap_size'])) // shrink to fit 3840x2160, never enlarge
                $filters[] = "scale=w='min(iw,3840)':h='min(ih,2160)':force_original_aspect_ratio=decrease";
            $filters[] = 'scale=trunc(iw/2)*2:trunc(ih/2)*2';
            $cmd .= " -c:v libx264 -preset medium -crf 23 -profile:v high -pix_fmt yuv420p -vf ".escapeshellarg(implode(',', $filters));
        }
        else
            $cmd .= " -c:v copy";

        if($plan['audio'] === 'transcode')
        {
            $cmd .= " -c:a aac -b:a 128k";
            if($plan['channels'] > 2) $cmd .= " -ac 2";
        }
        else if($plan['audio'] === 'copy')
            $cmd .= " -c:a copy";

        // chapters would be re-materialised as a text/data stream and defeat the stream-count check
        $cmd .= " -map_chapters -1 -movflags +faststart -f mp4 ".escapeshellarg($out);
        return $cmd;
    }

    /**
     * Rewrites $file in place (atomically, via rename) so that it plays everywhere.
     * Returns true when the file was replaced, false when nothing was needed or ffmpeg failed.
     */
    function normalize($file, $plan = null)
    {
        if($plan === null) $plan = $this->planNormalization($file);
        if($plan === false || !$plan['needed']) return false;

        // Same directory as the target so the final rename is atomic (data/ is a separate mount from tmp/)
        $out = $file.'.converting';
        @unlink($out);
        $cmd = $this->normalizationCommand($file, $plan, $out);
        $rc = 0;
        system($cmd, $rc);

        if($rc !== 0 || !is_file($out) || filesize($out) === 0 || $this->primaryVideoStream($this->probe($out) ?: array('streams'=>array())) === null)
        {
            error_log("pictshare: ffmpeg failed (rc=$rc) for $file: $cmd");
            @unlink($out);
            return false;
        }
        $after = $this->planNormalization($out);
        if($after !== false && $after['needed'])
            error_log("pictshare: normalised $file still flagged (".implode('; ', $after['reasons'])."), keeping it anyway");

        if(!rename($out, $file))
        {
            error_log("pictshare: could not replace $file with normalised output");
            @unlink($out);
            return false;
        }
        return true;
    }

    /**
     * Is this something we can turn into a playable MP4? Used for upload type detection.
     * Requires an MP4/MOV container with a real (non cover-art) H.264 video stream of non-zero duration.
     */
    function isProperMP4($filename)
    {
        $probe = $this->probe($filename);
        if($probe === false) return false;
        $format = strtolower($probe['format']['format_name'] ?? '');
        if(strpos($format, 'mp4') === false && strpos($format, 'mov') === false) return false;
        $v = $this->primaryVideoStream($probe);
        if($v === null || strtolower($v['codec_name'] ?? '') !== 'h264') return false;
        $duration = (float)($probe['format']['duration'] ?? $v['duration'] ?? 0);
        return $duration > 0;
    }

    function saveFirstFrameOfMP4($path,$target)
	{
		$bin = escapeshellcmd(FFMPEG_BINARY);
		$file = escapeshellarg($path);
		$cmd = "$bin -y -i $file -vframes 1 -f image2 $target";

		system($cmd);
    }

    /**
     * Displayed dimensions of the video (rotation from the container applied),
     * so that dimension-encoding hashes describe what the viewer sees.
     */
    function getVideoDimensions($path)
    {
        $probe = $this->probe($path);
        if($probe === false) return false;
        $v = $this->primaryVideoStream($probe);
        if($v === null) return false;
        $w = (int)($v['width'] ?? 0);
        $h = (int)($v['height'] ?? 0);
        if($w <= 0 || $h <= 0) return false;
        if(abs($this->streamRotation($v)) % 180 === 90)
            list($w, $h) = array($h, $w);
        return array('width' => $w, 'height' => $h);
    }

    function resize($in,$out,$width)
	{
		$file = escapeshellarg($in);
		$bin = escapeshellcmd(FFMPEG_BINARY);

		$addition = '-c:v libx264 -preset medium -crf 23 -profile:v high -pix_fmt yuv420p -movflags +faststart';
        $height = 'trunc(ow/a/2)*2';

		$cmd = "$bin -i $file -y -vf scale=\"$width:$height\" $addition ".escapeshellarg($out);
		system($cmd);

		return (file_exists($out) && filesize($out)>0);
	}
}
