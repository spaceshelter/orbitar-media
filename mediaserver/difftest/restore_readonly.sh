#!/bin/bash
# restore_readonly.sh <corpus dir>, after diff.py: (a) Go with an empty data dir restores the PHP
# uploads from s3mock byte for byte, (b) Go read-only on the PHP data dir changes nothing there.
: "${DIFF_WORK:?}"; H=$(dirname "$0"); CLIENT_ID="$(grep "^CLIENT_ID=" "$H/../../.env" | cut -d= -f2-)"
cd "$DIFF_WORK"
G="curl -sk --resolve orbitar.media.local:8445:127.0.0.1"
P="curl -sk --resolve orbitar.media.local:8443:127.0.0.1"
echo "s3mock objects: $(docker run --rm --network omdiff_php curlimages/curl -s 'http://s3:9090/media?list-type=2&max-keys=1000' | grep -o '<Key>' | wc -l)"
docker logs omdiff-mediaserver-1 2>&1 | grep -c "s3: upload" | sed 's/^/go s3 upload failures logged: /'

docker run --rm -v "$DIFF_WORK":/d alpine sh -c "rm -rf /d/data-empty; mkdir -p /d/data-empty; chown 100:101 /d/data-empty"
GO_DATA=empty "$H/stack.sh" restart-go
ok=0; bad=0
for h in $(python3 -c "import json;[print(v[0]) for v in json.load(open('work/pairs.json')).values()]"); do
  a=$($P https://orbitar.media.local:8443/raw/$h -o - | shasum | cut -c1-12)
  [ "${h##*.}" = mp4 ] || a=$($P https://orbitar.media.local:8443/$h -o - | shasum | cut -c1-12)
  path=/$h; [ "${h##*.}" = mp4 ] && path=/raw/$h
  b=$($G https://orbitar.media.local:8445$path -o - | shasum | cut -c1-12)
  if [ "$a" = "$b" ]; then ok=$((ok+1)); else bad=$((bad+1)); echo "restore mismatch $h"; fi
done
echo "restored from S3: $ok identical, $bad different"
echo "unknown names: $($G -o /dev/null -w '%{http_code}' https://orbitar.media.local:8445/zzzzzzzz.jpg) $($G -o /dev/null -w '%{http_code}' https://orbitar.media.local:8445/2BLOV4NEYXBvXQRj1i7UYSWaqD9C2Ekaxx.mp4)"

docker run --rm -v "$DIFF_WORK":/d alpine sh -c "cd /d/data-php && find . -exec stat -c '%n %s %Y' {} + | sort" > before.txt
GO_DATA=php GO_READ_ONLY=1 "$H/stack.sh" restart-go
n=0
for h in $(python3 -c "import json;[print(v[0]) for v in json.load(open('work/pairs.json')).values()]"); do
  for s in "" 300/ 77/ left/ preview/ preview/111/ raw/; do $G -o /dev/null https://orbitar.media.local:8445/$s$h; n=$((n+1)); done
done
up=$($G -H "Authorization: Client-ID $CLIENT_ID" -F file=@"$1/i_rgba.png" https://orbitar.media.local:8445/api/upload.php)
docker run --rm -v "$DIFF_WORK":/d alpine sh -c "cd /d/data-php && find . -exec stat -c '%n %s %Y' {} + | sort" > after.txt
echo "read-only: $n requests, upload answered $up, data dir changes: $(diff before.txt after.txt | grep -c '^[<>]')"
GO_DATA=go "$H/stack.sh" restart-go
