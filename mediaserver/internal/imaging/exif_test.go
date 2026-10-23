package imaging

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// withOrientation inserts an EXIF APP1 segment holding only the Orientation tag after SOI.
func withOrientation(jpg []byte, o uint16) []byte {
	var tiff bytes.Buffer
	tiff.WriteString("MM\x00\x2a")
	binary.Write(&tiff, binary.BigEndian, uint32(8))      // IFD0 offset
	binary.Write(&tiff, binary.BigEndian, uint16(1))      // one entry
	binary.Write(&tiff, binary.BigEndian, uint16(0x0112)) // Orientation
	binary.Write(&tiff, binary.BigEndian, uint16(3))      // SHORT
	binary.Write(&tiff, binary.BigEndian, uint32(1))
	binary.Write(&tiff, binary.BigEndian, o)
	binary.Write(&tiff, binary.BigEndian, uint16(0))
	binary.Write(&tiff, binary.BigEndian, uint32(0)) // no next IFD
	payload := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	seg := []byte{0xff, 0xe1, 0, 0}
	binary.BigEndian.PutUint16(seg[2:], uint16(len(payload)+2))
	out := append([]byte{}, jpg[:2]...)
	out = append(out, seg...)
	out = append(out, payload...)
	return append(out, jpg[2:]...)
}

// Phone photos: the pixels are stored sideways and the EXIF tag says how to show them. Upload
// applies the tag to the pixels and strips it, for all 8 orientations.
func TestNormalizeAppliesOrientation(t *testing.T) {
	if _, err := exec.LookPath("vips"); err != nil {
		t.Skip("vips not installed")
	}
	// stored 40x20, red top-left quadrant, white elsewhere
	src := image.NewRGBA(image.Rect(0, 0, 40, 20))
	for y := range 20 {
		for x := range 40 {
			c := color.RGBA{255, 255, 255, 255}
			if x < 20 && y < 10 {
				c = color.RGBA{255, 0, 0, 255}
			}
			src.Set(x, y, c)
		}
	}
	var plain bytes.Buffer
	if err := jpeg.Encode(&plain, src, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatal(err)
	}
	// where the stored top-left corner ends up when the tag is applied
	const tl, tr, bl, br = 0, 1, 2, 3
	want := map[uint16]int{1: tl, 2: tr, 3: br, 4: bl, 5: tl, 6: tr, 7: br, 8: bl}

	dir := t.TempDir()
	p := &Processor{MemoryMB: 1024, Timeout: time.Minute, MaxPixels: 1e8, TmpDir: dir}
	for o := uint16(1); o <= 8; o++ {
		in, out := filepath.Join(dir, "in.jpg"), filepath.Join(dir, "out.jpg")
		if err := os.WriteFile(in, withOrientation(plain.Bytes(), o), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := p.NormalizeJPEG(t.Context(), in, out); err != nil {
			t.Fatalf("orientation %d: %v", o, err)
		}
		b, _ := os.ReadFile(out)
		if bytes.Contains(b, []byte("Exif\x00\x00")) {
			t.Errorf("orientation %d: EXIF not stripped", o)
		}
		img, err := jpeg.Decode(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		w, h := img.Bounds().Dx(), img.Bounds().Dy()
		if wantW := map[bool]int{true: 20, false: 40}[o >= 5]; w != wantW || h != 60-wantW {
			t.Errorf("orientation %d: %dx%d", o, w, h)
			continue
		}
		red := -1
		for i, pt := range []image.Point{{w / 4, h / 4}, {3 * w / 4, h / 4}, {w / 4, 3 * h / 4}, {3 * w / 4, 3 * h / 4}} {
			if r, g, _, _ := img.At(pt.X, pt.Y).RGBA(); r > 0xc000 && g < 0x4000 {
				red = i
			}
		}
		if red != want[o] {
			t.Errorf("orientation %d: red corner is %d, want %d", o, red, want[o])
		}
	}
}
