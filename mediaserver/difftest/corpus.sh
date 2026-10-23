#!/bin/bash
# corpus.sh <dir>: synthetic images, videos and text covering the edge cases of both pipelines.
# Needs magick, exiftool and ffmpeg (with libx265, libvpx, libopus, libmp3lame) on the host.
set -u
mkdir -p "$1" && cd "$1" || exit 1
magick -size 1200x800 gradient:red-blue -fill white -pointsize 140 -gravity northwest -annotate +30+20 'TL' base.jpg
# EXIF 1..8: pixels hold the inverse transform; a viewer applying the tag gets base.jpg back
inv=( "" "" "-flop" "-rotate 180" "-flip" "-transpose" "-rotate -90" "-transverse" "-rotate 90" )
for o in 1 2 3 4 5 6 7 8; do magick base.jpg ${inv[$o]} i_exif$o.jpg; exiftool -q -Orientation=$o -n -overwrite_original i_exif$o.jpg; done
magick base.jpg -interlace JPEG i_progressive.jpg
magick base.jpg -colorspace CMYK i_cmyk.jpg
magick base.jpg -colorspace Gray i_gray.jpg
magick -size 600x400 radial-gradient:'rgba(255,0,0,1)'-'rgba(0,0,255,0)' PNG32:i_rgba.png
magick -size 600x400 plasma: -colors 64 PNG8:i_palette.png
magick -size 600x400 gradient:white-black -depth 16 PNG48:i_16bit.png
magick base.jpg -quality 80 i_lossy.webp
magick i_rgba.png -define webp:lossless=true i_alpha.webp
magick -delay 20 -size 200x200 xc:red xc:green xc:blue -loop 0 i_anim.webp
magick -delay 20 -size 200x200 xc:red xc:green xc:blue -loop 0 i_anim.gif
magick base.jpg i_static.gif
magick base.jpg BMP3:i_bmp.bmp
magick base.jpg -resize 64x64 i_icon.ico
magick base.jpg i_tiff.tiff
magick -size 7000x7000 plasma: -quality 92 i_huge7000.jpg
magick -size 3000x5000 plasma: -quality 85 i_tall.jpg
magick -size 1x1 xc:red i_1x1.png
magick -size 5000x20 gradient: i_wide.png
head -c 3000 i_rgba.png > i_truncated.png
echo "hello world" > t_text.txt; : > t_empty.jpg
f() { ffmpeg -nostdin -loglevel error -y "$@" || echo "FAILED: $*" >&2; }
SRC=(-f lavfi -t 4 -i testsrc2=size=640x360:rate=30 -f lavfi -t 4 -i sine=f=440:sample_rate=48000)
f "${SRC[@]}" -c:v libx264 -profile:v high -pix_fmt yuv420p -c:a aac -movflags +faststart v_ok.mp4
f "${SRC[@]}" -c:v libx264 -profile:v high -pix_fmt yuv420p -c:a aac v_moovend.mp4
f "${SRC[@]}" -c:v libx264 -pix_fmt yuv420p -c:a ac3 -movflags +faststart v_ac3.mp4
f "${SRC[@]}" -c:v libx264 -pix_fmt yuv420p -c:a libopus -movflags +faststart v_opus.mp4
f "${SRC[@]}" -c:v libx264 -pix_fmt yuv420p -c:a libmp3lame -movflags +faststart v_mp3.mp4
f -f lavfi -i testsrc2=size=640x360:rate=30 -t 4 -c:v libx264 -pix_fmt yuv420p -movflags +faststart v_noaudio.mp4
printf "1\n00:00:00,000 --> 00:00:02,000\nhello\n" > s.srt
f "${SRC[@]}" -i s.srt -map 0 -map 1 -map 2 -c:v libx264 -pix_fmt yuv420p -c:a aac -c:s mov_text -movflags +faststart v_subs.mp4
printf ";FFMETADATA1\n[CHAPTER]\nTIMEBASE=1/1000\nSTART=0\nEND=2000\ntitle=one\n[CHAPTER]\nTIMEBASE=1/1000\nSTART=2000\nEND=4000\ntitle=two\n" > ch.txt
f "${SRC[@]}" -i ch.txt -map 0 -map 1 -map_chapters 2 -c:v libx264 -pix_fmt yuv420p -c:a aac -movflags +faststart v_chapters.mp4
f "${SRC[@]}" -c:v libx264 -pix_fmt yuv444p -c:a aac -movflags +faststart v_yuv444.mp4
f "${SRC[@]}" -c:v libx264 -pix_fmt yuv420p10le -c:a aac -movflags +faststart v_hi10.mp4
f -f lavfi -i testsrc2=size=720x576:rate=50 -f lavfi -i sine -t 4 -vf tinterlace=interleave_top,fieldorder=tff -c:v libx264 -flags +ildct+ilme -x264opts tff=1 -pix_fmt yuv420p -c:a aac -movflags +faststart v_interlaced.mp4
f -f lavfi -i testsrc2=size=641x361:rate=30 -t 3 -c:v libx264 -pix_fmt yuv444p -movflags +faststart v_odd444.mp4
f -f lavfi -i testsrc2=size=4096x2304:rate=30 -t 1 -c:v libx264 -preset ultrafast -pix_fmt yuv420p -movflags +faststart v_over4k.mp4
f -f lavfi -i testsrc2=size=3840x2160:rate=60 -t 1 -c:v libx264 -preset ultrafast -pix_fmt yuv420p -level 5.2 -movflags +faststart v_4k60.mp4
f -f lavfi -i testsrc2=size=640x360:rate=120 -t 3 -c:v libx264 -pix_fmt yuv420p -movflags +faststart v_120fps.mp4
f -f lavfi -i testsrc2=size=640x360:rate=60 -t 4 -vf "setpts='if(lt(T,2),PTS,PTS+(N-120)*2/TB/60)'" -fps_mode vfr -c:v libx264 -pix_fmt yuv420p -movflags +faststart v_vfr.mp4
f -f lavfi -i testsrc2=size=640x360:rate=30 -t 3 -c:v libx264 -pix_fmt yuv420p -level 6.0 -movflags +faststart v_level6.mp4
f -display_rotation:v:0 90 -i v_ok.mp4 -c copy -movflags +faststart v_rot90.mp4
f "${SRC[@]}" -c:v libx264 -pix_fmt yuv420p -c:a aac v_mov.mov
f "${SRC[@]}" -c:v libx265 -x265-params log-level=error -pix_fmt yuv420p -tag:v hvc1 -c:a aac -movflags +faststart v_hevc.mp4
f "${SRC[@]}" -c:v libvpx-vp9 -deadline realtime -c:a libopus v_vp9.webm
head -c 55000000 /dev/urandom > v_toobig.mp4
rm -f s.srt ch.txt base.jpg
