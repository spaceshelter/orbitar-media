package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spaceshelter/orbitar-media/mediaserver/internal/hashcodec"
	"github.com/spaceshelter/orbitar-media/mediaserver/internal/imaging"
	"github.com/spaceshelter/orbitar-media/mediaserver/internal/s3store"
	"github.com/spaceshelter/orbitar-media/mediaserver/internal/store"
	"github.com/spaceshelter/orbitar-media/mediaserver/internal/video"
)

// fakeS3 is a path-style bucket in memory.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	meta    map[string]http.Header // x-amz-meta-* headers of each PUT
	gets    int
	puts    int
	deletes int
	down    bool             // every request fails
	putHook func(key string) // runs before a PUT is stored, outside the lock
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/media/")
	if r.Method == http.MethodPut && f.putHook != nil {
		f.putHook(key)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		f.objects[key] = b
		f.puts++
		if f.meta == nil {
			f.meta = map[string]http.Header{}
		}
		f.meta[key] = http.Header{}
		for k, v := range r.Header {
			if strings.HasPrefix(strings.ToLower(k), "x-amz-meta-") {
				f.meta[key][k] = v
			}
		}
	case http.MethodGet, http.MethodHead:
		f.gets++
		b, ok := f.objects[key]
		if !ok {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `<?xml version="1.0"?><Error><Code>NoSuchKey</Code></Error>`)
			return
		}
		for k, v := range f.meta[key] {
			w.Header()[k] = v
		}
		w.Header().Set("Content-Length", itoa(len(b)))
		if r.Method == http.MethodGet {
			_, _ = w.Write(b)
		}
	case http.MethodDelete:
		f.deletes++
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	}
}

// deleteCode is the delete code stored with key ("" when none).
func (f *fakeS3) deleteCode(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.meta[key].Get("X-Amz-Meta-Deletecode")
}

func (f *fakeS3) get(key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.objects[key]
	return b, ok
}

func newS3Server(t *testing.T, data string, fake *fakeS3, endpoint string) *harness {
	t.Helper()
	needTools(t, "file", "vips", "ffmpeg", "ffprobe")
	tmp := t.TempDir()
	codec, _ := hashcodec.New(testKey)
	s := New(Config{ClientID: "secret", MaxUploadMB: 5, TmpDir: tmp, UploadImages: true, UploadVideos: true, VideoSlots: 1, ImageSlots: 1},
		&store.Store{Root: data}, s3store.New(s3store.Config{Bucket: "media", AccessKey: "a", SecretKey: "b", Endpoint: endpoint}),
		codec, &imaging.Processor{MemoryMB: 1024, Timeout: time.Minute, MaxPixels: 1e8, TmpDir: tmp},
		&video.Tools{FFmpeg: "ffmpeg", FFprobe: "ffprobe", MemoryMB: 2560})
	s.logger.SetOutput(io.Discard)
	s.Start()
	ts := httptest.NewServer(s)
	t.Cleanup(func() { ts.Close(); s.Shutdown(t.Context()) })
	return &harness{t: t, s: s, ts: ts}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for range 100 {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestS3QueueAndRestore(t *testing.T) {
	fake := &fakeS3{objects: map[string][]byte{}}
	s3srv := httptest.NewServer(fake)
	defer s3srv.Close()
	data := t.TempDir()

	// a marker left by an earlier run is swept at startup
	_ = os.MkdirAll(filepath.Join(data, "left.png"), 0o755)
	_ = os.WriteFile(filepath.Join(data, "left.png", "left.png"), pngBytes(4, 4), 0o644)
	_ = os.MkdirAll(filepath.Join(data, ".s3-pending"), 0o755)
	_ = os.WriteFile(filepath.Join(data, ".s3-pending", "left.png"), nil, 0o644)

	h := newS3Server(t, data, fake, s3srv.URL)
	waitFor(t, "startup sweep", func() bool { _, ok := fake.get("left.png"); return ok })

	src := pngBytes(30, 30)
	hash := h.upload("a.png", src)["hash"].(string)
	waitFor(t, "upload to S3", func() bool { b, ok := fake.get(hash); return ok && bytes.Equal(b, src) })
	waitFor(t, "marker removal", func() bool {
		entries, _ := os.ReadDir(filepath.Join(data, ".s3-pending"))
		codes, _ := os.ReadDir(filepath.Join(data, ".s3-pending", "codes"))
		return len(entries) == 1 && entries[0].Name() == "codes" && len(codes) == 0
	})

	code := h.s.store.DeleteCode(hash)
	if got := fake.deleteCode(hash); got != code || code == "" {
		t.Errorf("delete code in S3: %q, want %q", got, code)
	}
	// a refresh (transcode) that finds no code locally keeps the one in S3
	_ = os.Remove(filepath.Join(data, hash, "deletecode"))
	fake.mu.Lock()
	puts := fake.puts
	fake.mu.Unlock()
	h.s.queue.push(hash, time.Now())
	waitFor(t, "refresh", func() bool { fake.mu.Lock(); defer fake.mu.Unlock(); return fake.puts > puts })
	if got := fake.deleteCode(hash); got != code {
		t.Errorf("delete code in S3 after a refresh without one: %q, want %q", got, code)
	}
	_ = os.WriteFile(filepath.Join(data, hash, "deletecode"), []byte(code), 0o644)

	// a second instance with an empty data dir restores from S3 on a miss, with the uploader's
	// delete code
	h2 := newS3Server(t, t.TempDir(), fake, s3srv.URL)
	if r := h2.get("/" + hash); r.code != 200 || !bytes.Equal(r.body, src) {
		t.Fatalf("restore: %d", r.code)
	}
	if got := h2.s.store.DeleteCode(hash); got != h.s.store.DeleteCode(hash) {
		t.Errorf("restored delete code %q, want the uploader's", got)
	}
	if r := h2.get("/10/" + hash); r.code != 200 {
		t.Errorf("variant of a restored file: %d", r.code)
	}

	// names this service can't have produced never reach S3; misses are remembered
	gets := fake.gets
	for _, p := range []string{"/../etc/passwd", "/x.php", "/2BAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA.jpg", "/" + strings.Repeat("a", 80) + ".jpg"} {
		if r := h2.get(p); r.code != 404 {
			t.Errorf("%s: %d", p, r.code)
		}
	}
	if fake.gets != gets {
		t.Errorf("invalid names were looked up in S3 (%d requests)", fake.gets-gets)
	}
	h2.get("/abcdefgh.jpg")
	n := fake.gets
	h2.get("/abcdefgh.jpg")
	if fake.gets != n {
		t.Error("S3 miss not cached")
	}

	// delete removes the S3 copy too
	before := time.Now()
	up := h.upload("b.png", pngBytes(31, 31))
	waitFor(t, "second upload", func() bool { _, ok := fake.get(up["hash"].(string)); return ok })
	if r := h.get("/delete_" + up["delete_code"].(string) + "/" + up["hash"].(string)); r.code != 200 {
		t.Fatalf("delete: %d", r.code)
	}
	if _, ok := fake.get(up["hash"].(string)); ok {
		t.Error("S3 copy survived the delete")
	}
	// a push from work that began before the delete (a transcode finishing late) is dropped
	h.s.queue.push(up["hash"].(string), before)
	if _, err := os.Stat(filepath.Join(data, ".s3-pending", up["hash"].(string))); !os.IsNotExist(err) {
		t.Error("a deleted file was queued for S3 again")
	}
}

// An upload whose S3 copy is still pending survives the cleanup service evicting its directory:
// the marker is a hard link, and the next run copies from it.
func TestS3PendingSurvivesEviction(t *testing.T) {
	fake := &fakeS3{objects: map[string][]byte{}, down: true}
	s3srv := httptest.NewServer(fake)
	defer s3srv.Close()
	data := t.TempDir()

	h := newS3Server(t, data, fake, s3srv.URL)
	src := pngBytes(23, 17)
	up := h.upload("a.png", src)
	hash, code := up["hash"].(string), up["delete_code"].(string)
	marker := filepath.Join(data, ".s3-pending", hash)
	if st, err := os.Stat(marker); err != nil || st.Size() != int64(len(src)) {
		t.Fatalf("marker is not a link to the upload: %v", err)
	}
	h.ts.Close()
	h.s.Shutdown(t.Context())

	if err := os.RemoveAll(filepath.Join(data, hash)); err != nil { // what cleanup.sh does
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.down = false
	fake.mu.Unlock()
	newS3Server(t, data, fake, s3srv.URL)
	waitFor(t, "upload from the marker", func() bool { b, ok := fake.get(hash); return ok && bytes.Equal(b, src) })
	if got := fake.deleteCode(hash); got != code {
		t.Errorf("delete code in S3: %q, want %q", got, code)
	}
	waitFor(t, "marker removal", func() bool {
		_, err := os.Stat(marker)
		_, cerr := os.Stat(filepath.Join(data, ".s3-pending", "codes", hash))
		return os.IsNotExist(err) && os.IsNotExist(cerr)
	})
}

// PictShare (same data directory) deleting a file while its S3 copy is in flight: it removes the
// directory and the marker; the copy that lands afterwards is deleted again.
func TestS3DeletedByPictShareInFlight(t *testing.T) {
	fake := &fakeS3{objects: map[string][]byte{}}
	s3srv := httptest.NewServer(fake)
	defer s3srv.Close()
	data := t.TempDir()
	var once sync.Once
	fake.putHook = func(key string) {
		once.Do(func() { // what PictShare's deleteHash does
			os.RemoveAll(filepath.Join(data, key))
			os.Remove(filepath.Join(data, ".s3-pending", key))
		})
	}
	h := newS3Server(t, data, fake, s3srv.URL)
	hash := h.upload("a.png", pngBytes(19, 21))["hash"].(string)
	waitFor(t, "the deleted copy to go", func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		_, ok := fake.objects[hash]
		return fake.deletes == 1 && !ok
	})
}

// Files stored under custom names earlier are restored too (rate limited, unlike dimension hashes).
func TestS3RestoreCustomName(t *testing.T) {
	fake := &fakeS3{objects: map[string][]byte{"my-logo.png": pngBytes(9, 9)}}
	s3srv := httptest.NewServer(fake)
	defer s3srv.Close()
	h := newS3Server(t, t.TempDir(), fake, s3srv.URL)
	if r := h.get("/my-logo.png"); r.code != 200 {
		t.Fatalf("restore of a custom name: %d", r.code)
	}
	// a copy without a delete code (made before they were kept in S3) gets a new one
	if code := h.s.store.DeleteCode("my-logo.png"); len(code) != 32 {
		t.Errorf("delete code of a restored file without one: %q", code)
	}
}

// The full-size video preview goes to S3 when rendered, and an instance without the video serves
// it from there instead of restoring the video and rendering a frame.
func TestS3Previews(t *testing.T) {
	fake := &fakeS3{objects: map[string][]byte{}}
	s3srv := httptest.NewServer(fake)
	defer s3srv.Close()
	h := newS3Server(t, t.TempDir(), fake, s3srv.URL)
	vid := makeVideo(t, t.TempDir(), "v.mp4", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-movflags", "+faststart")
	up := h.upload("v.mp4", vid)
	hash := up["hash"].(string)
	prev := h.get("/preview/" + hash)
	if prev.code != 200 || h.get("/preview/160/"+hash).code != 200 {
		t.Fatalf("previews: %d", prev.code)
	}
	waitFor(t, "preview in S3", func() bool { b, ok := fake.get(previewKey(hash)); return ok && bytes.Equal(b, prev.body) })
	waitFor(t, "video in S3", func() bool { _, ok := fake.get(hash); return ok })
	fake.mu.Lock()
	if len(fake.objects) != 2 {
		t.Errorf("sized previews went to S3 too: %d objects", len(fake.objects))
	}
	fake.mu.Unlock()

	// a second instance (evicted data directory) serves the preview without the video
	data2 := t.TempDir()
	h2 := newS3Server(t, data2, fake, s3srv.URL)
	if r := h2.get("/" + hash + "/preview/" + hash); r.code != 200 || !bytes.Equal(r.body, prev.body) {
		t.Fatalf("preview from S3: %d", r.code)
	}
	if _, err := os.Stat(filepath.Join(data2, hash, hash)); !os.IsNotExist(err) {
		t.Error("the video was restored for its preview")
	}
	n := fake.gets
	if r := h2.get("/preview/" + hash); r.code != 200 || fake.gets != n {
		t.Errorf("restored preview not served locally: %d, %d S3 requests", r.code, fake.gets-n)
	}
	if r := h2.get("/raw/preview/" + hash); r.code != 200 || r.hdr.Get("Content-Type") != "video/mp4" || !bytes.Equal(r.body, vid) {
		t.Errorf("raw wins over preview whether the video is local or not: %d %s", r.code, r.hdr.Get("Content-Type"))
	}
	if r := h2.get("/preview/" + hash); r.code != 200 || !bytes.Equal(r.body, prev.body) {
		t.Errorf("preview with the video back: %d", r.code)
	}

	// no copy in S3 yet: the video is restored, the frame rendered and uploaded
	fake.mu.Lock()
	delete(fake.objects, previewKey(hash))
	fake.mu.Unlock()
	if r := newS3Server(t, t.TempDir(), fake, s3srv.URL).get("/preview/" + hash); r.code != 200 {
		t.Fatalf("rendered preview: %d", r.code)
	}
	waitFor(t, "rendered preview in S3", func() bool { _, ok := fake.get(previewKey(hash)); return ok })

	// delete removes the preview everywhere
	if r := h.get("/delete_" + up["delete_code"].(string) + "/" + hash); r.code != 200 {
		t.Fatalf("delete: %d", r.code)
	}
	fake.mu.Lock()
	if len(fake.objects) != 0 {
		t.Errorf("objects survived the delete: %d", len(fake.objects))
	}
	fake.mu.Unlock()
	if r := newS3Server(t, t.TempDir(), fake, s3srv.URL).get("/preview/" + hash); r.code != 404 {
		t.Errorf("preview of a deleted video: %d", r.code)
	}
}
