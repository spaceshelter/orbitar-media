// Package imaging is the still-image pipeline, built on the libvips command line tools: upload
// normalisation (EXIF orientation applied, metadata stripped) and the cached URL variants
// (resize, forced size, rotation, WebP). Running vips as a child process keeps decoders out of
// the server's address space and puts each run under the same time and memory limits as ffmpeg.
package imaging

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "golang.org/x/image/webp"

	"github.com/spaceshelter/orbitar-media/mediaserver/internal/proc"
)

// Processor runs vips.
type Processor struct {
	MemoryMB  int           // ulimit -v per vips run
	Timeout   time.Duration // per vips run
	MaxPixels int64         // refuse to decode anything larger
	TmpDir    string        // intermediate files
}

// Sniff returns the image type from the magic bytes, using PictShare's extensions: jpg, png, gif,
// webp, bmp or ico; "" when it is none of them.
func Sniff(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	b := make([]byte, 16)
	n, _ := f.Read(b)
	b = b[:n]
	switch {
	case bytes.HasPrefix(b, []byte("GIF87a")), bytes.HasPrefix(b, []byte("GIF89a")):
		return "gif"
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return "png"
	case bytes.HasPrefix(b, []byte("\xff\xd8\xff")):
		return "jpg"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "webp"
	case bytes.HasPrefix(b, []byte("BM")):
		return "bmp"
	case bytes.HasPrefix(b, []byte("\x00\x00\x01\x00")):
		return "ico"
	}
	return ""
}

// Dimensions returns the pixel size from the file header.
func (p *Processor) Dimensions(ctx context.Context, path string) (int, int, error) {
	if f, err := os.Open(path); err == nil {
		cfg, _, err := image.DecodeConfig(f)
		f.Close()
		if err == nil && cfg.Width > 0 && cfg.Height > 0 {
			return cfg.Width, cfg.Height, nil
		}
	}
	// formats or variants the Go decoders don't parse (e.g. CMYK progressive JPEG)
	r := p.run(ctx, "vipsheader", "-f", "width", path)
	w, err1 := strconv.Atoi(strings.TrimSpace(string(r.Stdout)))
	r2 := p.run(ctx, "vipsheader", "-f", "height", path)
	h, err2 := strconv.Atoi(strings.TrimSpace(string(r2.Stdout)))
	if r.Err != nil || r2.Err != nil || err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return 0, 0, fmt.Errorf("cannot read dimensions of %s: %v %v", path, r.Err, r.Stderr)
	}
	return w, h, nil
}

func (p *Processor) run(ctx context.Context, name string, args ...string) proc.Result {
	return proc.Run(ctx, proc.Limits{Timeout: p.Timeout, MemoryMB: p.MemoryMB}, name, args...)
}

func (p *Processor) checkSize(w, h int) error {
	if p.MaxPixels > 0 && int64(w)*int64(h) > p.MaxPixels {
		return fmt.Errorf("image too large: %dx%d", w, h)
	}
	return nil
}

// NormalizeJPEG re-encodes an uploaded JPEG the way every stored JPEG looks: EXIF orientation
// applied to the pixels, colours in sRGB, all metadata (EXIF, GPS, ICC) removed, quality 90.
func (p *Processor) NormalizeJPEG(ctx context.Context, in, out string) error {
	w, h, err := p.Dimensions(ctx, in)
	if err != nil {
		return err
	}
	if err := p.checkSize(w, h); err != nil {
		return err
	}
	tmpDir, err := os.MkdirTemp(p.TmpDir, "vips")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	src := in
	if p.untaggedCMYK(ctx, in) {
		if src, err = p.naiveCMYKToRGB(ctx, in, tmpDir); err != nil {
			return err
		}
	}
	mid := filepath.Join(tmpDir, "rotated.v")
	if r := p.run(ctx, "vips", "thumbnail", src, mid, "10000000", "--height", "10000000",
		"--size", "down", "--export-profile", "srgb"); r.Err != nil {
		return fmt.Errorf("vips thumbnail: %v: %s", r.Err, r.Stderr)
	}
	return p.save(ctx, mid, out, "jpg")
}

// untaggedCMYK: a CMYK JPEG without an ICC profile. vips would convert it through a press
// profile; browsers and GD use the plain formula, so the colours would change on upload.
func (p *Processor) untaggedCMYK(ctx context.Context, in string) bool {
	r := p.run(ctx, "vipsheader", "-f", "interpretation", in)
	// prints "((VipsInterpretation) VIPS_INTERPRETATION_CMYK)"
	if r.Err != nil || !strings.Contains(strings.ToLower(string(r.Stdout)), "cmyk") {
		return false
	}
	return p.run(ctx, "vipsheader", "-f", "icc-profile-data", in).Err != nil
}

// naiveCMYKToRGB applies the orientation, then R = (255-C)(255-K)/255 and so on, into an
// untagged sRGB image in dir.
func (p *Processor) naiveCMYKToRGB(ctx context.Context, in, dir string) (string, error) {
	f := func(n string) string { return filepath.Join(dir, n) }
	steps := [][]string{
		{"autorot", in, f("rot.v")},
		{"extract_band", f("rot.v"), f("cmy.v"), "0", "--n", "3"},
		{"extract_band", f("rot.v"), f("k.v"), "3"},
		{"invert", f("cmy.v"), f("icmy.v")},
		{"invert", f("k.v"), f("ik.v")},
		{"multiply", f("icmy.v"), f("ik.v"), f("m.v")},
		{"linear", f("m.v"), f("l.v"), "0.00392156862745098", "0", "--uchar"},
		{"copy", f("l.v"), f("rgb.v"), "--interpretation", "srgb"},
	}
	for _, st := range steps {
		if r := p.run(ctx, "vips", st...); r.Err != nil {
			return "", fmt.Errorf("vips %s: %v: %s", st[0], r.Err, r.Stderr)
		}
	}
	return f("rgb.v"), nil
}

// save encodes in to out in the format PictShare used for typ (out's name doesn't matter).
func (p *Processor) save(ctx context.Context, in, out, typ string) error {
	var args []string
	switch typ {
	case "webp":
		args = []string{"webpsave", in, out, "--Q", "80", "--keep", "none"}
	case "png", "x-png":
		args = []string{"pngsave", in, out, "--compression", "6", "--keep", "none"}
	default: // jpg, jpeg: what GD's imagejpeg(…, 90) wrote, 4:2:0 included
		args = []string{"jpegsave", in, out, "--Q", "90", "--keep", "none", "--subsample-mode", "on"}
	}
	if r := p.run(ctx, "vips", args...); r.Err != nil {
		return fmt.Errorf("vips %s: %v: %s", args[0], r.Err, r.Stderr)
	}
	return nil
}

// Size is a requested bounding box ("300" means 300x300).
type Size struct{ W, H int }

// Modifiers of a still image, in the order PictShare applies them.
type Modifiers struct {
	// Ops: "size" and "rotation" in order of first appearance in the URL
	Ops       []string
	Size      Size
	SizeRaw   string // as written in the URL, for the variant name
	Rotation  string // upside, left, right
	WebP      bool
	ForceSize bool
}

func (m Modifiers) Empty() bool { return len(m.Ops) == 0 && !m.WebP }

// VariantName is PictShare's cache file name for a variant of hash:
// md5(http_build_query($modifiers, "", ",")) + "_" + hash.
func (m Modifiers) VariantName(hash string) string {
	var parts []string
	for _, op := range m.Ops {
		switch op {
		case "size":
			parts = append(parts, "size="+m.SizeRaw)
		case "rotation":
			parts = append(parts, "rotation="+m.Rotation)
		}
	}
	if m.WebP {
		parts = append(parts, "webp=1")
	}
	if m.ForceSize {
		parts = append(parts, "forcesize=1")
	}
	sum := md5.Sum([]byte(strings.Join(parts, ",")))
	return hex.EncodeToString(sum[:]) + "_" + hash
}

// ResizeDims is PictShare's resize(): never larger than the original, and the box only bounds
// the height of portrait images and the width of everything else.
func ResizeDims(w, h int, s Size) (int, int) {
	maxW, maxH := s.W, s.H
	if maxW > w {
		maxW = w
	}
	if maxH > h {
		maxH = h
	}
	if h > w {
		return max(1, int(float64(w)*(float64(maxH)/float64(h)))), maxH
	}
	return maxW, max(1, int(float64(h)*(float64(maxW)/float64(w))))
}

// ErrDecode means the stored image could not be read.
var ErrDecode = errors.New("could not decode image")

// Variant renders the variant of src described by m into out, whose format is typ (the
// original's extension, or webp).
func (p *Processor) Variant(ctx context.Context, src, out, typ string, m Modifiers) error {
	w, h, err := p.Dimensions(ctx, src)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDecode, err)
	}
	if err := p.checkSize(w, h); err != nil {
		return fmt.Errorf("%w: %v", ErrDecode, err)
	}

	tmpDir, err := os.MkdirTemp(p.TmpDir, "vips")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	cur := src
	for i, op := range m.Ops {
		next := filepath.Join(tmpDir, fmt.Sprintf("s%d.v", i))
		var args []string
		switch op {
		case "size":
			if m.ForceSize { // PictShare's forceResize: centre crop to the box, clamped to the image
				cw, ch := min(m.Size.W, w), min(m.Size.H, h)
				args = []string{"thumbnail", cur, next, strconv.Itoa(cw), "--height", strconv.Itoa(ch),
					"--crop", "centre", "--no-rotate", "--export-profile", "srgb"}
				w, h = cw, ch
			} else {
				nw, nh := ResizeDims(w, h, m.Size)
				args = []string{"thumbnail", cur, next, strconv.Itoa(nw), "--height", strconv.Itoa(nh),
					"--size", "force", "--no-rotate", "--export-profile", "srgb"}
				w, h = nw, nh
			}
		case "rotation":
			// GD's imagerotate() turns counter-clockwise, vips clockwise
			angle := map[string]string{"upside": "d180", "left": "d270", "right": "d90"}[m.Rotation]
			args = []string{"rot", cur, next, angle}
			if angle != "d180" {
				w, h = h, w
			}
		default:
			continue
		}
		if r := p.run(ctx, "vips", args...); r.Err != nil {
			return fmt.Errorf("vips %s: %v: %s", args[0], r.Err, r.Stderr)
		}
		if cur != src {
			os.Remove(cur)
		}
		cur = next
	}
	return p.save(ctx, cur, out, typ)
}
