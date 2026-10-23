# PHP2Shell response: PHP 8.4 upgrade

Built and tested locally on 2026-10-22.

## The vulnerability

CVE-2026-109618 and CVE-2026-109619 ("PHP2Shell"), critical issues in the PHP core, fixed in 8.5.12, 8.4.27, 8.3.36
and 8.2.35. PHP 8.1 and older will not get a fix, so the only real remedy for this image (PHP 7.4) is a newer PHP.

## What changed

| Area | Change |
|---|---|
| Base image | `alpine:3.24`, `php84` 8.4.27, ffmpeg 8.1.2 (was 4.4). `/usr/bin/php` symlink for the background worker. No `php84-session`, no mcrypt. `expose_php=Off`. |
| `start.sh` | `/etc/php84`, `php-fpm84` |
| AWS SDK | The vendored SDK 3.130.3 / Guzzle 6.5.1 is broken on PHP 8 (`RejectedPromise` throws `TypeError`, so every S3 404, e.g. an unknown hash, became a fatal 500). Replaced by Composer: `aws/aws-sdk-php` 3.399.2 pinned in `composer.lock`, S3 service only (8.5 MB), installed at image build, `vendor/` not committed. `use_aws_shared_config_files => false` (open_basedir warnings). |
| PHP 8 fixes | Undefined index/variable guards in `api/*.php`, `index.php`, `inc/core.php`, `templates/video.html`, `vimeo/vimeo.php`; int casts and a 1 px floor in `resize.php` (GD throws on floats/0 now); image decode only on a cache miss, undecodable image -> 500 `no-store` (was a cached empty 200); BMP no longer accepted (`file` now reports it, and there was never a serve path). `error_reporting` excludes `E_DEPRECATED`, because nginx truncates FastCGI stderr and the flood hid fatals. |
| ffmpeg guards | Every ffmpeg/ffprobe run goes through `VideoController::limited()`: `ulimit -v` (`FFMPEG_MEMORY_LIMIT_MB`, default 2560) and `timeout -s KILL` (60 s probe ... 3600 s background transcode). See CLAUDE.md. |

Removed from the earlier stopgap plan: the Caddy 405 rule and `pm.max_requests = 1` (they halved throughput and are no
longer needed).

## How it was tested

Differential run: the old image (PHP 7.4) and the new one side by side, each with its own s3mock bucket, fed the same
corpus: 96 files sampled from production (covering the URL patterns in the prod access log: plain, `/raw`, `/preview/`,
`/<width>/`, `forcesize`, `webp`, `mp4`, `download`, filters, rotation, info/geturl/base64/pastebin APIs) plus synthetic
edge cases (EXIF orientations 1-8, CMYK, palette/RGBA/animated, 7000x7000, HEVC/VP9 rejects, moov-at-end, AC-3/Opus/MP3,
High 10, 4:4:4, interlaced, VFR, 120 fps, >4K, level 6, chapters, subtitles, rotation). Compared status codes, headers,
decoded image pixels (RMSE), and ffprobe output for videos. Plus an S3 round trip (evict locally, pull back
byte-identical, delete removes the object) and `scripts/smoke-test.sh`.

Results (final image, 2026-10-22):
- Uploads: 147/147 identical decisions (BMP is rejected on both, only the error wording differs). Video
  store/remux/transcode decisions and probes are identical for all 37 MP4s.
- 2747 URL comparisons: everything else is identical apart from the removed `X-Powered-By` header. The exceptions:
  - 36 variants of corrupt production PNGs (CRC errors, truncated): old served 200 with a broken body, which the CDN
    then cached for a year; new returns 500 `no-store`.
  - 10 variants of 24 MP photos (rotate, webp) that hit the memory limit on old now render.
  - WebP of palette/1x1 PNGs and gif `/mp4/` that failed on old now work.
  - Remuxed MP4s differ by a few bytes (newer muxer tag), with identical probes.
- **PHP 8.4's bundled libgd needs a second full-size buffer for PNG decode and WebP encode**, so `PHP_MEMORY_LIMIT` goes
  from 150 to 256 (compose default). At 150, 43 variants of large images that old renders would have failed.
- S3 round trip passes on both. The new SDK turns unknown hashes back into 404 (the vendored one gave 500 on PHP 8).
- `scripts/smoke-test.sh`: 32/32.

Also in this change:
- Stricter authorization checks for delete and for server-side URL fetching (`geturl.php` checks the `Client-ID`
  secret itself; `docker-compose.yml` passes `CLIENT_ID` to the pictshare container for this).
- Image variants are written to a temp file and renamed into place, so a concurrent or failed write can't be served.
- Every ffmpeg/ffprobe run has a memory and wall-clock limit.

## Deploy

1. On prod: `git pull`, `docker compose build pictshare`, `docker compose up -d pictshare` (Caddy and cleanup are unchanged).
2. Check: `docker exec orbitar_media-pictshare-1 php -v` shows 8.4.27; an existing image, an mp4 `/raw` and its `/preview/`
   load; an unknown hash returns 404; a test upload works and lands in S3.
3. Watch `docker logs` (nginx error log + reencode log) for a day, especially `PHP message:` lines and `ffmpeg failed`.
4. Rollback: re-deploy the previous commit (the data directory and S3 layout are unchanged).

Tuning: on a small host, consider `MAX_CONCURRENT_VIDEO_HANDLERS=2` (each transcode is capped by
`FFMPEG_MEMORY_LIMIT_MB`, the sum is bounded by the number of slots).

## Running the smoke test

The local `.env` points at the **production S3 bucket**. Always disable S3 (or use a mock) and use a throwaway data dir.
Orbitar's local Caddy usually holds :443, so run Caddy on 8443 with a copy of the compose file:

```bash
D=$(mktemp -d); sed -e 's/"80:80"/"8080:80"/; s/"443:443"/"8443:443"/' docker-compose.yml > $D/compose.yml
export DATA_DIR=$D/data S3_BUCKET= S3_ACCESS_KEY= S3_SECRET_KEY= S3_REGION= MAX_UPLOAD_SIZE=50; mkdir -p $DATA_DIR
docker compose -p omsmoke --project-directory . -f $D/compose.yml up -d --build
# wait for start.sh to get to nginx (GET / is Caddy's static index.html and says nothing about PHP)
until docker exec omsmoke-pictshare-1 pgrep -x nginx >/dev/null; do sleep 1; done; sleep 2
CLIENT_ID=$(grep '^CLIENT_ID=' .env | cut -d= -f2-) BASE=https://orbitar.media.local:8443 \
  CONTAINER=omsmoke-pictshare-1 scripts/smoke-test.sh
docker compose -p omsmoke --project-directory . -f $D/compose.yml down -v
```

Sources: [Wiz](https://wiz.io/blog/php2shell-php-rce),
[Google Cloud](https://cloud.google.com/blog/products/identity-security/responding-to-core-php-vulnerabilities-cve-2026-109618-cve-2026-109619),
[Tenable](https://www.tenable.com/blog/php2shell-cve-2026-109618-cve-2026-109619-frequently-asked-questions-about-critical-php-remote),
[VirusTotal](https://blog.virustotal.com/2026/10/sizing-php-exposure-window-for-cve-2026.html).
