package server

import (
	"context"
	"crypto/subtle"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/spaceshelter/orbitar-media/mediaserver/internal/s3store"
	"github.com/spaceshelter/orbitar-media/mediaserver/internal/store"
)

var (
	imageExts = []string{"png", "gif", "jpg", "jpeg", "x-png", "webp"}
	videoExts = []string{"mp4"}
	textExts  = []string{"txt", "text", "csv"}

	// names worth asking S3 about besides dimension hashes: random names (text, setups without
	// a key) and the coub thumbnails of an earlier system
	randomNameRe = regexp.MustCompile(`^[0-9a-z]{6,12}\.[a-z]{3,4}$`)
	coubNameRe   = regexp.MustCompile(`^coub_\w{1,40}\.jpg$`)
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
	if err := s.store.Delete(hash); err != nil {
		log.Printf("delete %s: %v", hash, err)
	}
	if s.s3 != nil {
		s.queue.forget(hash)
		if err := s.s3.Delete(ctx, hash); err != nil {
			log.Printf("delete %s from S3: %v", hash, err)
		}
	}
}

// s3Candidate: only names this service could have produced are looked up in S3. Dimension
// hashes must decrypt with our key; other shapes are rate limited.
func (s *Server) s3Candidate(name string) bool {
	if !store.ValidName(name) || !servedExt(store.Ext(name)) {
		return false
	}
	if s.codec != nil {
		if _, err := s.codec.Decode(name); err == nil {
			return true
		}
	}
	if randomNameRe.MatchString(name) || coubNameRe.MatchString(name) {
		return s.s3Lookups.allow()
	}
	return false
}

// restoreFromS3 pulls name into the data directory when S3 has it. Concurrent requests for the
// same name share one download.
func (s *Server) restoreFromS3(ctx context.Context, name string) bool {
	if s.s3 == nil || s.misses.has(name) || !s.s3Candidate(name) {
		return false
	}
	s.restoreMu.Lock()
	if wg, ok := s.restoring[name]; ok {
		s.restoreMu.Unlock()
		wg.Wait()
		return s.store.Exists(name)
	}
	wg := &sync.WaitGroup{}
	wg.Add(1)
	s.restoring[name] = wg
	s.restoreMu.Unlock()
	defer func() {
		s.restoreMu.Lock()
		delete(s.restoring, name)
		s.restoreMu.Unlock()
		wg.Done()
	}()

	if s.store.Exists(name) {
		return true
	}
	// detached from the request: a client hanging up must not leave half the work done
	dctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tmp := store.TempName(filepath.Join(s.cfg.TmpDir, "s3-"+name))
	maxBytes := int64(max(s.cfg.MaxUploadMB, 1)) * 1_000_000 * 4 // originals are bounded by the upload limit
	err := s.s3.Get(dctx, name, tmp, maxBytes)
	if err == s3store.ErrNotFound {
		s.misses.add(name)
		return false
	}
	if err != nil {
		log.Printf("s3 restore %s: %v", name, err)
		return false
	}
	defer os.Remove(tmp)
	if _, err := s.store.Put(tmp, name); err != nil && err != store.ErrExists {
		log.Printf("s3 restore %s: %v", name, err)
		return false
	}
	return true
}
