package server

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// uploadQueue copies stored files to S3 in the background. A marker file per pending hash
// (data/.s3-pending/<hash>, hidden from the cleanup service's `ls`) survives restarts and is
// swept at startup; failures are retried with backoff. One worker, so pushes of the same hash
// (the original, then its transcode) can't overtake each other.
type uploadQueue struct {
	s       *Server
	dir     string
	mu      sync.Mutex
	pending map[string]int // hash -> generation (bumped by every push)
	wake    chan struct{}
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
	if err := os.MkdirAll(q.dir, 0o755); err == nil {
		_ = os.WriteFile(filepath.Join(q.dir, hash), nil, 0o644)
	}
	q.mu.Lock()
	q.pending[hash]++
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// forget drops a pending upload (the file was deleted).
func (q *uploadQueue) forget(hash string) {
	q.mu.Lock()
	delete(q.pending, hash)
	q.mu.Unlock()
	os.Remove(filepath.Join(q.dir, hash))
}

func (q *uploadQueue) start() {
	if !q.enabled() {
		return
	}
	if entries, err := os.ReadDir(q.dir); err == nil {
		for _, e := range entries {
			if !e.IsDir() && !isTempName(e.Name()) {
				q.pending[e.Name()]++
			}
		}
		if len(entries) > 0 {
			log.Printf("s3: %d uploads pending from an earlier run", len(entries))
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
		switch {
		case err == nil || os.IsNotExist(err):
			if os.IsNotExist(err) {
				log.Printf("s3: %s is gone locally, dropping it from the queue", hash)
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
		if q.s.bgCtx.Err() != nil {
			return
		}
	}
}

func (q *uploadQueue) upload(hash string) error {
	path := q.s.store.Path(hash)
	if _, err := os.Stat(path); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(q.s.bgCtx, 10*time.Minute)
	defer cancel()
	return q.s.s3.Put(ctx, hash, path)
}
