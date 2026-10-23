package imaging

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestResizeDims(t *testing.T) {
	cases := []struct {
		w, h int
		s    Size
		ew   int
		eh   int
	}{
		{1200, 800, Size{300, 300}, 300, 200},
		{800, 1200, Size{300, 300}, 200, 300},
		{1200, 800, Size{3000, 3000}, 1200, 800}, // no upscaling
		{1200, 800, Size{300, 10}, 300, 200},     // landscape: only the width bounds
		{800, 1200, Size{10, 300}, 200, 300},     // portrait: only the height bounds
		{1000, 1000, Size{333, 333}, 333, 333},
		{5000, 3, Size{100, 100}, 100, 1}, // never 0
		{1366, 768, Size{100, 100}, 100, 56},
	}
	for _, c := range cases {
		if w, h := ResizeDims(c.w, c.h, c.s); w != c.ew || h != c.eh {
			t.Errorf("%dx%d to %v: got %dx%d want %dx%d", c.w, c.h, c.s, w, h, c.ew, c.eh)
		}
	}
}

// Names computed with PHP: md5(http_build_query($m, "", ","))
func TestVariantName(t *testing.T) {
	cases := []struct {
		m    Modifiers
		want string
	}{
		{Modifiers{Ops: []string{"size"}, SizeRaw: "300"}, "size=300"},
		{Modifiers{Ops: []string{"size", "rotation"}, SizeRaw: "300", Rotation: "left"}, "size=300,rotation=left"},
		{Modifiers{Ops: []string{"rotation", "size"}, SizeRaw: "300x200", Rotation: "right"}, "rotation=right,size=300x200"},
		{Modifiers{WebP: true}, "webp=1"},
		{Modifiers{Ops: []string{"size"}, SizeRaw: "100", WebP: true, ForceSize: true}, "size=100,webp=1,forcesize=1"},
	}
	for _, c := range cases {
		got := c.m.VariantName("x.jpg")
		want := md5hex(c.want) + "_x.jpg"
		if got != want {
			t.Errorf("%+v: got %s want %s", c.m, got, want)
		}
	}
}

func TestSniff(t *testing.T) {
	dir := t.TempDir()
	for name, data := range map[string]string{
		"jpg": "\xff\xd8\xff\xe0", "png": "\x89PNG\r\n\x1a\n....", "gif": "GIF89a...", "webp": "RIFF\x00\x00\x00\x00WEBPVP8 ",
		"bmp": "BM......", "": "hello world",
	} {
		p := filepath.Join(dir, "f"+name)
		os.WriteFile(p, []byte(data), 0o644)
		if got := Sniff(p); got != name {
			t.Errorf("%q: got %q", name, got)
		}
	}
}

// Needs the vips tools (the dev image has them).
func TestVariantPipeline(t *testing.T) {
	if _, err := exec.LookPath("vips"); err != nil {
		t.Skip("vips not installed")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.png")
	if out, err := exec.Command("vips", "black", src, "120", "80", "--bands", "3").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	p := &Processor{Timeout: time.Minute, MemoryMB: 2048, TmpDir: dir, MaxPixels: 100e6}
	ctx := context.Background()
	cases := []struct {
		m      Modifiers
		typ    string
		ew, eh int
	}{
		{Modifiers{Ops: []string{"size"}, Size: Size{60, 60}, SizeRaw: "60"}, "png", 60, 40},
		{Modifiers{Ops: []string{"rotation"}, Rotation: "left"}, "png", 80, 120},
		{Modifiers{Ops: []string{"rotation", "size"}, Rotation: "right", Size: Size{40, 40}, SizeRaw: "40"}, "png", 26, 40},
		{Modifiers{Ops: []string{"size"}, Size: Size{50, 50}, SizeRaw: "50", ForceSize: true}, "png", 50, 50},
		{Modifiers{WebP: true}, "webp", 120, 80},
	}
	for i, c := range cases {
		out := filepath.Join(dir, "out")
		if err := p.Variant(ctx, src, out, c.typ, c.m); err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if got := Sniff(out); got != c.typ {
			t.Errorf("case %d: type %q", i, got)
		}
		w, h, err := p.Dimensions(ctx, out)
		if err != nil || w != c.ew || h != c.eh {
			t.Errorf("case %d: %dx%d %v, want %dx%d", i, w, h, err, c.ew, c.eh)
		}
	}
}
