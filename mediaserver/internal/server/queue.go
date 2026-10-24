package server

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/spaceshelter/orbitar-media/mediaserver/internal/store"
)

// uploadQueue copies stored files to S3 in the background. A marker per pending hash
// (data/.s3-pending/<hash>, hidden from the cleanup service's `ls`) survives restarts and is
// swept at startup; failures are retried with backoff. The marker is a hard link to the stored
// file, so the copy still reaches S3 when the cleanup service evicts the hash directory first.
// One worker, so pushes of the same hash (the original, then its transcode) can't overtake
// each other.
type uploadQueue struct {
	s       *Server
	dir     string
	mu      sync.Mutex
	pending map[string]int // hash -> generation (bumped by every push)
	wake    chan struct{}

	inflight        string // hash the worker is uploading
	inflightDeleted bool   // inflight was deleted meanwhile: remove the copy once it lands
}

func newUploadQueue(s *Server) *uploadQueue {
	return &uploadQueue{s: s, dir: filepath.Join(s.store.Root, ".s3-pending"), pending: map[string]int{}, wake: make(chan struct{}, 1)}
}

func (q *uploadQueue) enabled() bool { return q.s.s3 != nil && !q.s.store.ReadOnly }

// push schedules hash for upload.
func (q *uploadQueue) push(hash string) {
	if !q.enabled() {
		return
	}
	q.mu.Lock()
	q.writeMarker(hash)
	q.pending[hash]++
	if q.inflight == hash {
		q.inflightDeleted = false
	}
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// writeMarker links the stored file into the marker directory (an empty marker when linking
// fails). Called with q.mu held, so the worker can't remove a marker it hasn't seen.
func (q *uploadQueue) writeMarker(hash string) {
	if err := os.MkdirAll(q.dir, 0o755); err != nil {
		return
	}
	marker := filepath.Join(q.dir, hash)
	tmp := store.TempName(marker)
	if err := os.Link(q.s.store.Path(hash), tmp); err != nil {
		if err := os.WriteFile(tmp, nil, 0o644); err != nil {
			return
		}
	}
	if err := os.Rename(tmp, marker); err != nil {
		os.Remove(tmp)
	}
}

// forget drops a pending upload (the file was deleted).
func (q *uploadQueue) forget(hash string) {
	q.mu.Lock()
	delete(q.pending, hash)
	os.Remove(filepath.Join(q.dir, hash))
	if q.inflight == hash {
		q.inflightDeleted = true
	}
	q.mu.Unlock()
}

func (q *uploadQueue) start() {
	if !q.enabled() {
		return
	}
	if entries, err := os.ReadDir(q.dir); err == nil {
		for _, e := range entries {
			switch {
			case e.IsDir():
			case isTempName(e.Name()): // a marker link interrupted by a crash
				os.Remove(filepath.Join(q.dir, e.Name()))
			default:
				q.pending[e.Name()]++
			}
		}
		if len(q.pending) > 0 {
			log.Printf("s3: %d uploads pending from an earlier run", len(q.pending))
		}
	}
	q.s.bgWG.Add(1)
	go q.run()
}

func isTempName(n string) bool { return filepath.Ext(n) == ".tmp" }

func (q *uploadQueue) run() {
	defer q.s.bgWG.Done()
	backoff := map[string]time.Time{} // hash -> not before
	attempts := map[string]int{}
	for {
		q.mu.Lock()
		var hash string
		var gen int
		var nextRetry time.Time
		now := time.Now()
		for h, g := range q.pending {
			if t, ok := backoff[h]; ok && now.Before(t) {
				if nextRetry.IsZero() || t.Before(nextRetry) {
					nextRetry = t
				}
				continue
			}
			hash, gen = h, g
			break
		}
		q.inflight, q.inflightDeleted = hash, false
		q.mu.Unlock()

		if hash == "" {
			wait := time.Hour
			if !nextRetry.IsZero() {
				wait = time.Until(nextRetry)
			}
			select {
			case <-q.wake:
			case <-time.After(wait):
			case <-q.s.bgCtx.Done():
				return
			}
			continue
		}

		err := q.upload(hash)
		q.mu.Lock()
		deleted := q.inflightDeleted
		q.inflight, q.inflightDeleted = "", false
		switch {
		case err == nil || os.IsNotExist(err):
			if os.IsNotExist(err) {
				log.Printf("s3: %s is gone locally, dropping it from the queue", hash)
			} else if q.deletedElsewhere(hash) {
				deleted = true
			}
			delete(backoff, hash)
			delete(attempts, hash)
			if q.pending[hash] == gen { // not pushed again meanwhile
				delete(q.pending, hash)
				os.Remove(filepath.Join(q.dir, hash))
			}
		default:
			attempts[hash]++
			d := min(time.Minute<<min(attempts[hash]-1, 8), 6*time.Hour)
			backoff[hash] = time.Now().Add(d)
			log.Printf("s3: upload of %s failed (attempt %d, retry in %s): %v", hash, attempts[hash], d, err)
		}
		q.mu.Unlock()
		if deleted && err == nil {
			// deleted while it was being copied: the delete may have reached S3 first
			if derr := q.s.s3.Delete(q.s.bgCtx, hash); derr != nil {
				log.Printf("s3: delete of %s after its upload: %v", hash, derr)
			}
		}
		if q.s.bgCtx.Err() != nil {
			return
		}
	}
}

// deletedElsewhere reports a delete by PictShare, which shares the data directory: it removes
// the hash directory and the marker but can't flag the upload in flight.
func (q *uploadQueue) deletedElsewhere(hash string) bool {
	_, merr := os.Stat(filepath.Join(q.dir, hash))
	_, ferr := os.Stat(q.s.store.Path(hash))
	return os.IsNotExist(merr) && os.IsNotExist(ferr)
}

// upload copies hash from its marker, or from the data directory when the marker is an empty
// one.
func (q *uploadQueue) upload(hash string) error {
	path := filepath.Join(q.dir, hash)
	if st, err := os.Stat(path); err != nil || st.Size() == 0 {
		path = q.s.store.Path(hash)
	}
	if _, err := os.Stat(path); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(q.s.bgCtx, 10*time.Minute)
	defer cancel()
	return q.s.s3.Put(ctx, hash, path)
}
