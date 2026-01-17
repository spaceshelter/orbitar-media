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
│  - Hourly job to free disk space                            │
│  - Removes least recently accessed files when low on space  │
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

## Environment Variables

Key variables in `.env` (see `.env.sample`):

| Variable | Purpose |
|----------|---------|
| `SERVER_DOMAIN` | Domain for the service |
| `CLIENT_ID` | Auth header value for POST requests |
| `DATA_DIR` | Host path for persistent data volume |
| `HASH_DIMS_AES_KEY` | AES key for encoding dimensions in hash |
| `S3_BUCKET/REGION/ACCESS_KEY/SECRET_KEY` | S3 backup configuration |
| `REQUIRED_SPACE` | Minimum free GB before cleanup runs (default: 5) |

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
