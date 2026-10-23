package imaging

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// An untagged CMYK JPEG keeps the colours GD and browsers show (plain formula, not a press profile).
func TestNormalizeUntaggedCMYK(t *testing.T) {
	if _, err := exec.LookPath("vips"); err != nil {
		t.Skip("vips not installed")
	}
	dir := t.TempDir()
	in, out := filepath.Join(dir, "in.jpg"), filepath.Join(dir, "out.jpg")
	// a flat CMYK image: C=0 M=255 Y=0 K=0 -> magenta (255,0,255)
	steps := [][]string{
		{"black", filepath.Join(dir, "z.v"), "16", "16", "--bands", "4"},
		{"linear", filepath.Join(dir, "z.v"), filepath.Join(dir, "c.v"), "1 1 1 1", "0 255 0 0", "--uchar"},
		{"copy", filepath.Join(dir, "c.v"), filepath.Join(dir, "c2.v"), "--interpretation", "cmyk"},
		{"jpegsave", filepath.Join(dir, "c2.v"), in, "--Q", "100"},
	}
	for _, st := range steps {
		if b, err := exec.Command("vips", st...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v %s", st, err, b)
		}
	}
	p := &Processor{MemoryMB: 1024, Timeout: time.Minute, MaxPixels: 1e8, TmpDir: dir}
	if !p.untaggedCMYK(t.Context(), in) {
		t.Fatal("not detected as untagged CMYK")
	}
	if err := p.NormalizeJPEG(t.Context(), in, out); err != nil {
		t.Fatal(err)
	}
	b, err := exec.Command("vips", "getpoint", out, "8", "8").Output()
	if err != nil {
		t.Fatal(err)
	}
	var r, g, bl float64
	if _, err := fmt.Sscan(string(b), &r, &g, &bl); err != nil {
		t.Fatalf("getpoint %q: %v", b, err)
	}
	if r < 245 || g > 10 || bl < 245 {
		t.Errorf("magenta came out as %v %v %v", r, g, bl)
	}
}
