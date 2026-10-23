#!/bin/bash
# End-to-end smoke test against a running stack (see PHP_UPGRADE_PLAN.md, "Running the smoke test").
# Env: CLIENT_ID (required), BASE (default https://orbitar.media.local), CONTAINER (default orbitar-media-pictshare-1).
# Needs magick, exiftool, ffmpeg/ffprobe, python3 on the host. Never point it at a stack with S3 enabled:
# every upload would be copied to the bucket.
set -u
B=${BASE:-https://orbitar.media.local}
CT=${CONTAINER:-orbitar-media-pictshare-1}
M=$(mktemp -d); T=$(mktemp -d); trap 'rm -rf "$M" "$T"' EXIT

# test media; a different seed per run so nothing dedupes onto files from an earlier run
SEED=$RANDOM
magick -size 1200x800 gradient:red-blue -fill white -pointsize 120 -gravity north -annotate +0+40 "TOP$SEED" $M/photo.jpg
magick $M/photo.jpg -rotate -90 $M/orient6.jpg && exiftool -q -Orientation=6 -n -overwrite_original $M/orient6.jpg
magick -size 640x480 -seed $SEED plasma: $M/logo.png
magick -size 9000x9000 -seed $SEED plasma: -quality 92 $M/huge.jpg
mp4() { ffmpeg -loglevel error -y -f lavfi -i testsrc2=size=640x360:rate=30 -f lavfi -i sine=f=$((200 + SEED % 500)) -t 3 -c:v libx264 "$@"; }
mp4 -profile:v high -pix_fmt yuv420p -c:a aac -movflags +faststart $M/ok.mp4
mp4 -profile:v high -pix_fmt yuv420p -c:a aac -metadata comment=moov$SEED $M/moov_end.mp4
mp4 -pix_fmt yuv444p -c:a aac -movflags +faststart $M/yuv444.mp4

C="curl -sk --max-time 120"
PASS=0; FAIL=0
check() { # name expected actual
  if [ "$2" = "$3" ]; then PASS=$((PASS+1)); echo "PASS  $1 ($3)"; else FAIL=$((FAIL+1)); echo "FAIL  $1: expected '$2', got '$3'"; fi
}
code() { $C -o /dev/null -w '%{http_code}' "$@"; }
upload() { $C -H "Authorization: Client-ID $CLIENT_ID" -F "file=@$1" $B/api/upload.php; }
field() { python3 -c 'import sys,json
try: print(json.load(sys.stdin).get(sys.argv[1],""))
except ValueError: print("")' "$1"; }
echo "== runtime"
check "PHP >= 8.4.27 (PHP2Shell fix)" yes "$(docker exec $CT php -r 'echo version_compare(PHP_VERSION, "8.4.27", ">=") ? "yes" : "no";')"
check "no session extension" 0 "$(docker exec $CT php -m | grep -c '^session$')"
check "upload without Client-ID"        403 "$(code -F file=@$M/photo.jpg $B/api/upload.php)"
check "GET / still served"              200 "$(code $B/)"

echo "== images"
J=$(upload $M/photo.jpg); H_JPG=$(echo "$J" | field hash)
check "upload jpg status" ok "$(echo "$J" | field status)"
check "GET jpg" 200 "$(code $B/$H_JPG)"
check "dedupe re-upload same hash" "$H_JPG" "$(upload $M/photo.jpg | field hash)"
code $B/delete_/$H_JPG >/dev/null; code $B/delete_wrong/$H_JPG >/dev/null
check "file survives /delete_/ and /delete_wrong/" 200 "$(code $B/$H_JPG)"
check "geturl without Client-ID (Caddy)" 403 "$(code "$B/api/geturl.php?url=http://127.0.0.1/$H_JPG")"
check "geturl without Client-ID (PHP, past Caddy)" 403 "$(docker exec $CT curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1/api/geturl.php?url=http://127.0.0.1/$H_JPG")"
check "geturl with Client-ID" ok "$($C -H "Authorization: Client-ID $CLIENT_ID" "$B/api/geturl.php?url=http://127.0.0.1/$H_JPG" | field status)"
$C $B/300/$H_JPG -o $T/r.jpg 2>/dev/null
check "resized /300/ width" 300 "$(magick identify -format %w $T/r.jpg 2>/dev/null)"
J=$(upload $M/orient6.jpg); H_O=$(echo "$J" | field hash)
$C $B/$H_O -o $T/o.jpg
check "EXIF orient 6 rotated upright" 1200x800 "$(magick identify -format %wx%h $T/o.jpg)"
check "EXIF stripped" "" "$(exiftool -s3 -Orientation $T/o.jpg)"
J=$(upload $M/logo.png); H_PNG=$(echo "$J" | field hash)
check "upload png" ok "$(echo "$J" | field status)"
ET=$($C -D - -o /dev/null $B/$H_PNG | tr -d '\r' | awk -F': ' 'tolower($1)=="etag"{print $2}')
check "png 304 on If-None-Match" 304 "$(code -H "If-None-Match: $ET" $B/$H_PNG)"

echo "== out-of-memory, then reuse"
OOM0=$(docker exec $CT grep -c "Allowed memory size" /var/log/nginx/pictshare/error.log)
check "9000x9000 jpg upload hits the memory limit" 500 "$(code -H "Authorization: Client-ID $CLIENT_ID" -F "file=@$M/huge.jpg" $B/api/upload.php)"
OOM1=$(docker exec $CT grep -c "Allowed memory size" /var/log/nginx/pictshare/error.log)
check "OOM fatal logged" 1 "$((OOM1-OOM0))"
for i in 1 2 3 4 5; do code $B/$H_JPG >/dev/null; done
check "requests after OOM still fine" 200 "$(code $B/$H_JPG)"

echo "== ffmpeg guards (VideoController::limited)"
guard() { docker exec $CT php -r 'interface ContentController {} require "/var/www/content-controllers/video/video.controller.php";
  exec(VideoController::limited($argv[1], (int)$argv[2])." >/dev/null 2>&1", $o, $rc); echo $rc;' "$1" "$2"; }
check "endless encode killed by the time limit" 9 "$(guard 'ffmpeg -nostdin -f lavfi -i testsrc2=size=320x240 -f null -' 3)"
# frames pile up in paletteuse until palettegen sees an EOF the endless source never sends (a real 266 GB runaway)
check "frame-buffering runaway stopped by the memory cap" 244 "$(guard "ffmpeg -nostdin -y -f lavfi -i testsrc2=size=1920x1080:rate=60 -t 2 -filter_complex '[0:v]split[a][b];[a]palettegen[p];[b][p]paletteuse' /tmp/runaway.gif" 120)"

echo "== video"
J=$(upload $M/ok.mp4); H_OK=$(echo "$J" | field hash)
check "upload compliant mp4" ok "$(echo "$J" | field status)"
$C $B/$H_OK/raw -o $T/ok.mp4
check "compliant mp4 stored byte-for-byte" "$(shasum $M/ok.mp4 | cut -c1-40)" "$(shasum $T/ok.mp4 | cut -c1-40)"
check "range 0-99 -> 206" 206 "$(code -r 0-99 $B/$H_OK/raw)"
check "range bytes" 100 "$($C -r 0-99 $B/$H_OK/raw | wc -c | tr -d ' ')"
check "unsatisfiable range -> 416" 416 "$(code -r 99999999-  $B/$H_OK/raw)"
check "OPTIONS on raw" 200 "$(code -X OPTIONS $B/$H_OK/raw)"
check "preview" 200 "$(code $B/preview/$H_OK)"
J=$(upload $M/moov_end.mp4); H_ME=$(echo "$J" | field hash)
check "upload moov-at-end mp4" ok "$(echo "$J" | field status)"
$C $B/$H_ME/raw -o $T/me.mp4
FIRST=$(ffprobe -v trace $T/me.mp4 2>&1 | grep -oE "type:'(moov|mdat)'" | head -1)
check "moov-at-end remuxed synchronously (moov first)" "type:'moov'" "$FIRST"
J=$(upload $M/yuv444.mp4); H_444=$(echo "$J" | field hash)
check "upload yuv444 mp4" ok "$(echo "$J" | field status)"
for i in $(seq 1 60); do
  $C $B/$H_444/raw -o $T/444.mp4
  PF=$(ffprobe -v error -select_streams v -show_entries stream=pix_fmt -of csv=p=0 $T/444.mp4)
  [ "$PF" = yuv420p ] && break; sleep 2
done
check "background transcode finished (nohup worker outlives fpm worker)" yuv420p "$PF"

echo "== PHP error log (new lines other than the expected OOM)"
docker exec $CT sh -c 'grep -i "php" /var/log/nginx/pictshare/error.log | grep -v "Allowed memory size" | tail -5'
echo "== $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
