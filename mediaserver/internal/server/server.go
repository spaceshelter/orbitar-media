// Package server is the HTTP side: PictShare's URL grammar, the upload API and the
// coub/vimeo thumbnail redirects.
package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/subtle"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spaceshelter/orbitar-media/mediaserver/internal/hashcodec"
	"github.com/spaceshelter/orbitar-media/mediaserver/internal/imaging"
	"github.com/spaceshelter/orbitar-media/mediaserver/internal/s3store"
	"github.com/spaceshelter/orbitar-media/mediaserver/internal/store"
	"github.com/spaceshelter/orbitar-media/mediaserver/internal/video"
)

// Config is the service configuration (environment variables, see cmd/mediaserver).
type Config struct {
	URL              string // prefix of returned URLs ("" = bare hash, as in production)
	Title            string
	ClientID         string
	MaxUploadMB      int
	MasterDeleteCode string
	MasterDeleteIP   string
	CoubProxyURL     string
	UploadImages     bool // CONTENTCONTROLLERS: which types /api/upload.php accepts
	UploadVideos     bool
	UploadText       bool
	StaticDir        string // PictShare's css/js/fonts for the text page
	TmpDir           string
	VideoSlots       int // MAX_CONCURRENT_VIDEO_HANDLERS
	ImageSlots       int // concurrent vips/preview runs
	// AllowPrivateFetch lets geturl fetch from private addresses (tests only).
	AllowPrivateFetch bool
}

// Server handles every request that Caddy forwards.
type Server struct {
	cfg    Config
	store  *store.Store
	s3     *s3store.Client
	codec  *hashcodec.Codec // nil: random names
	img    *imaging.Processor
	vid    *video.Tools
	queue  *uploadQueue
	fetch  *http.Client // geturl
	api    *http.Client // coub, vimeo
	logger *log.Logger

	videoSlots   chan struct{}
	imageSlots   chan struct{}
	probeSlots   chan struct{} // ffprobe / file runs on uploads
	textSlots    chan struct{} // text pages rendered in memory
	restoreSlots chan struct{} // S3 downloads

	previewPuts chan previewPut // previews on their way to S3

	restoreMu  sync.Mutex
	restoring  map[string]*flight
	misses     *missCache
	deleted    *missCache // hashes deleted lately, so a preview download or upload in flight can't bring one back
	s3Lookups  *rateLimiter
	generating keyedMutex
	bgWG       sync.WaitGroup
	bgCtx      context.Context
	bgCancel   context.CancelFunc
}

// New wires a server. s3 may be nil (no S3), codec may be nil (random names).
func New(cfg Config, st *store.Store, s3 *s3store.Client, codec *hashcodec.Codec, img *imaging.Processor, vid *video.Tools) *Server {
	if cfg.VideoSlots < 1 {
		cfg.VideoSlots = 1
	}
	if cfg.ImageSlots < 1 {
		cfg.ImageSlots = 1
	}
	if cfg.Title == "" {
		cfg.Title = "PictShare"
	}
	s := &Server{
		cfg: cfg, store: st, s3: s3, codec: codec, img: img, vid: vid,
		fetch:        newFetchClient(cfg.AllowPrivateFetch),
		api:          &http.Client{Timeout: 10 * time.Second},
		logger:       log.New(os.Stdout, "", 0),
		videoSlots:   make(chan struct{}, cfg.VideoSlots),
		imageSlots:   make(chan struct{}, cfg.ImageSlots),
		probeSlots:   make(chan struct{}, 2*cfg.VideoSlots),
		textSlots:    make(chan struct{}, 2),
		restoreSlots: make(chan struct{}, 8),
		previewPuts:  make(chan previewPut, 64),
		restoring:    map[string]*flight{},
		misses:       newMissCache(20000, 10*time.Minute),
		deleted:      newMissCache(20000, 15*time.Minute), // longer than a download (5 min) or upload (1 min) runs
		s3Lookups:    newRateLimiter(10, 20),
	}
	s.bgCtx, s.bgCancel = context.WithCancel(context.Background())
	s.queue = newUploadQueue(s)
	return s
}

// Start launches the background S3 uploaders (including files left pending by an earlier run).
func (s *Server) Start() {
	s.queue.start()
	if s.queue.enabled() {
		s.bgWG.Add(1)
		go s.runPreviewUploads()
	}
}

// Shutdown stops background work, waiting up to ctx for running jobs.
func (s *Server) Shutdown(ctx context.Context) {
	s.bgCancel()
	done := make(chan struct{})
	go func() { s.bgWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

var (
	coubRe  = regexp.MustCompile(`^/coub/(\w+)$`)
	vimeoRe = regexp.MustCompile(`^/vimeo/(\w+)$`)
)

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rw := &respWriter{ResponseWriter: w, status: 200}
	h := rw.Header()
	// what nginx added to every PictShare response
	h.Set("X-Frame-Options", "SAMEORIGIN")
	h.Set("X-XSS-Protection", "1; mode=block")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer-when-downgrade")
	h.Set("Content-Security-Policy", "default-src * data: 'unsafe-eval' 'unsafe-inline'")
	h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains; preload")
	defer func() {
		if v := recover(); v != nil {
			if v == http.ErrAbortHandler {
				panic(v)
			}
			log.Printf("panic serving %s: %v", r.URL.Path, v)
			if !rw.wrote {
				s.plainError(rw, http.StatusInternalServerError, "internal error")
			}
		}
		s.logger.Printf("%s %s %q %d %d %s %q", clientIP(r), r.Method, r.URL.RequestURI(), rw.status, rw.bytes,
			time.Since(start).Round(time.Millisecond), r.UserAgent())
	}()
	s.route(rw, r)
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if strings.HasPrefix(p, "/api/") && !s.authorized(r) {
		// Caddy enforces this too; checked here so the service is safe behind any proxy
		s.writeJSONStatus(w, r, http.StatusForbidden, kv{"status", "err"}, kv{"reason", "Access denied"})
		return
	}
	switch p {
	case "/api/upload.php":
		s.handleUpload(w, r)
		return
	case "/api/geturl.php":
		s.handleGetURL(w, r)
		return
	case "/api/info.php":
		s.handleInfo(w, r)
		return
	case "/healthz":
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte("ok"))
		return
	}
	if m := coubRe.FindStringSubmatch(p); m != nil {
		s.handleCoub(w, r, m[1])
		return
	}
	if m := vimeoRe.FindStringSubmatch(p); m != nil {
		s.handleVimeo(w, r, m[1])
		return
	}
	if s.serveStatic(w, r) {
		return
	}
	s.architect(w, r)
}

// authorized checks "Authorization: Client-ID <CLIENT_ID>" (always true without a CLIENT_ID).
func (s *Server) authorized(r *http.Request) bool {
	if s.cfg.ClientID == "" {
		return true
	}
	want := "Client-ID " + s.cfg.ClientID
	return subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(want)) == 1
}

// serveStatic serves PictShare's css/js/fonts, which the text page links to.
func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.StaticDir == "" {
		return false
	}
	p := r.URL.Path
	if !strings.HasPrefix(p, "/css/") && !strings.HasPrefix(p, "/js/") && !strings.HasPrefix(p, "/fonts/") {
		return false
	}
	clean := filepath.Clean("/" + p)
	full := filepath.Join(s.cfg.StaticDir, clean)
	st, err := os.Stat(full)
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	w.Header().Set("Cache-Control", "public, max-age=604800")
	http.ServeFile(w, r, full)
	return true
}

// respWriter records the status for the access log and turns every error response into
// no-store, so a CDN never keeps an error for as long as a file.
type respWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func (w *respWriter) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.status = code
	if code >= 400 {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Del("ETag")
		w.Header().Del("Last-Modified")
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *respWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *respWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// plainError is PictShare's die('...') with a status: text/html, no-store.
func (s *Server) plainError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=UTF-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(msg))
}

// kv is an ordered JSON object (PHP arrays keep insertion order; Orbitar doesn't care, but
// diffs against the PHP service are easier to read).
type kv struct {
	k string
	v any
}

func marshalOrdered(fields []kv) []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range fields {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(jsonValue(f.k))
		b.WriteByte(':')
		b.Write(jsonValue(f.v))
	}
	b.WriteByte('}')
	return b.Bytes()
}

// jsonValue encodes like PHP's json_encode: "/" escaped, no HTML escaping.
func jsonValue(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.ReplaceAll(bytes.TrimRight(b.Bytes(), "\n"), []byte("/"), []byte(`\/`))
}

// writeJSON sends a JSON body with status 200, like the PHP API (errors are status "err" in the
// body, not HTTP errors).
func (s *Server) writeJSON(w http.ResponseWriter, r *http.Request, fields ...kv) {
	s.writeJSONStatus(w, r, http.StatusOK, fields...)
}

func (s *Server) writeJSONStatus(w http.ResponseWriter, r *http.Request, code int, fields ...kv) {
	w.Header().Set("Cache-Control", "no-store")
	s.writeBody(w, r, code, "application/json", marshalOrdered(fields))
}

// writeBody sends a generated body, gzipped when the client accepts it (nginx did that for
// PictShare's text responses).
func (s *Server) writeBody(w http.ResponseWriter, r *http.Request, code int, contentType string, body []byte) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Add("Vary", "Accept-Encoding")
	if len(body) > 256 && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		var z bytes.Buffer
		gw, _ := gzip.NewWriterLevel(&z, 6)
		_, _ = gw.Write(body)
		_ = gw.Close()
		h.Set("Content-Encoding", "gzip")
		body = z.Bytes()
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(code)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// clientIP is PictShare's getUserIP() behind Caddy: the first X-Forwarded-For entry, then
// Client-IP, then the peer address.
func clientIP(r *http.Request) string {
	if f := r.Header.Get("X-Forwarded-For"); f != "" {
		first := strings.TrimSpace(strings.Split(f, ",")[0])
		if net.ParseIP(first) != nil {
			return first
		}
	}
	if c := strings.TrimSpace(r.Header.Get("Client-IP")); net.ParseIP(c) != nil {
		return c
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// keyedMutex serialises work per key (one generator per variant file).
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*keyedEntry
}

type keyedEntry struct {
	sync.Mutex
	refs int
}

func (k *keyedMutex) Lock(key string) func() {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*keyedEntry{}
	}
	e := k.locks[key]
	if e == nil {
		e = &keyedEntry{}
		k.locks[key] = e
	}
	e.refs++
	k.mu.Unlock()
	e.Lock()
	return func() {
		e.Unlock()
		k.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}

// missCache remembers names S3 doesn't have, for a while.
type missCache struct {
	mu   sync.Mutex
	max  int
	ttl  time.Duration
	seen map[string]time.Time
}

func newMissCache(max int, ttl time.Duration) *missCache {
	return &missCache{max: max, ttl: ttl, seen: map[string]time.Time{}}
}

func (m *missCache) has(k string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.seen[k]
	if ok && time.Since(t) > m.ttl {
		delete(m.seen, k)
		return false
	}
	return ok
}

func (m *missCache) add(k string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.seen) >= m.max {
		m.seen = map[string]time.Time{} // crude, but bounded
	}
	m.seen[k] = time.Now()
}

// rateLimiter is a token bucket.
type rateLimiter struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
}

func newRateLimiter(perSecond, burst float64) *rateLimiter {
	return &rateLimiter{rate: perSecond, burst: burst, tokens: burst, last: time.Now()}
}

func (l *rateLimiter) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.tokens = min(l.burst, l.tokens+now.Sub(l.last).Seconds()*l.rate)
	l.last = now
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}
