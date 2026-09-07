# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Self-hosted media server for [Orbitar](https://github.com/spaceshelter/orbitar/), based on a patched version of [PictShare](https://github.com/HaschekSolutions/pictshare). Handles upload, storage, and serving of images, videos (MP4), and text files with deduplication, EXIF stripping, and optional S3 backup.

**Stack:** PHP 7, Nginx, Alpine Linux, Docker Compose, Caddy (reverse proxy)

## Common Commands

```bash
# Local development
cp .env.sample .env          # Configure environment (edit values)
docker compose up -d         # Start all services
docker compose build         # Rebuild images after changes
docker compose logs -f       # View logs

# Validate configuration
docker compose config        # Check docker-compose.yml syntax
```

Add `127.0.0.1 orbitar.media.local` to `/etc/hosts` for local testing.

## Local Development Gotchas

### Domain & SSL
- **Use `orbitar.media.local`**, not `localhost` - Caddy is configured for this domain
- Caddy auto-generates SSL certificates and redirects HTTP → HTTPS
- Use `curl -k` to skip SSL verification for local testing

### Authorization Header
The Caddyfile expects `Authorization: Client-ID <value>`, NOT `Client-ID: <value>`:
```bash
# Correct
curl -k -H "Authorization: Client-ID $CLIENT_ID" ...

# Wrong (will get 403)
curl -k -H "Client-ID: $CLIENT_ID" ...
```

### Container Access
- Cannot connect directly to pictshare container IP from host (Docker network isolation)
- Always go through Caddy at `https://orbitar.media.local`
- For debugging inside container: `docker exec orbitar-media-pictshare-1 php -r '...'`

### Testing Image Uploads
```bash
# Upload a file
curl -k -H "Authorization: Client-ID $CLIENT_ID" \
  -F "file=@test.jpg" \
  https://orbitar.media.local/api/upload.php

# View uploaded image
curl -k https://orbitar.media.local/<hash>.jpg -o result.jpg
```

### Creating EXIF Test Images
For testing EXIF orientation, create images with **actually rotated pixels** + matching EXIF tag:
```bash
# Orient 6 test (90° CW): pixels must be rotated CCW, EXIF tells viewer to rotate CW
magick original.jpg -rotate -90 temp.jpg
exiftool -Orientation=6 -n -overwrite_original temp.jpg

# Orient 3 test (180°): pixels rotated 180°, EXIF tells viewer to rotate 180°
magick original.jpg -rotate 180 temp.jpg
exiftool -Orientation=3 -n -overwrite_original temp.jpg
```
**Note:** Test images with normal pixels + fake EXIF tags won't test the real-world scenario correctly.

## Architecture

Three Docker containers orchestrated via docker-compose.yml:

```
┌─────────────────────────────────────────────────────────────┐
│  Caddy (reverse proxy)                                      │
│  - Routes traffic to pictshare                              │
│  - Enforces Client-ID header on POST /api/*                 │
│  - Serves /static/ files                                    │
└─────────────────────────┬───────────────────────────────────┘
                          │
┌─────────────────────────▼───────────────────────────────────┐
│  PictShare (main PHP app)                                   │
│  - Entry: index.php → inc/core.php                          │
│  - Content controllers: pictshare/content-controllers/      │
│  - Storage controllers: pictshare/storage-controllers/      │
│  - API endpoints: pictshare/api/                            │
└─────────────────────────┬───────────────────────────────────┘
                          │
┌─────────────────────────▼───────────────────────────────────┐
│  Cleanup Service (cron microservice)                        │
│  - Runs every 15 minutes to free disk space                 │
│  - Evicts oldest files by mtime (FIFO) when low on space    │
└─────────────────────────────────────────────────────────────┘
```

### Key Code Paths

- **Entry point:** `pictshare/index.php` → `inc/core.php` (`architect()` function)
- **API endpoints:** `pictshare/api/` (upload.php, geturl.php, info.php, pastebin.php, base64.php)
- **Content handlers:** `pictshare/content-controllers/` (image/, video/, text/, url/)
- **Storage backends:** `pictshare/storage-controllers/` (s3/, ftp.controller.php)
- **Docker entrypoint:** `pictshare/docker/rootfs/start.sh` (generates config, adjusts PHP settings)
- **Cleanup logic:** `cleanup-service/cleanup.sh`

### File Processing Flow

1. Upload received at `/api/upload.php`
2. File validated and SHA1 hash computed for deduplication
3. Routed to appropriate content controller (image/video/text)
4. Stored locally in `data/` directory, optionally backed up to S3
5. Returns JSON with hash URL and delete code

## Video Pipeline (MP4)

`pictshare/content-controllers/video/video.controller.php` decides per upload what browsers can play,
using `ffprobe` (`planNormalization`). Target: H.264 up to High profile / level 5.2, 8-bit 4:2:0,
progressive, even dimensions, AAC or MP3 audio, one video + at most one audio stream, `moov` before `mdat`.

- Compliant uploads are stored byte-for-byte (no `encoder`-tag heuristics any more).
- Container-only or audio-only problems (moov at the end, extra subtitle/data streams, AC-3/Opus audio)
  are fixed **synchronously during the upload** with `-c:v copy`, so the bytes behind a URL never change
  after the URL has been handed out.
- Video transcodes (non-4:2:0, High 10 / 4:4:4, level > 5.2, > 4K or > 60 fps, odd dimensions, interlaced -> `yadif`)
  still run in the background via `tools/re-encode_mp4.php <hash>` (capped by `MAX_CONCURRENT_VIDEO_HANDLERS`).
  Transcodes emit High profile, CRF 23, and shrink the output to fit 3840x2160 / 60 fps (`fps=60`,
  `scale=w='min(iw,3840)':h='min(ih,2160)':force_original_aspect_ratio=decrease`, never upscaling) so the result stays
  within level 5.2; the trigger is pixel budget > 3840*2160, either side > 4096, *average* fps > 60, or a source level > 5.2 (the nominal
  `r_frame_rate` never triggers, VFR sources inflate it, but it does pin `fps=` to the real rate during a transcode because
  ffmpeg 4.4 pads VFR up to the nominal rate); the ffmpeg 4.4 `fps` filter does not take expressions, so the caps are fixed values chosen
  from the probe. `-map_chapters -1` keeps chapters from re-materialising as a data stream (which would fail the
  stream-count check on every later scan). The output replaces the file atomically (`rename` inside `data/<hash>/`, per-process
  temp name `<target>.<pid>.<rand>.tmp` so concurrent generators never share one), and the synchronous path registers the
  new file's sha1 so re-uploads dedupe.
- `tools/re-encode_mp4.php <hash>...` normalises given hashes; the full scan needs an explicit `all` (plus `dryrun` to only
  report) because every rewrite changes the bytes and the ETag while Bunny may still hold the old chunks. Unresolvable hash
  arguments make it refuse rather than fall back to a scan; `altfolder` has the same gate. It refreshes the S3 copy after a rewrite.
- Concurrency: `acquireConversionSlot()` (flock on `tmp/video-slot-N.lock`, N = `MAX_CONCURRENT_VIDEO_HANDLERS`) bounds
  ffmpeg runs across the upload-time remux, the background worker, `/<width>/` resizes and gif->mp4. Capacity is reserved
  *before* `storeFile()`: a busy upload is rejected with "System is busy" while nothing has been published yet, so there
  is no rollback and no window in which a concurrent identical upload could dedupe onto a file that then disappears.
  Admission for background transcodes probes the same slots (no more `ps aux | grep`). Lock files are chmod 0666; when
  another user (root via `docker exec`) owns one it is opened read-only and flocked, never unlinked and recreated (that
  would be a second inode, i.e. a duplicate slot). `addSha1` appends under flock.
- Every generated file (normalised video, resize, first-frame preview, gif->mp4) is written to a temp name and `rename`d
  into place; generation failures return 503 `no-store` instead of leaving an empty file that `file_exists()` would serve
  forever. ffmpeg is run through `exec()`, never `system()`, so nothing can leak into the HTTP body.
- Interlace detection decodes the first 5 frames (`ffprobe -read_intervals %+#5`) because ffprobe 4.4 reports
  `field_order=unknown` for most H.264 streams.
- `getVideoDimensions` returns *displayed* dimensions (rotation from the display matrix applied), which is what
  the dimension-encoding hash should describe.
- `isProperMP4` (upload type detection) still requires an MP4/MOV container with an H.264 stream; HEVC/AV1/WebM
  uploads are rejected as before. Widening this needs a "processing" state or a CDN purge, see below.

### Serving and caching
- `serveMP4` opens the file first and derives size/ETag/Last-Modified from `fstat()` of that handle, so an atomic
  replacement cannot split one response across two versions.
- `/raw`, previews and images send a strong `ETag` (inode-mtime-size, `statETag()` in `inc/core.php`) and
  `Last-Modified`, answer `If-None-Match` / `If-Modified-Since` with 304, and honour `If-Range` (stale validator
  => whole file, 200). Range parsing lives in `parseByteRange()`; suffix and open-ended ranges work, multi-range
  is ignored (200), unsatisfiable ranges return 416 with `Content-Range: bytes */<size>`. Error responses (404/416/500/503)
  carry `Cache-Control: no-store`.
- The old code used the immutable hash as ETag and rewrote videos in place ~1-2 minutes after upload. Bunny CDN
  (`b.orbitar.media`) caches 5 MB chunks per URL and never revalidates them, so viewers got chunks of the original
  spliced with chunks of the re-encode: frozen players and 416s (incident of 2026-09-07). The synchronous remux
  removes that window for stream-copy cases; genuine background transcodes still need a Bunny purge of
  `/<hash>.mp4/raw` when they finish (not implemented, needs the Bunny API key).

## Environment Variables

Key variables in `.env` (see `.env.sample`):

| Variable | Purpose |
|----------|---------|
| `SERVER_DOMAIN` | Domain for the service |
| `CLIENT_ID` | Auth header value for POST requests |
| `DATA_DIR` | Host path for persistent data volume |
| `HASH_DIMS_AES_KEY` | AES key for encoding dimensions in hash |
| `S3_BUCKET/REGION/ACCESS_KEY/SECRET_KEY` | S3 backup configuration |
| `REQUIRED_SPACE` | Minimum free GB before cleanup runs (default: 10) |

## CI/CD

- **build.yml:** Validates docker-compose.yml and builds images on push/PR to main
- **deploy-prod.yml:** Production deployment workflow

## API Quick Reference

All POST requests to `/api/*` require `Authorization: Client-ID <value>` header (enforced by Caddy).

```bash
# Upload file
curl -k -H "Authorization: Client-ID $CLIENT_ID" -F "file=@image.jpg" https://orbitar.media.local/api/upload.php

# Upload from URL (GET, no auth required)
curl -k "https://orbitar.media.local/api/geturl.php?url=https://example.com/image.jpg"

# Get file info (GET, no auth required)
curl -k "https://orbitar.media.local/api/info.php?hash=abc123.jpg"

# Create text paste
curl -k -H "Authorization: Client-ID $CLIENT_ID" -F "api_paste_code=Hello" https://orbitar.media.local/api/pastebin.php
```

See `pictshare/rtfm/` for comprehensive documentation (API.md, CONFIG.md, etc.).
