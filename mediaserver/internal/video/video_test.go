package video

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func tools(t *testing.T) *Tools {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	return &Tools{FFmpeg: "ffmpeg", FFprobe: "ffprobe", MemoryMB: 2560}
}

// gen makes a 1 s test clip with ffmpeg's lavfi sources.
func gen(t *testing.T, dir, name string, args ...string) string {
	out := filepath.Join(dir, name)
	base := []string{"-y", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25:duration=1",
		"-f", "lavfi", "-i", "sine=duration=1"}
	if o, err := exec.Command("ffmpeg", append(append(base, args...), out)...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %s: %v %s", name, err, o)
	}
	return out
}

func TestPlan(t *testing.T) {
	tl := tools(t)
	dir := t.TempDir()
	ctx := context.Background()
	h264 := []string{"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac"}
	cases := []struct {
		name    string
		args    []string
		video   string
		audio   string
		remux   bool
		reasons string
	}{
		{"ok.mp4", append(h264, "-movflags", "+faststart"), "copy", "copy", false, ""},
		{"moovend.mp4", h264, "copy", "copy", true, "moov after mdat"},
		{"yuv444.mp4", []string{"-c:v", "libx264", "-pix_fmt", "yuv444p", "-c:a", "aac", "-movflags", "+faststart"}, "transcode", "copy", false, "pixel format yuv444p; profile high 4:4:4 predictive"},
		{"ac3.mp4", []string{"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "ac3", "-movflags", "+faststart"}, "copy", "transcode", false, "audio codec ac3"},
		{"noaudio.mp4", []string{"-map", "0:v", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-movflags", "+faststart"}, "copy", "none", false, ""},
		{"fps120.mp4", []string{"-vf", "fps=120", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-movflags", "+faststart"}, "transcode", "copy", false, "frame rate 120"},
		{"mkv.mkv", h264, "copy", "copy", true, "container matroska,webm; unparseable atom layout"},
	}
	for _, c := range cases {
		f := gen(t, dir, c.name, c.args...)
		pl := tl.Plan(ctx, f, nil)
		if pl == nil {
			t.Fatalf("%s: no plan", c.name)
		}
		if pl.Video != c.video || pl.Audio != c.audio || pl.Remux != c.remux || strings.Join(pl.Reasons, "; ") != c.reasons {
			t.Errorf("%s: got %+v", c.name, pl)
		}
		if pl.Needed {
			if !tl.Normalize(ctx, f, pl) {
				t.Fatalf("%s: normalize failed", c.name)
			}
			if after := tl.Plan(ctx, f, nil); after == nil || after.Needed {
				t.Errorf("%s: still needs work after normalising: %+v", c.name, after)
			}
		}
	}
}

func TestProperMP4AndDimensions(t *testing.T) {
	tl := tools(t)
	dir := t.TempDir()
	ctx := context.Background()
	f := gen(t, dir, "rot.mp4", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-metadata:s:v", "rotate=90")
	// newer ffmpeg ignores the rotate tag on output; set the display matrix instead
	rot := filepath.Join(dir, "rot2.mp4")
	if o, err := exec.Command("ffmpeg", "-y", "-loglevel", "error", "-display_rotation", "90", "-i", f, "-c", "copy", rot).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, o)
	}
	p := tl.Probe(ctx, rot)
	if !p.IsProperMP4() {
		t.Fatal("not a proper mp4")
	}
	if w, h, ok := p.Dimensions(); !ok || w != 240 || h != 320 {
		t.Fatalf("dimensions %dx%d", w, h)
	}
	webm := gen(t, dir, "x.webm", "-c:v", "libvpx-vp9", "-c:a", "libopus")
	if tl.Probe(ctx, webm).IsProperMP4() {
		t.Fatal("webm accepted")
	}
	txt := filepath.Join(dir, "x.txt")
	os.WriteFile(txt, []byte("hello"), 0o644)
	if p := tl.Probe(ctx, txt); p.IsProperMP4() {
		t.Fatal("text accepted")
	}
}

func TestPreviewAndGif(t *testing.T) {
	tl := tools(t)
	dir := t.TempDir()
	ctx := context.Background()
	f := gen(t, dir, "v.mp4", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac")
	for _, b := range []*Box{nil, {W: 100}, {W: 100, H: 50, Box: true}} {
		out := filepath.Join(dir, "p.jpg")
		if err := tl.FirstFrame(ctx, f, out, b); err != nil {
			t.Fatal(err)
		}
	}
	gif := gen(t, dir, "a.gif", "-map", "0:v")
	if err := tl.GifToMP4(ctx, gif, filepath.Join(dir, "a.mp4")); err != nil {
		t.Fatal(err)
	}
	if !tl.Probe(ctx, filepath.Join(dir, "a.mp4")).IsProperMP4() {
		t.Fatal("gif->mp4 output not playable")
	}
}

func TestMoovBeforeMdatGarbage(t *testing.T) {
	f := filepath.Join(t.TempDir(), "g")
	os.WriteFile(f, []byte("\x00\x00\x00\x01mdat\x00\x00\x00\x00"), 0o644)
	if MoovBeforeMdat(f) != nil {
		t.Fatal("garbage parsed")
	}
}
