package server

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/spaceshelter/orbitar-media/mediaserver/internal/video"
)

func (s *Server) tryVideoSlot() bool {
	select {
	case s.videoSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Server) releaseVideoSlot() { <-s.videoSlots }

func (s *Server) handleVideo(w http.ResponseWriter, r *http.Request, hash string, segs []string) {
	path := s.store.Path(hash)
	size := ""
	for _, seg := range segs {
		if _, ok := parseSize(seg); ok || isPHPSize(seg) {
			size = seg
		}
	}
	preview := slices.Contains(segs, "preview")
	// resized copies of the video itself are not offered; sized previews are
	if size != "" && !preview {
		s.plainError(w, http.StatusNotFound, "video size variants are not available")
		return
	}
	switch {
	case slices.Contains(segs, "raw"):
		s.serveFile(w, r, path, "video/mp4")
	case preview:
		var box *video.Box
		name := hash + "_preview.jpg"
		if size != "" {
			sz, ok := parseSize(size)
			if !ok || sz.W > 4096 || sz.H > 4096 {
				s.plainError(w, http.StatusNotFound, "unsupported preview size")
				return
			}
			box = &video.Box{W: sz.W, H: sz.H, Box: strings.Contains(size, "x")}
			// the file name PictShare's old resize path used, so existing previews are reused
			name = strconv.Itoa(sz.W) + "_" + hash + "_preview.jpg"
		}
		target := s.store.VariantPath(hash, name)
		if !fileExists(target) {
			if err := s.generate(r.Context(), target, func(ctx context.Context, tmp string) error {
				return s.vid.FirstFrame(ctx, path, tmp, box)
			}); err != nil {
				log.Printf("preview %s: %v", target, err)
				s.plainError(w, http.StatusServiceUnavailable, "could not render preview")
				return
			}
		}
		s.serveFile(w, r, target, "image/jpeg")
	case slices.Contains(segs, "download"):
		s.serveDownload(w, r, path, hash)
	default:
		s.videoPage(w, r, hash, strings.Join(segs, "/"), path)
	}
}

// isPHPSize catches segments PHP's isSize() treated as a size that parseSize rejects (0, huge
// numbers), so they still get the 404 instead of the full video.
func isPHPSize(seg string) bool {
	a, b, found := strings.Cut(seg, "x")
	digits := func(s string) bool {
		return s != "" && strings.Trim(s, "0123456789") == ""
	}
	if !found {
		return digits(a)
	}
	return digits(a) && digits(b)
}

func (s *Server) videoPage(w http.ResponseWriter, r *http.Request, hash, url, path string) {
	var size int64
	if st, err := os.Stat(path); err == nil {
		size = st.Size()
	}
	var b bytes.Buffer
	if err := videoTemplate.Execute(&b, map[string]any{
		"URL": s.cfg.URL, "Title": s.cfg.Title, "Hash": hash, "Url": url, "Filesize": renderSize(size),
	}); err != nil {
		s.plainError(w, http.StatusInternalServerError, "template error")
		return
	}
	s.writeBody(w, r, http.StatusOK, "text/html; charset=UTF-8", b.Bytes())
}

// renderSize is PictShare's renderSize(): "1.5 MB", "300 Byte".
func renderSize(n int64) string {
	units := []string{"Byte", "KB", "MB", "GB", "TB", "PB", "EB"}
	f := float64(n)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	f = float64(int64(f*100+0.5)) / 100
	return strconv.FormatFloat(f, 'f', -1, 64) + " " + units[i]
}
