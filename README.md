# orbitar-media


Self hosted media server for [Orbitar](https://github.com/spaceshelter/orbitar/): Caddy in front of
`mediaserver/`, a Go service (README there) that replaced a patched
[PictShare](https://github.com/HaschekSolutions/pictshare) and keeps its URLs, storage layout and API.
A cleanup service evicts the oldest files when the disk runs low; with S3 configured, they are restored on demand.

---

## Run locally

1. Add local domain to `/etc/hosts`, e.g. `127.0.0.1 orbitar.media.local` 
2. Copy `.env.sample` to `.env` and adjust values
3. Run `docker compose up -d`
4. Optionally `CLIENT_ID=... scripts/smoke-test.sh` (only against a stack without S3: it uploads test files)

## Next to Orbitar on one box

Orbitar's Caddy already owns port 443, so the media stack runs behind it
(`docker-compose.colocated.yml`, needs docker compose 2.24.4+):

```bash
docker compose -f docker-compose.yml -f docker-compose.colocated.yml up -d
```

Its Caddy then listens for plain HTTP on the Docker bridge only, `172.17.0.1:8090` (override with
`MEDIA_HTTP_BIND`). Orbitar derives `b.<domain>` and `origin.<domain>` from `MEDIA_HOSTING_URL`, so point all three
names at the box and add a site to Orbitar's Caddy, which gets the certificates. Orbitar imports extra sites from
`caddy/sites/*.caddy` (its `caddy/sites/README.md`; the deploy copies them from
`/opt/deployment-specific-files/caddy-sites/` on the host):

```
media.example.org, b.media.example.org, origin.media.example.org {
    reverse_proxy 172.17.0.1:8090
}
```

Then set Orbitar's `MEDIA_HOSTING_URL=https://media.example.org` and `MEDIA_HOSTING_CLIENT_ID` to this stack's
`CLIENT_ID`. S3 is optional (empty `S3_BUCKET` = local disk only; the cleanup service then deletes files for good when
the disk runs low, see `REQUIRED_SPACE`).

Staging (`stage.orbitar.media` on Orbitar's staging box) is deployed this way by `.github/workflows/deploy-stage.yml`,
with its `.env` in `/opt/deployment-specific-files-media/` and its own bucket. The runner can't create top-level
directories, so `/orbitar_media` (owned by the runner user) and `DATA_DIR` are created once by root.
