# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Self-hosted media server for [Orbitar](https://github.com/spaceshelter/orbitar/): `mediaserver/`, a Go service that
replaced a patched [PictShare](https://github.com/HaschekSolutions/pictshare) (PHP, removed from the repo) and keeps
its URLs, data layout, S3 keys and upload API. Handles upload, storage, and serving of images, videos (MP4), and text
files with deduplication, EXIF stripping, and optional S3 backup.

**Stack:** Go 1.26, libvips CLI, ffmpeg (Alpine 3.24), Caddy (reverse proxy), Docker Compose.

## Common Commands

```bash
# Local development
cp .env.sample .env          # Configure environment (edit values)
docker compose up -d         # Start all services
docker compose build         # Rebuild images after changes
docker compose logs -f       # View logs

# Go tests (local Go is too old; the test stage has vips/ffmpeg/file)
docker build -f mediaserver/Dockerfile --target test .

# End-to-end smoke test (only against a stack WITHOUT S3: it uploads test files)
CLIENT_ID=... BASE=https://orbitar.media.local scripts/smoke-test.sh

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
- Cannot connect directly to the mediaserver container IP from host (Docker network isolation)
- Always go through Caddy at `https://orbitar.media.local`
- The local `.env` may point at the production bucket: blank the `S3_*` variables on the command line for local tests
- The mediaserver image has a shell, `vips`, `ffmpeg`/`ffprobe`: `docker exec orbitar-media-mediaserver-1 ...`

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

Docker containers orchestrated via docker-compose.yml:

```
┌─────────────────────────────────────────────────────────────┐
│  Caddy (reverse proxy)                                      │
│  - Routes traffic to mediaserver                            │
│  - Enforces Client-ID header on every /api/* request        │
│  - Serves /static/ files                                    │
└─────────────────────────┬───────────────────────────────────┘
                          │
┌─────────────────────────▼───────────────────────────────────┐
│  mediaserver (Go)                                           │
│  - Entry: mediaserver/cmd/mediaserver → internal/server     │
│  - URL dispatch: internal/server/architect.go               │
│  - Upload API: internal/server/api.go                       │
└─────────────────────────┬───────────────────────────────────┘
                          │
┌─────────────────────────▼───────────────────────────────────┐
│  Cleanup Service (cron microservice)                        │
│  - Runs every 5 minutes to free disk space                  │
│  - Evicts oldest files by mtime (FIFO) when low on space    │
└─────────────────────────────────────────────────────────────┘
```

### Key Code Paths (`mediaserver/`, table in its README)

- **Entry point:** `cmd/mediaserver/main.go` (env config) → `internal/server` (`architect()` dispatches URLs)
- **API endpoints:** `internal/server/api.go` (upload.php, geturl.php, info.php)
- **Images:** `internal/imaging` (vips), **video:** `internal/video` (ffprobe/ffmpeg), served by `internal/server/{image,video}.go`
- **Storage:** `internal/store` (on-disk layout), `internal/s3store`, `internal/server/queue.go` (S3 upload queue)
- **Text page assets:** `mediaserver/static/` (css/js/fonts)
- **Cleanup logic:** `cleanup-service/cleanup.sh`

### File Processing Flow

1. Upload received at `/api/upload.php`
2. File validated and SHA1 hash computed for deduplication
3. Handled by type (image/video/text)
4. Stored locally in `data/<hash>/`, queued for the S3 copy
5. Returns JSON with hash URL and delete code

## Video Pipeline (MP4)

`mediaserver/internal/video` (ported from PictShare's `video.controller.php`) decides per upload what browsers can
play, using `ffprobe`. Target: H.264 up to High profile / level 5.2, 8-bit 4:2:0, progressive, even dimensions, AAC or
MP3 audio, one video + at most one audio stream, `moov` before `mdat`.

- Compliant uploads are stored byte-for-byte.
- Container-only or audio-only problems (moov at the end, extra subtitle/data streams, AC-3/Opus audio)
  are fixed **synchronously during the upload** with `-c:v copy`, so the bytes behind a URL never change
  after the URL has been handed out.
- Video transcodes (non-4:2:0, High 10 / 4:4:4, level > 5.2, > 4K or > 60 fps, odd dimensions, interlaced -> `yadif`)
  still run in the background (`transcodeInBackground` in `internal/server/api.go`: waits at most 10 minutes for a
  slot, then keeps the original). Transcodes emit High profile, CRF 23, and shrink the output to fit 3840x2160 / 60 fps
  (never upscaling) so the result stays within level 5.2; the trigger is pixel budget > 3840*2160, either side > 4096,
  *average* fps > 60, or a source level > 5.2 (the nominal `r_frame_rate` never triggers, VFR sources inflate it, but it
  does pin `fps=` to the real rate during a transcode). `-map_chapters -1` keeps chapters from re-materialising as a
  data stream (which would fail the stream-count check on every later scan). The output replaces the file atomically
  and registers the new file's sha1 so re-uploads dedupe, then refreshes the S3 copy.
- Concurrency: in-process semaphores, `MAX_CONCURRENT_VIDEO_HANDLERS` (ffmpeg: upload remux, transcodes, gif->mp4) and
  `MAX_CONCURRENT_IMAGE_HANDLERS` (vips, previews). Capacity is reserved *before* the file is stored: a busy upload is
  rejected with "System is busy" while nothing has been published yet.
- `/<width>/` variants of the video itself return 404 (unauthenticated full transcode, nothing links to them), but
  sized previews `/preview/<w>/<hash>.mp4` and `/preview/<w>x<h>/...` are served as a scaled first frame rendered from
  the original, cached as `<w>_<hash>_preview.jpg` (the name PictShare's old resize path produced).
- The full-size preview (`/preview/<hash>.mp4`, the only one Orbitar uses) is also copied to S3 as
  `previews/<hash>_preview.jpg`; for an evicted video it is served from there without restoring the video
  (mediaserver/README.md, "Video previews in S3").
- Every generated file (normalised video, preview, gif->mp4, image variants) is written to a temp name and `rename`d
  into place; generation failures return 503 `no-store` instead of leaving an empty file.
- Interlace detection decodes the first 5 frames (`ffprobe -read_intervals %+#5`) because ffprobe reports
  `field_order=unknown` for most H.264 streams.
- Every ffmpeg/ffprobe/vips run goes through `internal/proc`: `ulimit -v` (`FFMPEG_MEMORY_LIMIT_MB`, default 2560, `VIPS_MEMORY_LIMIT_MB` for vips; a 4K
  x264 transcode peaks at ~1.9 GB virtual / 1.5 GB resident on 2 cores) plus a kill timeout (probe 60 s, preview
  120 s, upload remux 300 s, gif->mp4 600 s, background transcode 3600 s).
- Video dimensions are *displayed* dimensions (rotation from the display matrix applied), which is what the
  dimension-encoding hash should describe.
- Upload type detection still requires an MP4/MOV container with an H.264 stream; HEVC/AV1/WebM uploads are
  rejected. Widening this needs a "processing" state or a CDN purge, see below.

### Serving and caching
- Files are opened first and size/ETag/Last-Modified come from `Stat()` of that handle, so an atomic
  replacement cannot split one response across two versions.
- `/raw`, previews and images send a strong `ETag` (inode-mtime-size, `statETag()` in `internal/server/serve.go`) and
  `Last-Modified`; Range, If-Range, If-None-Match and If-Modified-Since are Go's `http.ServeContent`. Error responses
  carry `Cache-Control: no-store`.
- The original PictShare code used the immutable hash as ETag and rewrote videos in place ~1-2 minutes after upload. Bunny CDN
  (`b.orbitar.media`) caches 5 MB chunks per URL and never revalidates them, so viewers got chunks of the original
  spliced with chunks of the re-encode: frozen players and 416s (incident of 2026-09-07). The synchronous remux
  removes that window for stream-copy cases; genuine background transcodes still need a Bunny purge of
  `/<hash>.mp4/raw` when they finish (not implemented, needs the Bunny API key).

## Environment Variables

Key variables in `.env` (see `.env.sample`):

| Variable | Purpose |
|----------|---------|
| `SERVER_DOMAIN` | The one hostname Caddy serves, HTTPS and plain HTTP (prod: `origin.orbitar.media`, which Bunny pulls from) |
| `CLIENT_ID` | Auth header value for POST requests |
| `DATA_DIR` | Host path for persistent data volume |
| `HASH_DIMS_AES_KEY` | AES key for encoding dimensions in hash |
| `S3_BUCKET/REGION/ACCESS_KEY/SECRET_KEY` | S3 backup configuration |
| `REQUIRED_SPACE` | Minimum free GB before cleanup runs (default: 10) |
| `MAX_CONCURRENT_VIDEO_HANDLERS` | Parallel ffmpeg runs (default 2) |
| `FFMPEG_MEMORY_LIMIT_MB` | `ulimit -v` per ffmpeg/ffprobe run (default 2560) |
| `MASTER_DELETE_CODE` / `MASTER_DELETE_IP` | Optional admin delete; empty means disabled |

`MEDIASERVER_*` overrides (`READ_ONLY`, `TOUCH_ON_ACCESS_MINUTES`, `IMAGE_HANDLERS`) go on the command line, never into
the prod `.env`: the deploys check that its keys match `.env.sample`. Full list in mediaserver/README.md.

## CI/CD

- **build.yml:** On push to main and every PR: validates compose and Caddy configs and builds the images
  (`build-docker`), and runs the Go tests (`mediaserver-test`, the Dockerfile's `test` stage)
- **deploy-prod.yml:** Production deployment workflow (environment `Media`). Deploys run
  `docker compose up -d --build --force-recreate --remove-orphans`.
- **deploy-stage.yml:** Staging (`stage.orbitar.media`) on Orbitar's staging box, behind Orbitar's Caddy
  (`docker-compose.colocated.yml`, README "Next to Orbitar on one box"). Runner label `stage`, environment
  `MediaStaging`, `.env` from `/opt/deployment-specific-files-media/` (bucket `orbitar-media-stage`, never the prod one).
- Caddy routing lives in `caddy/media.caddy`, imported by `Caddyfile` (prod, own TLS) and `Caddyfile.colocated`.

## API Quick Reference

Every request to `/api/*`, any method, requires `Authorization: Client-ID <value>` (enforced by Caddy, and again by mediaserver).

```bash
# Upload file
curl -k -H "Authorization: Client-ID $CLIENT_ID" -F "file=@image.jpg" https://orbitar.media.local/api/upload.php

# Upload from URL (server-side fetch from public addresses only)
curl -k -H "Authorization: Client-ID $CLIENT_ID" "https://orbitar.media.local/api/geturl.php?url=https://example.com/image.jpg"

# Get file info (also needs Client-ID)
curl -k -H "Authorization: Client-ID $CLIENT_ID" "https://orbitar.media.local/api/info.php?hash=abc123.jpg"
```

`/api/base64.php` and `/api/pastebin.php` are gone; URL grammar and differences from PictShare are in mediaserver/README.md.
