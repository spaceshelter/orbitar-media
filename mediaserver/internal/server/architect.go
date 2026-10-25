package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/spaceshelter/orbitar-media/mediaserver/internal/s3store"
	"github.com/spaceshelter/orbitar-media/mediaserver/internal/store"
)

var (
	imageExts = []string{"png", "gif", "jpg", "jpeg", "x-png", "webp"}
	videoExts = []string{"mp4"}
	textExts  = []string{"txt", "text", "csv"}

	// what dimension hashes look like (34 base62 chars in production)
	dimsHashShape = regexp.MustCompile(`^[0-9A-Za-z]{30,}\.`)
)

func servedExt(ext string) bool {
	return slices.Contains(imageExts, ext) || slices.Contains(videoExts, ext) || slices.Contains(textExts, ext)
}

// segments splits a path like PictShare's array_filter(explode('/', $url)).
func segments(path string) []string {
	var out []string
	for _, s := range strings.Split(path, "/") {
		if s != "" && s != "0" { // array_filter drops "0" too
			out = append(out, s)
		}
	}
	return out
}

// architect is PictShare's URL dispatcher: the first path segment that names a stored file is
// the hash, everything else modifies how it is served, in any order.
func (s *Server) architect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		s.plainError(w, http.StatusNotFound, "404")
		return
	}
	segs := segments(r.URL.Path)
	hash := ""
	for _, seg := range segs {
		if s.store.Exists(seg) {
			hash = seg
			break
		}
		if s.servePreviewFromS3(w, r, seg, segs) {
			return
		}
		if s.restoreFromS3(r.Context(), seg) {
			hash = seg
			break
		}
	}
	if hash == "" {
		s.plainError(w, http.StatusNotFound, "404")
		return
	}

	for _, seg := range segs {
		if code, ok := strings.CutPrefix(seg, "delete_"); ok && s.deleteAllowed(r, hash, code) {
			if s.store.ReadOnly {
				s.plainError(w, http.StatusServiceUnavailable, "read-only")
				return
			}
			s.deleteHash(r.Context(), hash)
			w.Header().Set("Cache-Control", "no-store")
			s.writeBody(w, r, http.StatusOK, "text/html; charset=UTF-8", []byte(hash+" deleted successfully"))
			return
		}
	}

	s.store.Touch(hash)
	ext := store.Ext(hash)
	switch {
	case slices.Contains(imageExts, ext):
		s.handleImage(w, r, hash, segs)
	case slices.Contains(videoExts, ext):
		s.handleVideo(w, r, hash, segs)
	case slices.Contains(textExts, ext):
		s.handleText(w, r, hash, segs)
	default:
		s.plainError(w, http.StatusNotFound, "404")
	}
}

func (s *Server) deleteAllowed(r *http.Request, hash, code string) bool {
	if code == "" {
		return false
	}
	if own := s.store.DeleteCode(hash); own != "" && subtle.ConstantTimeCompare([]byte(own), []byte(code)) == 1 {
		return true
	}
	if m := s.cfg.MasterDeleteCode; m != "" && subtle.ConstantTimeCompare([]byte(m), []byte(code)) == 1 {
		return true
	}
	return s.cfg.MasterDeleteIP != "" && s.cfg.MasterDeleteIP == clientIP(r)
}

func (s *Server) deleteHash(ctx context.Context, hash string) {
	s.invalidated.add(hash) // before anything is removed: see restoreFromS3, servePreviewFromS3, uploadQueue.push
	if err := s.store.Delete(hash); err != nil {
		log.Printf("delete %s: %v", hash, err)
	}
	if s.s3 != nil {
		s.queue.forget(hash)
		if err := s.s3.Delete(ctx, hash); err != nil {
			log.Printf("delete %s from S3: %v", hash, err)
		}
		if store.Ext(hash) == "mp4" {
			s.deletePreviewFromS3(hash)
		}
		// a file or preview restored from S3 while that ran: removed here if it landed already,
		// and rejected by the newer mark if it lands later
		s.invalidated.add(hash)
		_ = s.store.Delete(hash)
	}
}

// s3Candidate: which names are looked up in S3. Dimension hashes must decrypt with our key;
// other names (random ones, custom ones, the coub thumbnails of an earlier system) are rate
// limited.
func (s *Server) s3Candidate(name string) bool {
	if !store.ValidName(name) || !servedExt(store.Ext(name)) {
		return false
	}
	if s.codec != nil {
		if _, err := s.codec.Decode(name); err == nil {
			return true
		}
		if dimsHashShape.MatchString(name) {
			return false
		}
	}
	return s.s3Lookups.allow()
}

// restoreFromS3 pulls name into the data directory when S3 has it.
func (s *Server) restoreFromS3(ctx context.Context, name string) bool {
	maxBytes := int64(max(s.cfg.MaxUploadMB, 1)) * 1_000_000 * 4 // originals are bounded by the upload limit
	start := time.Now()
	return s.fetchFromS3(ctx, name, maxBytes, func() bool { return s.s3Candidate(name) },
		func() bool { return s.store.Exists(name) },
		func(tmp string, meta map[string]string) error {
			// the uploader's delete code travels with the S3 copy (absent on copies made before it did)
			_, err := s.store.Put(tmp, name, meta[s3DeleteCode])
			if s.invalidated.since(name, start) { // deleted while it was downloading (Put may have published it and failed later)
				_ = s.store.Delete(name)
				return errors.New("deleted meanwhile")
			}
			if err != nil && err != store.ErrExists {
				return err
			}
			return nil
		})
}

// fetchFromS3 downloads key and hands the file and its S3 metadata to place. Concurrent requests for the same key
// share one download, and at most cap(restoreSlots) downloads run at a time. candidate decides
// whether key is worth a lookup at all, have whether it is already here.
func (s *Server) fetchFromS3(ctx context.Context, key string, maxBytes int64, candidate, have func() bool, place func(tmp string, meta map[string]string) error) bool {
	if s.s3 == nil || s.misses.has(key) {
		return false
	}
	s.restoreMu.Lock()
	if f, ok := s.restoring[key]; ok {
		s.restoreMu.Unlock()
		<-f.done
		return f.ok
	}
	if !candidate() {
		s.restoreMu.Unlock()
		return false
	}
	f := &flight{done: make(chan struct{})}
	s.restoring[key] = f
	s.restoreMu.Unlock()
	defer func() {
		s.restoreMu.Lock()
		delete(s.restoring, key)
		s.restoreMu.Unlock()
		close(f.done)
	}()

	if have() {
		f.ok = true
		return true
	}
	select {
	case s.restoreSlots <- struct{}{}:
	case <-ctx.Done():
		return false
	}
	defer func() { <-s.restoreSlots }()
	// detached from the request: a client hanging up must not leave half the work done
	dctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tmp := store.TempName(filepath.Join(s.cfg.TmpDir, "s3-"+strings.ReplaceAll(key, "/", "_")))
	meta, err := s.s3.Get(dctx, key, tmp, maxBytes)
	if err == s3store.ErrNotFound {
		s.misses.add(key)
		return false
	}
	if err != nil {
		log.Printf("s3 restore %s: %v", key, err)
		return false
	}
	defer os.Remove(tmp)
	if err := place(tmp, meta); err != nil {
		log.Printf("s3 restore %s: %v", key, err)
		return false
	}
	f.ok = true
	return true
}

// flight is one S3 download that later requests for the same key wait for.
type flight struct {
	done chan struct{}
	ok   bool
}
