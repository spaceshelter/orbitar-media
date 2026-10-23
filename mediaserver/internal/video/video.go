// Package video decides per upload what browsers can play and makes it so, with ffprobe and
// ffmpeg. Port of PictShare's video.controller.php (planNormalization and friends): the target is
// H.264 up to High profile / level 5.2, 8-bit 4:2:0, progressive, even dimensions, AAC or MP3
// audio, one video and at most one audio stream, moov before mdat.
package video

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spaceshelter/orbitar-media/mediaserver/internal/proc"
	"github.com/spaceshelter/orbitar-media/mediaserver/internal/store"
)

var (
	compatibleProfiles     = []string{"constrained baseline", "baseline", "main", "high"}
	compatiblePixelFormats = []string{"yuv420p", "yuvj420p"}
	compatibleAudioCodecs  = []string{"aac", "mp3"}
	interlacedFieldOrders  = []string{"tt", "bb", "tb", "bt"}
)

const (
	maxH264Level = 52 // 5.2 covers 4K@60
	maxPixels    = 3840 * 2160
	maxSide      = 4096
	maxFps       = 60
)

// Time limits per ffmpeg/ffprobe run.
const (
	ProbeTimeout     = 60 * time.Second
	PreviewTimeout   = 120 * time.Second
	RemuxTimeout     = 300 * time.Second
	GifTimeout       = 600 * time.Second
	TranscodeTimeout = 3600 * time.Second
)

// Tools locates ffmpeg/ffprobe and caps their memory.
type Tools struct {
	FFmpeg, FFprobe string
	MemoryMB        int
}

func (t *Tools) run(ctx context.Context, timeout time.Duration, name string, args ...string) proc.Result {
	return proc.Run(ctx, proc.Limits{Timeout: timeout, MemoryMB: max(256, t.MemoryMB)}, name, args...)
}

// Stream is the part of ffprobe's stream object we look at.
type Stream struct {
	Index        int               `json:"index"`
	CodecType    string            `json:"codec_type"`
	CodecName    string            `json:"codec_name"`
	Profile      string            `json:"profile"`
	Level        int               `json:"level"`
	PixFmt       string            `json:"pix_fmt"`
	Width        int               `json:"width"`
	Height       int               `json:"height"`
	AvgFrameRate string            `json:"avg_frame_rate"`
	RFrameRate   string            `json:"r_frame_rate"`
	FieldOrder   string            `json:"field_order"`
	Channels     *int              `json:"channels"`
	Duration     string            `json:"duration"`
	Disposition  map[string]int    `json:"disposition"`
	Tags         map[string]string `json:"tags"`
	SideDataList []map[string]any  `json:"side_data_list"`
}

// Probe is ffprobe's -show_format -show_streams output.
type Probe struct {
	Streams []Stream `json:"streams"`
	Format  struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
	} `json:"format"`
}

// Probe runs ffprobe; nil when the file is not media (or ffprobe failed).
func (t *Tools) Probe(ctx context.Context, file string) *Probe {
	r := t.run(ctx, ProbeTimeout, t.FFprobe, "-v", "error", "-print_format", "json", "-show_format", "-show_streams", file)
	if len(r.Stdout) == 0 {
		return nil
	}
	var p Probe
	if err := json.Unmarshal(r.Stdout, &p); err != nil || len(p.Streams) == 0 {
		return nil
	}
	return &p
}

// PrimaryVideo is the stream we would play: the first video stream that is not cover art.
func (p *Probe) PrimaryVideo() *Stream {
	for i := range p.Streams {
		s := &p.Streams[i]
		if s.CodecType == "video" && s.Disposition["attached_pic"] == 0 {
			return s
		}
	}
	return nil
}

func (p *Probe) PrimaryAudio() *Stream {
	for i := range p.Streams {
		if p.Streams[i].CodecType == "audio" {
			return &p.Streams[i]
		}
	}
	return nil
}

// frameRate parses one of ffprobe's rational rates (0 when unknown). avg_frame_rate is the real
// rate; r_frame_rate is the nominal one, which VFR sources inflate to 120, 1000 or more.
func frameRate(v string) float64 {
	n, d, ok := strings.Cut(v, "/")
	if !ok {
		return 0
	}
	fn, err1 := strconv.ParseFloat(n, 64)
	fd, err2 := strconv.ParseFloat(d, 64)
	if err1 != nil || err2 != nil || fn <= 0 || fd <= 0 {
		return 0
	}
	return fn / fd
}

// rotation in degrees from the rotate tag or the display matrix (0 when absent).
func rotation(s *Stream) int {
	if v, ok := s.Tags["rotate"]; ok {
		if r, err := strconv.ParseFloat(v, 64); err == nil {
			return int(r) % 360
		}
	}
	for _, sd := range s.SideDataList {
		if r, ok := sd["rotation"].(float64); ok {
			return int(math.Round(r)) % 360
		}
	}
	return 0
}

// Dimensions are the displayed dimensions (rotation applied), which is what the dimension hash
// describes.
func (p *Probe) Dimensions() (int, int, bool) {
	v := p.PrimaryVideo()
	if v == nil || v.Width <= 0 || v.Height <= 0 {
		return 0, 0, false
	}
	w, h := v.Width, v.Height
	if r := rotation(v); r%180 == 90 || r%180 == -90 {
		w, h = h, w
	}
	return w, h, true
}

// IsProperMP4 is the upload type check: an MP4/MOV container with a real H.264 stream of
// non-zero duration.
func (p *Probe) IsProperMP4() bool {
	if p == nil {
		return false
	}
	f := strings.ToLower(p.Format.FormatName)
	if !strings.Contains(f, "mp4") && !strings.Contains(f, "mov") {
		return false
	}
	v := p.PrimaryVideo()
	if v == nil || strings.ToLower(v.CodecName) != "h264" {
		return false
	}
	d := p.Format.Duration
	if d == "" {
		d = v.Duration
	}
	dur, _ := strconv.ParseFloat(d, 64)
	return dur > 0
}

// isInterlaced: browsers render interlaced video with combing, so it gets deinterlaced. Headers
// rarely say (ffprobe reports field_order "unknown" for most H.264), so when they are silent the
// first few frames are decoded and their interlaced flag inspected.
func (t *Tools) isInterlaced(ctx context.Context, file string, index int, fieldOrder string) bool {
	if slices.Contains(interlacedFieldOrders, fieldOrder) {
		return true
	}
	if fieldOrder == "progressive" {
		return false
	}
	r := t.run(ctx, ProbeTimeout, t.FFprobe, "-v", "error", "-select_streams", strconv.Itoa(index),
		"-read_intervals", "%+#5", "-show_entries", "frame=interlaced_frame", "-of", "csv=p=0", file)
	for _, line := range strings.Split(strings.TrimSpace(string(r.Stdout)), "\n") {
		if strings.TrimSpace(line) == "1" {
			return true
		}
	}
	return false
}

// MoovBeforeMdat walks the top-level MP4 boxes: true when moov precedes mdat, false when it
// trails it, nil when the layout can't be parsed.
func MoovBeforeMdat(file string) *bool {
	f, err := os.Open(file)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	size := st.Size()
	var pos int64
	seenMdat := false
	hdr := make([]byte, 16)
	for pos+8 <= size {
		if _, err := f.ReadAt(hdr[:8], pos); err != nil {
			return nil
		}
		boxSize := int64(binary.BigEndian.Uint32(hdr[:4]))
		typ := string(hdr[4:8])
		switch {
		case boxSize == 1:
			if _, err := f.ReadAt(hdr[8:16], pos+8); err != nil && err != io.EOF {
				return nil
			}
			ext := binary.BigEndian.Uint64(hdr[8:16])
			if ext < 16 || ext > math.MaxInt64 {
				return nil
			}
			boxSize = int64(ext)
		case boxSize == 0:
			boxSize = size - pos
		case boxSize < 8:
			return nil
		}
		if typ == "moov" {
			r := !seenMdat
			return &r
		}
		if typ == "mdat" {
			seenMdat = true
		}
		pos += boxSize
	}
	return nil
}

// Plan says what has to be done to make a file play everywhere.
type Plan struct {
	Video      string // copy | transcode
	Audio      string // copy | transcode | none
	Remux      bool   // container needs rewriting (moov placement, stray streams)
	Needed     bool
	Reasons    []string
	VIndex     int
	AIndex     int // -1: no audio
	Interlaced bool
	CapSize    bool    // shrink to fit 3840x2160
	FpsOut     float64 // 0: keep
	Channels   int
}

// Plan is planNormalization. nil when the file has no playable video stream.
func (t *Tools) Plan(ctx context.Context, file string, p *Probe) *Plan {
	if p == nil {
		p = t.Probe(ctx, file)
	}
	if p == nil {
		return nil
	}
	v := p.PrimaryVideo()
	if v == nil {
		return nil
	}
	a := p.PrimaryAudio()
	pl := &Plan{Video: "copy", Audio: "none", VIndex: v.Index, AIndex: -1}
	if a != nil {
		pl.Audio = "copy"
		pl.AIndex = a.Index
		pl.Channels = 2
		if a.Channels != nil {
			pl.Channels = *a.Channels
		}
	}

	codec := strings.ToLower(v.CodecName)
	if codec != "h264" {
		pl.Reasons = append(pl.Reasons, "video codec "+codec)
	}
	pix := strings.ToLower(v.PixFmt)
	if !slices.Contains(compatiblePixelFormats, pix) {
		pl.Reasons = append(pl.Reasons, "pixel format "+pix)
	}
	profile := strings.ToLower(strings.TrimSpace(v.Profile))
	if codec == "h264" && profile != "" && profile != "unknown" && !slices.Contains(compatibleProfiles, profile) {
		pl.Reasons = append(pl.Reasons, "profile "+profile)
	}
	if codec == "h264" && v.Level > maxH264Level {
		pl.Reasons = append(pl.Reasons, fmt.Sprintf("level %d", v.Level))
		pl.CapSize = true // whatever pushed the level up, fitting into 4K brings it back
	}
	w, h := v.Width, v.Height
	if w <= 0 || h <= 0 || w%2 != 0 || h%2 != 0 {
		pl.Reasons = append(pl.Reasons, fmt.Sprintf("dimensions %dx%d", w, h))
	}
	// beyond 4K@60 is above level 5.2 whatever the source says; 5.2 also bounds each side
	if w*h > maxPixels || w > maxSide || h > maxSide {
		pl.Reasons = append(pl.Reasons, fmt.Sprintf("size %dx%d", w, h))
		pl.CapSize = true
	}
	// Only the real (average) rate decides whether a transcode is needed; the nominal rate pins
	// the output rate once we transcode anyway (ffmpeg pads VFR input up to r_frame_rate).
	avg, nominal := frameRate(v.AvgFrameRate), frameRate(v.RFrameRate)
	if avg > maxFps {
		pl.Reasons = append(pl.Reasons, "frame rate "+formatFloat(math.Round(avg*100)/100))
	}
	if avg > maxFps || nominal > maxFps {
		if avg > 0 && avg <= maxFps {
			pl.FpsOut = math.Round(avg*1000) / 1000
		} else {
			pl.FpsOut = maxFps
		}
	}
	fieldOrder := strings.ToLower(v.FieldOrder)
	if fieldOrder == "" {
		fieldOrder = "unknown"
	}
	if t.isInterlaced(ctx, file, v.Index, fieldOrder) {
		pl.Interlaced = true
		pl.Reasons = append(pl.Reasons, "interlaced ("+fieldOrder+")")
	}
	if len(pl.Reasons) > 0 {
		pl.Video = "transcode"
	}

	if a != nil {
		ac := strings.ToLower(a.CodecName)
		if !slices.Contains(compatibleAudioCodecs, ac) {
			pl.Audio = "transcode"
			pl.Reasons = append(pl.Reasons, "audio codec "+ac)
		}
	}

	format := strings.ToLower(p.Format.FormatName)
	if !strings.Contains(format, "mp4") && !strings.Contains(format, "mov") {
		pl.Remux = true
		pl.Reasons = append(pl.Reasons, "container "+format)
	}
	wanted := 1
	if a != nil {
		wanted = 2
	}
	if len(p.Streams) > wanted {
		pl.Remux = true
		pl.Reasons = append(pl.Reasons, fmt.Sprintf("%d streams", len(p.Streams)))
	}
	if m := MoovBeforeMdat(file); m == nil || !*m {
		pl.Remux = true
		if m == nil {
			pl.Reasons = append(pl.Reasons, "unparseable atom layout")
		} else {
			pl.Reasons = append(pl.Reasons, "moov after mdat")
		}
	}
	pl.Needed = pl.Video == "transcode" || pl.Audio == "transcode" || pl.Remux
	return pl
}

func formatFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// Args is the ffmpeg command line that writes a browser-friendly MP4 of file to out.
func (pl *Plan) Args(file, out string) []string {
	args := []string{"-y", "-nostdin", "-loglevel", "error", "-i", file, "-map", "0:" + strconv.Itoa(pl.VIndex)}
	if pl.AIndex >= 0 {
		args = append(args, "-map", "0:"+strconv.Itoa(pl.AIndex))
	}
	if pl.Video == "transcode" {
		var filters []string
		if pl.Interlaced {
			filters = append(filters, "yadif")
		}
		if pl.FpsOut > 0 {
			filters = append(filters, "fps="+formatFloat(pl.FpsOut))
		}
		if pl.CapSize { // shrink to fit 3840x2160, never enlarge
			filters = append(filters, "scale=w='min(iw,3840)':h='min(ih,2160)':force_original_aspect_ratio=decrease")
		}
		filters = append(filters, "scale=trunc(iw/2)*2:trunc(ih/2)*2")
		args = append(args, "-c:v", "libx264", "-preset", "medium", "-crf", "23", "-profile:v", "high",
			"-pix_fmt", "yuv420p", "-vf", strings.Join(filters, ","))
	} else {
		args = append(args, "-c:v", "copy")
	}
	switch pl.Audio {
	case "transcode":
		args = append(args, "-c:a", "aac", "-b:a", "128k")
		if pl.Channels > 2 {
			args = append(args, "-ac", "2")
		}
	case "copy":
		args = append(args, "-c:a", "copy")
	}
	// chapters would come back as a data stream and fail the stream-count check next time
	return append(args, "-map_chapters", "-1", "-movflags", "+faststart", "-f", "mp4", out)
}

// Normalize rewrites file in place (atomically) according to pl. Returns true when the file was
// replaced.
func (t *Tools) Normalize(ctx context.Context, file string, pl *Plan) bool {
	if pl == nil || !pl.Needed {
		return false
	}
	timeout := RemuxTimeout
	if pl.Video == "transcode" {
		timeout = TranscodeTimeout
	}
	err := store.WriteAtomic(file, func(tmp string) error {
		r := t.run(ctx, timeout, t.FFmpeg, pl.Args(file, tmp)...)
		if r.Err != nil {
			return fmt.Errorf("ffmpeg: %v: %s", r.Err, r.Stderr)
		}
		out := t.Probe(ctx, tmp)
		if out == nil || out.PrimaryVideo() == nil {
			return fmt.Errorf("ffmpeg output has no video stream")
		}
		if after := t.Plan(ctx, tmp, out); after != nil && after.Needed {
			log.Printf("video: normalised %s still flagged (%s), keeping it anyway", file, strings.Join(after.Reasons, "; "))
		}
		return nil
	})
	if err != nil {
		log.Printf("video: normalising %s failed (%s): %v", file, strings.Join(pl.Reasons, "; "), err)
		return false
	}
	return true
}

// Box is an optional preview size: a plain width keeps the aspect ratio, WxH fits inside.
type Box struct {
	W, H int
	Box  bool
}

// FirstFrame renders the first frame of src as JPEG to out (atomically).
func (t *Tools) FirstFrame(ctx context.Context, src, out string, box *Box) error {
	return store.WriteAtomic(out, func(tmp string) error {
		args := []string{"-y", "-nostdin", "-loglevel", "error", "-i", src, "-vframes", "1"}
		if box != nil {
			if box.Box {
				args = append(args, "-vf", fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease", box.W, box.H))
			} else {
				args = append(args, "-vf", fmt.Sprintf("scale=%d:-2", box.W))
			}
		}
		args = append(args, "-f", "image2", tmp)
		if r := t.run(ctx, PreviewTimeout, t.FFmpeg, args...); r.Err != nil {
			return fmt.Errorf("preview of %s: %v: %s", src, r.Err, r.Stderr)
		}
		return nil
	})
}

// GifToMP4 converts a GIF to H.264 (for /mp4/ URLs of GIFs).
func (t *Tools) GifToMP4(ctx context.Context, src, out string) error {
	return store.WriteAtomic(out, func(tmp string) error {
		r := t.run(ctx, GifTimeout, t.FFmpeg, "-y", "-nostdin", "-loglevel", "error", "-f", "gif", "-i", src,
			"-an", "-c:v", "libx264", "-preset", "medium", "-crf", "23", "-profile:v", "high", "-pix_fmt", "yuv420p",
			"-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2", "-movflags", "+faststart", "-f", "mp4", tmp)
		if r.Err != nil {
			return fmt.Errorf("gif->mp4 of %s: %v: %s", src, r.Err, r.Stderr)
		}
		return nil
	})
}
