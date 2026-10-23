#!/bin/bash
# stack.sh up|down|restart-go  -- the PHP stack on :8443 and the Go stack on :8445, one s3mock.
# Env: DIFF_WORK (scratch dir, required), PHP_IMAGE, GO_IMAGE; for restart-go also GO_DATA=go|php|empty, GO_READ_ONLY=1.
# HASH_DIMS_AES_KEY / CLIENT_ID / COUB_PROXY_URL are read from the repo's .env. S3 is s3mock only.
set -u
: "${DIFF_WORK:?set DIFF_WORK to a scratch directory}"
export REPO=$(cd "$(dirname "$0")/../.." && pwd) DIFF_WORK
for v in HASH_DIMS_AES_KEY CLIENT_ID COUB_PROXY_URL; do export $v="$(grep "^$v=" "$REPO/.env" | cut -d= -f2-)"; done
C="docker compose -p omdiff --project-directory $DIFF_WORK -f $(dirname "$0")/compose.yml"
case $1 in
up)
  docker run --rm -v "$DIFF_WORK":/d alpine sh -c "rm -rf /d/data-php /d/data-go; mkdir -p /d/data-php /d/data-go; chown 100:101 /d/data-go"
  $C up -d 2>&1 | grep -iv "container\|network\|creat\|start"
  for i in $(seq 1 60); do docker run --rm --network omdiff_php curlimages/curl -s -o /dev/null -w '%{http_code}' -X PUT http://s3:9090/media 2>/dev/null | grep -q "200\|409" && break; sleep 1; done
  for i in $(seq 1 120); do docker exec omdiff-pictshare-1 pgrep -x nginx >/dev/null 2>&1 && break; sleep 1; done
  for i in $(seq 1 30); do curl -sk --resolve orbitar.media.local:8445:127.0.0.1 https://orbitar.media.local:8445/healthz | grep -q ok && break; sleep 1; done
  echo up;;
restart-go) $C up -d --force-recreate mediaserver 2>&1 | tail -1; sleep 2;;
down) $C down -v 2>&1 | tail -1;;
esac
