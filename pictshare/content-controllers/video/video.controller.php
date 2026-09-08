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

        // No /<width>/ variants for video: that was an unauthenticated full transcode on the
        // request path (any numeric segment, no bound), nothing links to it, and it would
        // compete with uploads for the conversion slots.
        foreach($url as $u)
            if(isSize($u)==true)
            {
                header('HTTP/1.1 404 Not Found');
                header('Cache-Control: no-store');
                die('video size variants are not available');
            }


        if(in_array('raw',$url))
            $this->serveMP4($path,$hash);
        else if(in_array('preview',$url))
        {
            $preview = $path.'_preview.jpg';
            if(!file_exists($preview) && !$this->saveFirstFrameOfMP4($path,$preview))
            {
                header('HTTP/1.1 503 Service Unavailable');
                header('Cache-Control: no-store');
                die('could not render preview');
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
        $probe = $this->probe($tmpfile);
        if($hash===false)
            if (defined('HASH_DIMS_AES_KEY')) {
                $dimensions = $this->getVideoDimensions($tmpfile, $probe);
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

        // Decide (and reserve capacity) on the temp file, before anything is published: once
        // storeFile() has run, the hash and its checksum are visible to concurrent uploads, and a
        // "busy" rejection could no longer be rolled back safely.
        $plan = $this->planNormalization($tmpfile, $probe);
        $busy = array('status'=>'err','hash'=>$hash,'reason'=>'System is busy, try again later');

        if($plan === false || $plan['video'] === 'transcode')
        {
            // Real transcode: too slow for the request, hand it to the background worker, but only
            // if a slot is free right now (probe and release), so the worker does not sit blocked
            // while the un-normalised original is already being served.
            $probeSlot = $this->acquireConversionSlot();
            if($probeSlot === false)
                return $busy;
            $this->releaseConversionSlot($probeSlot);
            storeFile($tmpfile,$hash,true);
            // The worker's stdout/stderr (per-file outcome, error_log() from normalize()) land in a file
            // that start.sh pre-creates and tails, so background failures are visible in `docker logs`.
            system("nohup php ".ROOT.DS.'tools'.DS.'re-encode_mp4.php '.escapeshellarg($hash)." >> /var/log/nginx/pictshare/reencode.log 2>&1 &");
        }
        else if($plan['needed'])
        {
            // Stream copy only: finish before the URL is handed out so the bytes never change afterwards.
            // Still an ffmpeg run on the request path, so it takes one of the shared conversion slots.
            $slot = $this->acquireConversionSlot();
            if($slot === false)
                return $busy;
            try
            {
                $file = storeFile($tmpfile,$hash,true);
                if($this->normalize($file, $plan))
                    addSha1($hash, sha1_file($file)); // re-uploads of the normalised file must dedupe too
                else
                    error_log("pictshare: remux of $hash failed, serving the original (".implode('; ', $plan['reasons']).")");
            }
            finally
            {
                $this->releaseConversionSlot($slot);
            }
        }
        else
            storeFile($tmpfile,$hash,true);

        return array('status'=>'ok','hash'=>$hash,'url'=>URL.$hash);
    }

    /**
     * Per-process temp name next to $target (same filesystem, so the final rename is atomic).
     * Unique per call: two requests generating the same uncached file must not share it, or the
     * first rename would pull the pathname from under the second encoder.
     */
    function tempNameFor($target)
    {
        return $target.'.'.getmypid().'.'.bin2hex(random_bytes(4)).'.tmp';
    }

    /**
     * Bounded number of concurrent ffmpeg runs across both the upload path and the background
     * worker: MAX_CONCURRENT_VIDEO_HANDLERS flock()ed slot files in tmp/. Returns the lock
     * handle, or false when every slot is taken. $waitSeconds > 0 keeps retrying for that long
     * (never forever: a wedged slot must surface as a failure, not as sleeping processes).
     */
    function acquireConversionSlot($waitSeconds = 0)
    {
        $slots = max(1, (int)MAX_CONCURRENT_VIDEO_HANDLERS);
        $deadline = time() + (int)$waitSeconds;
        while(true)
        {
            $openable = 0;
            for($i = 0; $i < $slots; $i++)
            {
                $lock = ROOT.DS.'tmp'.DS."video-slot-$i.lock";
                // flock() is per inode and ignores the open mode, so a read-only handle locks just as
                // well when another user (root running the CLI tool) owns the file. Never unlink and
                // recreate: that would be a second inode, i.e. a second copy of the same slot.
                $fh = @fopen($lock, 'c') ?: @fopen($lock, 'r');
                if(!$fh) continue;
                $openable++;
                @chmod($lock, 0666); // CLI (root) and PHP-FPM (nginx) share these
                if(flock($fh, LOCK_EX | LOCK_NB)) return $fh;
                fclose($fh);
            }
            if($openable === 0)
                error_log("pictshare: none of the $slots conversion slot files in ".ROOT.DS.'tmp'." can be opened; every conversion will report busy");
            if(time() >= $deadline) return false;
            sleep(1);
        }
    }

    function releaseConversionSlot($fh)
    {
        if(!$fh) return;
        flock($fh, LOCK_UN);
        fclose($fh);
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
            header('Cache-Control: no-store');
            die('file not found');
        }

        // Open first and describe what was opened: a background normalisation may rename a
        // new file over $path at any moment, and validators must never describe another body.
        $fp = fopen($path, 'rb');
        if(!$fp)
        {
            header('HTTP/1.1 500 Internal Server Error');
            header('Cache-Control: no-store');
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
                header('Cache-Control: no-store'); // never let a CDN keep an error for a year
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
        if(($_SERVER['REQUEST_METHOD'] ?? 'GET') === 'HEAD' || $length <= 0)
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
        if(!$json)
        {
            // legitimate for a non-media file, but also what a missing/crashing ffprobe looks like
            if(is_file($file)) error_log("pictshare: ffprobe produced no output for $file");
            return false;
        }
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
     * One of ffprobe's rational frame-rate fields as float (0 when unknown).
     * avg_frame_rate is the real rate; r_frame_rate is the timebase-derived nominal rate, which
     * VFR sources (screen recordings, concatenations) inflate to 120, 1000 or more.
     */
    function frameRate($stream, $key = 'avg_frame_rate')
    {
        if(empty($stream[$key]) || strpos($stream[$key], '/') === false) return 0;
        list($n, $d) = explode('/', $stream[$key], 2);
        if((float)$d <= 0 || (float)$n <= 0) return 0;
        return (float)$n / (float)$d;
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
    function planNormalization($file, $probe = null)
    {
        if($probe === null) $probe = $this->probe($file);
        if($probe === false) return false;

        $v = $this->primaryVideoStream($probe);
        if($v === null) return false;
        $a = $this->primaryAudioStream($probe);

        $plan = array(
            'video' => 'copy', 'audio' => $a ? 'copy' : 'none', 'remux' => false,
            'reasons' => array(), 'vindex' => $v['index'], 'aindex' => $a ? $a['index'] : null,
            'interlaced' => false, 'cap_size' => false, 'fps_out' => null, 'channels' => $a ? (int)($a['channels'] ?? 2) : 0,
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
        if(($w * $h) > self::$maxPixels || $w > self::$maxSide || $h > self::$maxSide)
        {
            $plan['reasons'][] = "size {$w}x{$h}";
            $plan['cap_size'] = true;
        }
        // Only the real (average) rate decides whether a transcode is needed. The nominal rate
        // is still relevant once we transcode for another reason: ffmpeg 4.4 pads VFR input up
        // to r_frame_rate when writing MP4, so pin the output rate to the real one in that case.
        $avgFps = $this->frameRate($v, 'avg_frame_rate');
        $nominalFps = $this->frameRate($v, 'r_frame_rate');
        if($avgFps > self::$maxFps)
            $plan['reasons'][] = "frame rate ".round($avgFps, 2);
        if($avgFps > self::$maxFps || $nominalFps > self::$maxFps)
            $plan['fps_out'] = ($avgFps > 0 && $avgFps <= self::$maxFps) ? round($avgFps, 3) : self::$maxFps;

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
            if(!empty($plan['fps_out']))
                $filters[] = 'fps='.$plan['fps_out'];
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
        $out = $this->tempNameFor($file);
        $cmd = $this->normalizationCommand($file, $plan, $out);
        $output = array();
        $rc = 0;
        exec($cmd.' 2>&1', $output, $rc); // exec(), not system(): nothing may leak into the HTTP response

        if($rc !== 0 || !is_file($out) || filesize($out) === 0 || $this->primaryVideoStream($this->probe($out) ?: array('streams'=>array())) === null)
        {
            error_log("pictshare: ffmpeg failed (rc=$rc) for $file: $cmd :: ".implode(' | ', array_slice($output, -5)));
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

    /**
     * First frame as JPEG. Rendered to a temp name and renamed into place so a concurrent
     * request can never serve (and a CDN never cache) a half-written preview.
     */
    function saveFirstFrameOfMP4($path,$target)
    {
        $bin = escapeshellcmd(FFMPEG_BINARY);
        $tmp = $this->tempNameFor($target);
        $cmd = "$bin -y -nostdin -loglevel error -i ".escapeshellarg($path)." -vframes 1 -f image2 ".escapeshellarg($tmp).' 2>&1';
        $output = array();
        $rc = 0;
        exec($cmd, $output, $rc);
        if($rc !== 0 || !is_file($tmp) || filesize($tmp) === 0 || !rename($tmp, $target))
        {
            error_log("pictshare: preview render failed (rc=$rc) for $path :: ".implode(' | ', array_slice($output, -3)));
            @unlink($tmp);
            return false;
        }
        return true;
    }

    /**
     * Displayed dimensions of the video (rotation from the container applied),
     * so that dimension-encoding hashes describe what the viewer sees.
     */
    function getVideoDimensions($path, $probe = null)
    {
        if($probe === null) $probe = $this->probe($path);
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

}
