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
	"time"

	"github.com/spaceshelter/orbitar-media/mediaserver/internal/store"
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
	size := sizeSeg(segs)
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
		name, box, ok := previewName(hash, size)
		if !ok {
			s.plainError(w, http.StatusNotFound, "unsupported preview size")
			return
		}
		target := s.store.VariantPath(hash, name)
		if !fileExists(target) {
			if p := s.store.PreviewPath(hash, name); fileExists(p) { // restored from S3 before the video was
				s.serveFile(w, r, p, "image/jpeg")
				return
			}
			rendered := false
			if err := s.generate(r.Context(), target, func(ctx context.Context, tmp string) error {
				rendered = true
				return s.vid.FirstFrame(ctx, path, tmp, box)
			}); err != nil {
				log.Printf("preview %s: %v", target, err)
				s.plainError(w, http.StatusServiceUnavailable, "could not render preview")
				return
			}
			if rendered {
				s.uploadPreview(hash, name, target)
			}
		}
		s.serveFile(w, r, target, "image/jpeg")
	case slices.Contains(segs, "download"):
		s.serveDownload(w, r, path, hash)
	default:
		s.videoPage(w, r, hash, strings.Join(segs, "/"), path)
	}
}

// sizeSeg is the (last) size segment of a URL, "" for none.
func sizeSeg(segs []string) string {
	size := ""
	for _, seg := range segs {
		if _, ok := parseSize(seg); ok || isPHPSize(seg) {
			size = seg
		}
	}
	return size
}

// previewName is the cached file name of a video preview and the size to render it at (nil: the
// video's own); false for sizes that aren't offered.
func previewName(hash, size string) (string, *video.Box, bool) {
	if size == "" {
		return hash + "_preview.jpg", nil, true
	}
	sz, ok := parseSize(size)
	if !ok || sz.W > 4096 || sz.H > 4096 {
		return "", nil, false
	}
	// the file name PictShare's old resize path used, so existing previews are reused
	return strconv.Itoa(sz.W) + "_" + hash + "_preview.jpg", &video.Box{W: sz.W, H: sz.H, Box: strings.Contains(size, "x")}, true
}

// Previews are kept in S3 too, under previews/<hash>/<name>, so that a page of old videos whose
// files were evicted gets its previews without downloading every video and rendering a frame.
func previewKeyPrefix(hash string) string { return "previews/" + hash + "/" }

const maxPreviewBytes = 20_000_000

// servePreviewFromS3 answers a preview URL of a video that isn't stored here with the preview's
// S3 copy (or one restored earlier). false: not such a URL, or S3 doesn't have the preview; the
// video is then restored and the preview rendered as usual.
func (s *Server) servePreviewFromS3(w http.ResponseWriter, r *http.Request, hash string, segs []string) bool {
	if !store.ValidName(hash) || store.Ext(hash) != "mp4" || !slices.Contains(segs, "preview") {
		return false
	}
	for _, seg := range segs {
		if strings.HasPrefix(seg, "delete_") {
			return false
		}
	}
	name, _, ok := previewName(hash, sizeSeg(segs))
	if !ok {
		return false
	}
	p := s.store.PreviewPath(hash, name)
	if !fileExists(p) && !s.fetchFromS3(r.Context(), previewKeyPrefix(hash)+name, maxPreviewBytes,
		func() bool { return s.s3Candidate(hash) },
		func() bool { return fileExists(s.store.PreviewPath(hash, name)) },
		func(tmp string) error { return s.store.PutPreview(tmp, hash, name) }) {
		return false
	}
	s.serveFile(w, r, p, "image/jpeg")
	return true
}

type previewPut struct{ hash, name, path string }

// uploadPreview copies a freshly rendered preview to S3 in the background. Best effort: a preview
// that doesn't make it is rendered again the next time it's needed.
func (s *Server) uploadPreview(hash, name, path string) {
	if s.s3 == nil || s.store.ReadOnly {
		return
	}
	select {
	case s.previewPuts <- previewPut{hash, name, path}:
	default:
	}
}

func (s *Server) runPreviewUploads() {
	defer s.bgWG.Done()
	for {
		select {
		case p := <-s.previewPuts:
			if !s.store.Exists(p.hash) { // deleted meanwhile
				continue
			}
			ctx, cancel := context.WithTimeout(s.bgCtx, time.Minute)
			if err := s.s3.Put(ctx, previewKeyPrefix(p.hash)+p.name, p.path); err != nil && !os.IsNotExist(err) {
				log.Printf("s3: upload of preview %s: %v", p.path, err)
			}
			cancel()
		case <-s.bgCtx.Done():
			return
		}
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
