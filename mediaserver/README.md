# mediaserver

The media server behind Orbitar. It replaced the PictShare (PHP) service it was ported from and kept its URLs, data
directory layout, S3 keys and upload API, so files stored by PictShare are served as they were.

One static binary, with `vips` (libvips CLI), `ffmpeg`/`ffprobe` and `file` from Alpine. Each external tool runs with a
timeout and `ulimit -v`, so a bad input can only fail its own request.

## Layout

| Package | What |
|---|---|
| `cmd/mediaserver` | env config, startup, graceful shutdown |
| `internal/hashcodec` | dimension-encoding hashes (AES-256-CBC, base62), golden tests generated with the PHP code |
| `internal/store` | `data/<hash>/<hash>`, variants, `deletecode`, `sha1.csv`; atomic writes; read-only overlay |
| `internal/imaging` | type sniffing, JPEG normalisation at upload (EXIF orientation applied, metadata stripped), resize / rotate / forcesize / webp variants |
| `internal/video` | ffprobe-based normalisation plan (ported from `video.controller.php`, checked against it on the corpus), remux / transcode, preview frames, gif to mp4 |
| `internal/s3store` | S3 get / put / delete (aws-sdk-go-v2) |
| `internal/server` | HTTP: URL grammar (`architect`), upload / geturl / info API, coub / vimeo, serving with ETag / Range / 304, S3 restore and upload queue |

## Behaviour

- **URLs**: everything PictShare served in production: `/<hash>`, `/<size>/`, `/w/<n>/`, `NxM`, `left` / `right` / `upside`,
  `forcesize`, `webp`, `/raw/`, `/preview/[<w>/]`, `/download/`, `/delete_<code>/`, gif `/mp4/`, `/coub/<id>`, `/vimeo/<id>`,
  segments in any order. Variant files keep PictShare's names (`md5(http_build_query(...))_<hash>`), so variants
  PictShare generated are still reused.
- **Dropped**: the colour filters (`sepia`, `vintage`, ...: the segment is ignored and the picture is served without it),
  `/api/base64.php`, `/api/pastebin.php`. Query strings are ignored.
- **API**: same JSON fields and reason strings as PictShare, except an oversized upload gets
  `{"status":"err","reason":"File too big. 50mb max"}` instead of an nginx 413 page. Every `/api/*` request needs
  `Authorization: Client-ID <CLIENT_ID>`; Caddy checks it, and so does the service. `geturl` fetches only from public
  addresses, with a size cap, a timeout and a redirect limit.
- **Images**: JPEG uploads are re-encoded with the orientation applied (all 8 EXIF orientations) and metadata
  stripped. Untagged CMYK JPEGs are converted with the plain formula, as browsers and GD do. PNG / GIF / WebP are
  stored as uploaded. Resampling is libvips instead of GD: same sizes, pixels differ slightly.
- **Video**: as documented in `../CLAUDE.md` ("Video Pipeline"). Container and audio fixes happen synchronously,
  before the URL is returned, and transcodes run in the background. Concurrency is bounded by in-process semaphores
  (`MAX_CONCURRENT_VIDEO_HANDLERS`) instead of lock files.
- **Caching**: strong ETag (inode-mtime-size) and Last-Modified on files, `Cache-Control: public, max-age=31536000`
  on success, `no-store` on every error. Range, If-Range, If-None-Match and If-Modified-Since come from Go's `http.ServeContent`.
- **S3**: on a local miss, the original is restored from the bucket. Dimension hashes must decrypt with the key,
  other names are rate limited, misses are cached, and concurrent downloads are capped.
  Uploads go to S3 from a background queue: a marker per pending file in `data/.s3-pending/` (which the
  cleanup service's `ls` doesn't see), retries with backoff, and a sweep at startup. The marker is a hard link to the
  file, so a copy still pending when the cleanup service evicts the directory is not lost. An S3 outage delays the
  copy, not the upload.
- **Video previews in S3**: the full-size preview frame (`/preview/<hash>.mp4`, the only one Orbitar uses) is copied
  to `previews/<hash>_preview.jpg` in the bucket when rendered (best effort). Its URL, for a video that isn't stored
  locally, is answered from that copy (placed in `data/<hash>/` without the video), so a page of old videos doesn't
  restore every video to render a frame. Without a copy, the video is restored and the frame rendered and uploaded
  as before. Sized previews are only rendered. Deleting a video removes its preview, and a custom name reused after its
  file was evicted drops the earlier file's.
- **Read-only mode** (`READ_ONLY=1`): never writes to the data directory. Uploads and deletes return 503. Files
  restored from S3 and generated variants go to a scratch overlay, which is cleared at startup. Lets a second
  instance serve a copy of production's volume and bucket without changing either.

## Configuration

| Variable | Default | |
|---|---|---|
| `HASH_DIMS_AES_KEY` | | key of the dimension-encoding names; empty = random names |
| `CLIENT_ID` | | required for `/api/*` |
| `MAX_UPLOAD_SIZE` | 50 | MB |
| `CONTENTCONTROLLERS` | all | `IMAGE,VIDEO,TEXT` subset accepted by `upload.php` |
| `S3_BUCKET`, `S3_REGION`, `S3_ACCESS_KEY`, `S3_SECRET_KEY`, `S3_ENDPOINT` | | empty bucket = no S3 |
| `COUB_PROXY_URL` | | proxy for coub.com API requests (IP blocks) |
| `MASTER_DELETE_CODE`, `MASTER_DELETE_IP` | | empty = disabled |
| `MAX_CONCURRENT_VIDEO_HANDLERS` | 2 | ffmpeg runs (remux, transcode, previews of gif->mp4) |
| `MAX_CONCURRENT_IMAGE_HANDLERS` | 2 | vips / preview renders |
| `FFMPEG_MEMORY_LIMIT_MB` | 2560 | `ulimit -v` per ffmpeg / ffprobe |
| `VIPS_MEMORY_LIMIT_MB` | 1024 | `ulimit -v` per vips run |
| `MAX_IMAGE_MEGAPIXELS` | 200 | larger images are refused |
| `READ_ONLY` | | `1` = read-only mode (above); `OVERLAY_DIR` for its scratch space |
| `TOUCH_ON_ACCESS_MINUTES` | 0 | > 0: bump a hash directory's mtime when served (at most this often), so the cleanup service evicts by last use instead of upload time |
| `DATA_DIR`, `TMP_DIR`, `STATIC_DIR`, `LISTEN` | `/var/www/data`, `/tmp/mediaserver`, `/srv/static`, `:80` | |
| `URL`, `TITLE` | | prefix of returned URLs (empty in production), page title |

The container runs as uid 100 / gid 101, the `nginx` user of the old PictShare image, which owns the files on the volume.

## Build and test

```bash
# from the repository root
docker build -f mediaserver/Dockerfile --target test .   # go vet + unit and HTTP tests with real vips/ffmpeg
docker build -f mediaserver/Dockerfile -t orbitar-mediaserver .
docker compose up -d                                     # the whole stack
CLIENT_ID=... scripts/smoke-test.sh                      # end to end, against a stack without S3 (BASE=...)
```
