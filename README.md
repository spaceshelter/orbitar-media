# orbitar-media


Self hosted media server for [Orbitar](https://github.com/spaceshelter/orbitar/), 
based on the (patched) [Pictshare](https://github.com/HaschekSolutions/pictshare) at 
[9a4a20f](https://github.com/HaschekSolutions/pictshare/commit/9a4a20fb413c4110dd05164d312539112fd8ebaf).

---

## Run locally

1. Add local domain to `/etc/hosts`, e.g. `127.0.0.1 orbitar.media.local` 
2. Copy `.env.sample` to `.env` and adjust values
3. Run `docker compose up -d`
