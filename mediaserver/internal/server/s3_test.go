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
	gets    int
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
	if prefix := r.URL.Query().Get("prefix"); r.URL.Query().Get("list-type") == "2" {
		var b strings.Builder
		b.WriteString(`<?xml version="1.0"?><ListBucketResult><IsTruncated>false</IsTruncated>`)
		for k := range f.objects {
			if strings.HasPrefix(k, prefix) {
				b.WriteString("<Contents><Key>" + k + "</Key></Contents>")
			}
		}
		b.WriteString("</ListBucketResult>")
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, b.String())
		return
	}
	switch r.Method {
	case http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		f.objects[key] = b
	case http.MethodGet, http.MethodHead:
		f.gets++
		b, ok := f.objects[key]
		if !ok {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `<?xml version="1.0"?><Error><Code>NoSuchKey</Code></Error>`)
			return
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
		return len(entries) == 0
	})

	// a second instance with an empty data dir restores from S3 on a miss
	h2 := newS3Server(t, t.TempDir(), fake, s3srv.URL)
	if r := h2.get("/" + hash); r.code != 200 || !bytes.Equal(r.body, src) {
		t.Fatalf("restore: %d", r.code)
	}
	if r := h2.get("/10/" + hash); r.code != 200 {
		t.Errorf("variant of a restored file: %d", r.code)
	}

	// names this service can't have produced never reach S3; misses are remembered
	before := fake.gets
	for _, p := range []string{"/../etc/passwd", "/x.php", "/2BAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA.jpg", "/" + strings.Repeat("a", 80) + ".jpg"} {
		if r := h2.get(p); r.code != 404 {
			t.Errorf("%s: %d", p, r.code)
		}
	}
	if fake.gets != before {
		t.Errorf("invalid names were looked up in S3 (%d requests)", fake.gets-before)
	}
	h2.get("/abcdefgh.jpg")
	n := fake.gets
	h2.get("/abcdefgh.jpg")
	if fake.gets != n {
		t.Error("S3 miss not cached")
	}

	// delete removes the S3 copy too
	up := h.upload("b.png", pngBytes(31, 31))
	waitFor(t, "second upload", func() bool { _, ok := fake.get(up["hash"].(string)); return ok })
	if r := h.get("/delete_" + up["delete_code"].(string) + "/" + up["hash"].(string)); r.code != 200 {
		t.Fatalf("delete: %d", r.code)
	}
	if _, ok := fake.get(up["hash"].(string)); ok {
		t.Error("S3 copy survived the delete")
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
	hash := up["hash"].(string)
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
	waitFor(t, "marker removal", func() bool { _, err := os.Stat(marker); return os.IsNotExist(err) })
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

// Custom names are restored too (rate limited, unlike dimension hashes).
func TestS3RestoreCustomName(t *testing.T) {
	fake := &fakeS3{objects: map[string][]byte{"my-logo.png": pngBytes(9, 9)}}
	s3srv := httptest.NewServer(fake)
	defer s3srv.Close()
	h := newS3Server(t, t.TempDir(), fake, s3srv.URL)
	if r := h.get("/my-logo.png"); r.code != 200 {
		t.Fatalf("restore of a custom name: %d", r.code)
	}
}

// Video previews go to S3 when rendered, and an instance without the video serves them from there
// instead of restoring the video and rendering a frame.
func TestS3Previews(t *testing.T) {
	fake := &fakeS3{objects: map[string][]byte{}}
	s3srv := httptest.NewServer(fake)
	defer s3srv.Close()
	h := newS3Server(t, t.TempDir(), fake, s3srv.URL)
	vid := makeVideo(t, t.TempDir(), "v.mp4", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-movflags", "+faststart")
	up := h.upload("v.mp4", vid)
	hash := up["hash"].(string)
	full, sized := previewKeyPrefix(hash)+hash+"_preview.jpg", previewKeyPrefix(hash)+"160_"+hash+"_preview.jpg"
	r1, r2 := h.get("/preview/"+hash), h.get("/preview/160/"+hash)
	if r1.code != 200 || r2.code != 200 {
		t.Fatalf("previews: %d %d", r1.code, r2.code)
	}
	waitFor(t, "previews in S3", func() bool {
		b1, ok1 := fake.get(full)
		b2, ok2 := fake.get(sized)
		return ok1 && ok2 && bytes.Equal(b1, r1.body) && bytes.Equal(b2, r2.body)
	})
	waitFor(t, "video in S3", func() bool { _, ok := fake.get(hash); return ok })

	// a second instance (evicted data directory) serves the preview without the video
	data2 := t.TempDir()
	h2 := newS3Server(t, data2, fake, s3srv.URL)
	if r := h2.get("/preview/160/" + hash); r.code != 200 || !bytes.Equal(r.body, r2.body) {
		t.Fatalf("preview from S3: %d", r.code)
	}
	if _, err := os.Stat(filepath.Join(data2, hash)); !os.IsNotExist(err) {
		t.Error("the video's directory was created for its preview (PictShare would take the video as stored)")
	}
	n := fake.gets
	if r := h2.get("/preview/160/" + hash); r.code != 200 || fake.gets != n {
		t.Errorf("restored preview not served locally: %d, %d S3 requests", r.code, fake.gets-n)
	}
	// the video is restored when it's asked for, and the restored preview is still used
	if r := h2.get("/raw/" + hash); r.code != 200 || !bytes.Equal(r.body, vid) {
		t.Fatalf("raw after a preview restore: %d", r.code)
	}
	if r := h2.get("/preview/160/" + hash); r.code != 200 || !bytes.Equal(r.body, r2.body) {
		t.Errorf("preview with the video back: %d", r.code)
	}

	// a preview S3 doesn't have yet: the video is restored, the frame rendered and uploaded
	data3 := t.TempDir()
	h3 := newS3Server(t, data3, fake, s3srv.URL)
	if r := h3.get("/preview/200x100/" + hash); r.code != 200 {
		t.Fatalf("rendered preview: %d", r.code)
	}
	waitFor(t, "rendered preview in S3", func() bool { _, ok := fake.get(previewKeyPrefix(hash) + "200_" + hash + "_preview.jpg"); return ok })

	// delete removes the previews everywhere
	if r := h.get("/delete_" + up["delete_code"].(string) + "/" + hash); r.code != 200 {
		t.Fatalf("delete: %d", r.code)
	}
	fake.mu.Lock()
	for k := range fake.objects {
		if strings.HasPrefix(k, hash) || strings.HasPrefix(k, previewKeyPrefix(hash)) {
			t.Errorf("%s survived the delete", k)
		}
	}
	fake.mu.Unlock()
}
