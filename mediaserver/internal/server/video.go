package server

import (
	"bytes"
	"context"
	"errors"
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
			rendered := false
			if err := s.generate(r.Context(), target, func(ctx context.Context, tmp string) error {
				rendered = true
				return s.vid.FirstFrame(ctx, path, tmp, box)
			}); err != nil {
				log.Printf("preview %s: %v", target, err)
				s.plainError(w, http.StatusServiceUnavailable, "could not render preview")
				return
			}
			if rendered && box == nil {
				s.uploadPreview(hash, target)
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

// The full-size preview (the only one Orbitar uses) is kept in S3 too, so that a page of old
// videos whose files were evicted gets its previews without downloading every video and
// rendering a frame. Sized previews are only rendered: any width is accepted, and each would be
// another object.
func previewKey(hash string) string { return "previews/" + hash + "_preview.jpg" }

const maxPreviewBytes = 20_000_000

// servePreviewFromS3 answers the preview URL of a video that isn't stored here with the preview's
// S3 copy (or one restored earlier). false: not such a URL, or S3 doesn't have the preview; the
// video is then restored and the preview rendered as usual.
func (s *Server) servePreviewFromS3(w http.ResponseWriter, r *http.Request, hash string, segs []string) bool {
	// "raw" wins over "preview" in handleVideo: the same URL must not change type with eviction
	if !store.ValidName(hash) || store.Ext(hash) != "mp4" || !slices.Contains(segs, "preview") ||
		slices.Contains(segs, "raw") || sizeSeg(segs) != "" {
		return false
	}
	for _, seg := range segs {
		if strings.HasPrefix(seg, "delete_") {
			return false
		}
	}
	name, _, _ := previewName(hash, "")
	have := func() bool { return s.store.FindVariant(hash, name) != "" }
	start := time.Now()
	if !have() && !s.fetchFromS3(r.Context(), previewKey(hash), maxPreviewBytes,
		func() bool { return s.s3Candidate(hash) }, have,
		func(tmp string) error {
			dst, err := s.store.PutVariant(tmp, hash, name)
			if err != nil {
				return err
			}
			if s.invalidated.since(hash, start) { // deleted, or its name reused, while it was downloading
				_ = os.Remove(dst)
				return errors.New("invalidated meanwhile")
			}
			return nil
		}) {
		return false
	}
	p := s.store.FindVariant(hash, name)
	if p == "" {
		return false
	}
	s.store.Touch(hash)
	s.serveFile(w, r, p, "image/jpeg")
	return true
}

// reuseName prepares a custom name for a new upload: what may be left of an earlier, evicted file
// under it (its variants here, its preview in S3) belongs to that one, and an earlier preview
// being restored right now must not land afterwards.
func (s *Server) reuseName(hash string) {
	s.invalidated.add(hash)
	if err := s.store.DropVariants(hash); err != nil {
		log.Printf("drop variants of %s: %v", hash, err)
	}
	if store.Ext(hash) == "mp4" && s.s3 != nil && !s.store.ReadOnly {
		// through the upload worker, so it can't overtake the new video's preview upload
		select {
		case s.previewPuts <- previewPut{hash: hash}:
		default:
			s.bgWG.Add(1)
			go func() {
				defer s.bgWG.Done()
				s.deletePreviewFromS3(hash)
			}()
		}
	}
}

func (s *Server) deletePreviewFromS3(hash string) {
	ctx, cancel := context.WithTimeout(s.bgCtx, time.Minute)
	defer cancel()
	if err := s.s3.Delete(ctx, previewKey(hash)); err != nil {
		log.Printf("s3: delete preview of %s: %v", hash, err)
	}
}

type previewPut struct{ hash, path string } // path "": delete hash's preview

// uploadPreview copies a freshly rendered preview to S3 in the background. Best effort: a preview
// that doesn't make it is rendered again the next time it's needed.
func (s *Server) uploadPreview(hash, path string) {
	if s.s3 == nil || s.store.ReadOnly {
		return
	}
	select {
	case s.previewPuts <- previewPut{hash, path}:
	default:
	}
}

func (s *Server) runPreviewUploads() {
	defer s.bgWG.Done()
	for {
		select {
		case p := <-s.previewPuts:
			if p.path == "" {
				s.deletePreviewFromS3(p.hash)
				continue
			}
			start := time.Now()
			if !s.store.Exists(p.hash) {
				continue
			}
			ctx, cancel := context.WithTimeout(s.bgCtx, time.Minute)
			err := s.s3.Put(ctx, previewKey(p.hash), p.path)
			cancel()
			if err != nil && !os.IsNotExist(err) {
				log.Printf("s3: upload of preview %s: %v", p.path, err)
			}
			if s.invalidated.since(p.hash, start) { // deleted while it was uploading (a failed PUT may have landed too)
				s.deletePreviewFromS3(p.hash)
			}
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
