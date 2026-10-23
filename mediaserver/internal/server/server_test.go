package server

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spaceshelter/orbitar-media/mediaserver/internal/hashcodec"
	"github.com/spaceshelter/orbitar-media/mediaserver/internal/imaging"
	"github.com/spaceshelter/orbitar-media/mediaserver/internal/store"
	"github.com/spaceshelter/orbitar-media/mediaserver/internal/video"
)

const testKey = "0123456789abcdef0123456789abcdef"

func needTools(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
}

type harness struct {
	t   *testing.T
	s   *Server
	ts  *httptest.Server
	dir string
}

func newHarness(t *testing.T, mod func(*Config, *store.Store)) *harness {
	t.Helper()
	needTools(t, "file", "vips", "ffmpeg", "ffprobe")
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	tmp := filepath.Join(dir, "tmp")
	for _, d := range []string{data, tmp} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	st := &store.Store{Root: data}
	cfg := Config{
		ClientID: "secret", MaxUploadMB: 5, TmpDir: tmp,
		UploadImages: true, UploadVideos: true, UploadText: true,
		VideoSlots: 2, ImageSlots: 2,
	}
	if mod != nil {
		mod(&cfg, st)
	}
	codec, err := hashcodec.New(testKey)
	if err != nil {
		t.Fatal(err)
	}
	img := &imaging.Processor{MemoryMB: 1024, Timeout: time.Minute, MaxPixels: 100e6, TmpDir: tmp}
	vid := &video.Tools{FFmpeg: "ffmpeg", FFprobe: "ffprobe", MemoryMB: 2560}
	s := New(cfg, st, nil, codec, img, vid)
	s.logger.SetOutput(io.Discard)
	s.Start()
	ts := httptest.NewServer(s)
	t.Cleanup(func() {
		ts.Close()
		s.Shutdown(t.Context())
	})
	return &harness{t: t, s: s, ts: ts, dir: dir}
}

type resp struct {
	code int
	hdr  http.Header
	body []byte
}

func (h *harness) do(req *http.Request) resp {
	h.t.Helper()
	tr := &http.Transport{DisableCompression: true}
	c := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := c.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{res.StatusCode, res.Header, b}
}

func (h *harness) get(path string, hdr ...string) resp {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, h.ts.URL+path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	return h.do(req)
}

func (h *harness) upload(name string, content []byte, fields ...string) map[string]any {
	h.t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for i := 0; i+1 < len(fields); i += 2 {
		_ = mw.WriteField(fields[i], fields[i+1])
	}
	fw, _ := mw.CreateFormFile("file", name)
	_, _ = fw.Write(content)
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPost, h.ts.URL+"/api/upload.php", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Client-ID secret")
	r := h.do(req)
	if r.code != 200 {
		h.t.Fatalf("upload: HTTP %d %s", r.code, r.body)
	}
	var out map[string]any
	if err := json.Unmarshal(r.body, &out); err != nil {
		h.t.Fatalf("upload: %v: %s", err, r.body)
	}
	return out
}

func testImage(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{uint8(x * 255 / w), uint8(y * 255 / h), 128, 255})
		}
	}
	return img
}

func jpegBytes(w, h int) []byte {
	var b bytes.Buffer
	_ = jpeg.Encode(&b, testImage(w, h), &jpeg.Options{Quality: 90})
	return b.Bytes()
}

func pngBytes(w, h int) []byte {
	var b bytes.Buffer
	_ = png.Encode(&b, testImage(w, h))
	return b.Bytes()
}

func decodeSize(t *testing.T, b []byte) (int, int, string) {
	t.Helper()
	cfg, format, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("decode: %v (%d bytes: %.60q)", err, len(b), b)
	}
	return cfg.Width, cfg.Height, format
}

func TestImageUploadAndVariants(t *testing.T) {
	h := newHarness(t, nil)
	up := h.upload("a.jpg", jpegBytes(640, 480))
	if up["status"] != "ok" || up["filetype"] != "jpeg" {
		t.Fatalf("upload: %v", up)
	}
	hash := up["hash"].(string)
	info, err := h.s.codec.Decode(hash)
	if err != nil || info.Width != 640 || info.Height != 480 || info.Type != "jpg" {
		t.Fatalf("hash %s decodes to %+v, %v", hash, info, err)
	}
	if up["url"] != hash || up["delete_url"] != "delete_"+up["delete_code"].(string)+"/"+hash {
		t.Fatalf("urls: %v", up)
	}

	cases := []struct {
		path   string
		w, h   int
		format string
	}{
		{"/" + hash, 640, 480, "jpeg"},
		{"/300/" + hash, 300, 225, "jpeg"},
		{"/w/300/" + hash, 300, 225, "jpeg"},
		{"/300x100/" + hash, 300, 225, "jpeg"}, // PictShare: landscape images fit the width only
		{"/50x100/" + hash, 50, 37, "jpeg"},
		{"/2000/" + hash, 640, 480, "jpeg"}, // never upscaled
		{"/left/" + hash, 480, 640, "jpeg"},
		{"/300/right/" + hash, 225, 300, "jpeg"},
		{"/right/300/" + hash, 225, 300, "jpeg"},
		{"/upside/" + hash, 640, 480, "jpeg"},
		{"/sepia/300/" + hash, 300, 225, "jpeg"}, // dropped filters are ignored
		{"/100x100/forcesize/" + hash, 100, 100, "jpeg"},
		{"/300/webp/" + hash, 300, 225, "webp"},
		{"/" + hash + "/300", 300, 225, "jpeg"},
	}
	for _, c := range cases {
		r := h.get(c.path)
		if r.code != 200 {
			t.Errorf("%s: HTTP %d %s", c.path, r.code, r.body)
			continue
		}
		if c.format == "webp" {
			if r.hdr.Get("Content-Type") != "image/webp" || !bytes.HasPrefix(r.body[8:], []byte("WEBP")) {
				t.Errorf("%s: not webp (%s)", c.path, r.hdr.Get("Content-Type"))
			}
			continue
		}
		w, hh, f := decodeSize(t, r.body)
		if w != c.w || hh != c.h || f != c.format {
			t.Errorf("%s: got %dx%d %s, want %dx%d %s", c.path, w, hh, f, c.w, c.h, c.format)
		}
		if !strings.Contains(r.hdr.Get("Cache-Control"), "max-age=31536000") || r.hdr.Get("ETag") == "" {
			t.Errorf("%s: caching headers %v", c.path, r.hdr)
		}
	}

	// repeated requests hit the cached variant
	a, b := h.get("/300/left/"+hash), h.get("/300/left/"+hash)
	if !bytes.Equal(a.body, b.body) || a.hdr.Get("ETag") != b.hdr.Get("ETag") {
		t.Error("variant regenerated")
	}
	if r := h.get("/300/left/"+hash, "If-None-Match", a.hdr.Get("ETag")); r.code != 304 {
		t.Errorf("variant 304: %d", r.code)
	}

	// dedupe: same bytes again -> same hash, no delete code
	again := h.upload("b.jpg", jpegBytes(640, 480))
	if again["status"] != "ok" {
		t.Fatalf("re-upload: %v", again)
	}
	// JPEGs are re-encoded, so the second upload of the original bytes is a new file; uploading the
	// stored (normalised) bytes must dedupe.
	stored := h.get("/" + hash).body
	dup := h.upload("c.jpg", stored)
	if dup["hash"] != hash || dup["delete_code"] != nil {
		t.Errorf("dedupe of stored bytes: %v", dup)
	}

	// wrong delete code: served normally; right one deletes
	if r := h.get("/delete_nope/" + hash); r.code != 200 || !bytes.Equal(r.body, stored) {
		t.Errorf("wrong delete code: %d", r.code)
	}
	r := h.get("/delete_" + up["delete_code"].(string) + "/" + hash)
	if r.code != 200 || string(r.body) != hash+" deleted successfully" {
		t.Fatalf("delete: %d %s", r.code, r.body)
	}
	if r := h.get("/" + hash); r.code != 404 || r.hdr.Get("Cache-Control") != "no-store" {
		t.Errorf("after delete: %d %v", r.code, r.hdr)
	}
}

func TestPNGStoredAsIs(t *testing.T) {
	h := newHarness(t, nil)
	src := pngBytes(50, 40)
	up := h.upload("a.png", src)
	if up["status"] != "ok" || up["filetype"] != "png" {
		t.Fatalf("upload: %v", up)
	}
	r := h.get("/" + up["hash"].(string))
	if !bytes.Equal(r.body, src) || r.hdr.Get("Content-Type") != "image/png" {
		t.Errorf("png not stored byte for byte (%s)", r.hdr.Get("Content-Type"))
	}
	if r := h.get("/20/" + up["hash"].(string)); r.code != 200 {
		t.Errorf("png resize: %d", r.code)
	} else if w, hh, f := decodeSize(t, r.body); w != 20 || hh != 16 || f != "png" {
		t.Errorf("png resize: %dx%d %s", w, hh, f)
	}
}

func TestConditionalAndRange(t *testing.T) {
	h := newHarness(t, nil)
	hash := h.upload("a.png", pngBytes(64, 64))["hash"].(string)
	full := h.get("/" + hash)
	etag, lm := full.hdr.Get("ETag"), full.hdr.Get("Last-Modified")
	if !strings.HasPrefix(etag, `"`) || strings.HasPrefix(etag, `W/`) || lm == "" {
		t.Fatalf("validators: %q %q", etag, lm)
	}
	if r := h.get("/"+hash, "If-None-Match", etag); r.code != 304 {
		t.Errorf("If-None-Match: %d", r.code)
	}
	if r := h.get("/"+hash, "If-Modified-Since", lm); r.code != 304 {
		t.Errorf("If-Modified-Since: %d", r.code)
	}
	n := len(full.body)
	r := h.get("/"+hash, "Range", "bytes=10-19")
	if r.code != 206 || !bytes.Equal(r.body, full.body[10:20]) || r.hdr.Get("Content-Range") != "bytes 10-19/"+itoa(n) {
		t.Errorf("range: %d %q", r.code, r.hdr.Get("Content-Range"))
	}
	if r := h.get("/"+hash, "Range", "bytes=-5"); r.code != 206 || !bytes.Equal(r.body, full.body[n-5:]) {
		t.Errorf("suffix range: %d", r.code)
	}
	if r := h.get("/"+hash, "Range", "bytes=10-19", "If-Range", `"stale"`); r.code != 200 || len(r.body) != n {
		t.Errorf("stale If-Range: %d", r.code)
	}
	r = h.get("/"+hash, "Range", "bytes=999999-")
	if r.code != 416 || r.hdr.Get("Content-Range") != "bytes */"+itoa(n) || r.hdr.Get("Cache-Control") != "no-store" ||
		r.hdr.Get("ETag") != "" {
		t.Errorf("unsatisfiable: %d %v", r.code, r.hdr)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestNotFoundAndHeaders(t *testing.T) {
	h := newHarness(t, nil)
	for _, p := range []string{"/", "/nothere.jpg", "/../../etc/passwd", "/wp-admin/css/colors/light/x.css"} {
		r := h.get(p)
		if r.code != 404 || r.hdr.Get("Cache-Control") != "no-store" {
			t.Errorf("%s: %d %q", p, r.code, r.hdr.Get("Cache-Control"))
		}
		if r.hdr.Get("X-Content-Type-Options") != "nosniff" || r.hdr.Get("X-Frame-Options") != "SAMEORIGIN" {
			t.Errorf("%s: security headers missing", p)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, h.ts.URL+"/x.jpg", nil)
	if r := h.do(req); r.code != 404 {
		t.Errorf("POST: %d", r.code)
	}
	if r := h.get("/api/base64.php", "Authorization", "Client-ID secret"); r.code != 404 {
		t.Errorf("dropped API: %d", r.code)
	}
	if r := h.get("/api/info.php?hash=x.jpg"); r.code != 403 {
		t.Errorf("API without Client-ID: %d", r.code)
	}
	if r := h.get("/healthz"); r.code != 200 || string(r.body) != "ok" {
		t.Errorf("healthz: %d", r.code)
	}
}

func TestUploadErrors(t *testing.T) {
	h := newHarness(t, nil)
	if up := h.upload("x.bin", []byte{0, 1, 2, 3, 0xff, 0xfe, 0, 0, 9}); up["status"] != "err" ||
		!strings.HasPrefix(up["reason"].(string), "Unsupported filetype") {
		t.Errorf("binary: %v", up)
	}
	if up := h.upload("big.txt", bytes.Repeat([]byte("a"), 5_000_001)); up["reason"] != "File too big. 5mb max" {
		t.Errorf("too big: %v", up)
	}
	req, _ := http.NewRequest(http.MethodPost, h.ts.URL+"/api/upload.php", strings.NewReader("x"))
	if r := h.do(req); r.code != 403 || !bytes.Contains(r.body, []byte(`"Access denied"`)) {
		t.Errorf("no Client-ID: %d %s", r.code, r.body)
	}
	req, _ = http.NewRequest(http.MethodPost, h.ts.URL+"/api/upload.php", strings.NewReader("x"))
	req.Header.Set("Authorization", "Client-ID secret")
	if r := h.do(req); !bytes.Contains(r.body, []byte(`"Upload error"`)) {
		t.Errorf("no file: %s", r.body)
	}
	// custom names are validated
	if up := h.upload("a.png", pngBytes(8, 8), "hash", "mine"); up["status"] != "ok" || up["hash"] != "mine.png" {
		t.Errorf("custom: %v", up)
	}
	if up := h.upload("a.png", pngBytes(9, 9), "hash", "mine"); up["reason"] != "Custom hash already exists" {
		t.Errorf("custom exists: %v", up)
	}
}

func TestJSONEncoding(t *testing.T) {
	got := string(marshalOrdered([]kv{{"status", "ok"}, {"url", "https://x/y<z>&"}, {"n", 3}, {"f", false}}))
	want := `{"status":"ok","url":"https:\/\/x\/y<z>&","n":3,"f":false}`
	if got != want {
		t.Errorf("got %s want %s", got, want)
	}
}

func TestTextAndGzip(t *testing.T) {
	h := newHarness(t, nil)
	text := strings.Repeat("hello <world>\n", 100)
	up := h.upload("a.txt", []byte(text))
	if up["status"] != "ok" || up["filetype"] != "text" || !strings.HasSuffix(up["hash"].(string), ".txt") {
		t.Fatalf("text upload: %v", up)
	}
	hash := up["hash"].(string)
	r := h.get("/raw/" + hash)
	if string(r.body) != text || !strings.HasPrefix(r.hdr.Get("Content-Type"), "text/plain") {
		t.Errorf("raw: %q", r.hdr.Get("Content-Type"))
	}
	r = h.get("/"+hash, "Accept-Encoding", "gzip")
	if r.hdr.Get("Content-Encoding") != "gzip" {
		t.Fatalf("text page not gzipped: %v", r.hdr)
	}
	zr, err := gzip.NewReader(bytes.NewReader(r.body))
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(zr)
	if !bytes.Contains(page, []byte("hello &lt;world&gt;")) || bytes.Contains(page, []byte("<world>")) {
		t.Error("text page doesn't escape the content")
	}
	req, _ := http.NewRequest(http.MethodHead, h.ts.URL+"/"+hash, nil)
	if r := h.do(req); r.code != 200 || !strings.HasPrefix(r.hdr.Get("Content-Type"), "text/html") || len(r.body) != 0 {
		t.Errorf("HEAD text page: %d %q", r.code, r.hdr.Get("Content-Type"))
	}
}

// The whole upload request is bounded, not only the file part.
func TestUploadRequestLimit(t *testing.T) {
	h := newHarness(t, nil)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "a.png")
	_, _ = fw.Write(pngBytes(8, 8))
	jw, _ := mw.CreateFormField("junk")
	_, _ = jw.Write(bytes.Repeat([]byte("x"), 7_000_000)) // MaxUploadMB is 5
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPost, h.ts.URL+"/api/upload.php", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Client-ID secret")
	r := h.do(req)
	if !bytes.Contains(r.body, []byte("File too big")) {
		t.Fatalf("oversized request: %d %s", r.code, r.body)
	}
}

func makeVideo(t *testing.T, dir, name string, args ...string) []byte {
	t.Helper()
	out := filepath.Join(dir, name)
	base := []string{"-v", "error", "-f", "lavfi", "-i", "testsrc=size=320x240:rate=25:duration=1",
		"-f", "lavfi", "-i", "sine=duration=1"}
	cmd := exec.Command("ffmpeg", append(append(base, args...), "-y", out)...)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, b)
	}
	b, _ := os.ReadFile(out)
	return b
}

func TestVideo(t *testing.T) {
	h := newHarness(t, nil)
	dir := t.TempDir()

	// compliant: stored byte for byte
	good := makeVideo(t, dir, "good.mp4", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-movflags", "+faststart")
	up := h.upload("good.mp4", good)
	if up["status"] != "ok" || up["filetype"] != "mp4" {
		t.Fatalf("upload: %v", up)
	}
	hash := up["hash"].(string)
	if r := h.get("/raw/" + hash); !bytes.Equal(r.body, good) || r.hdr.Get("Content-Type") != "video/mp4" {
		t.Error("compliant video was modified")
	}
	if r := h.get("/" + hash); r.code != 200 || !bytes.Contains(r.body, []byte("<video")) {
		t.Errorf("page: %d", r.code)
	}
	if r := h.get("/preview/" + hash); r.code != 200 {
		t.Errorf("preview: %d %s", r.code, r.body)
	} else if w, hh, f := decodeSize(t, r.body); w != 320 || hh != 240 || f != "jpeg" {
		t.Errorf("preview: %dx%d %s", w, hh, f)
	}
	if r := h.get("/preview/160/" + hash); r.code != 200 {
		t.Errorf("sized preview: %d", r.code)
	} else if w, _, _ := decodeSize(t, r.body); w != 160 {
		t.Errorf("sized preview width %d", w)
	}
	if r := h.get("/300/" + hash); r.code != 404 {
		t.Errorf("video size variant: %d", r.code)
	}

	// moov at the end: fixed during the upload, before the URL is handed out
	late := makeVideo(t, dir, "late.mp4", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac")
	up = h.upload("late.mp4", late)
	if up["status"] != "ok" {
		t.Fatalf("upload: %v", up)
	}
	path := h.s.store.Path(up["hash"].(string))
	if mb := video.MoovBeforeMdat(path); mb == nil || !*mb {
		t.Error("moov still after mdat")
	}
	// re-uploading the original dedupes onto the same hash
	if again := h.upload("late.mp4", late); again["hash"] != up["hash"] {
		t.Errorf("dedupe of remuxed upload: %v", again)
	}

	// HEVC is still rejected
	hevc := makeVideo(t, dir, "hevc.mp4", "-c:v", "libx265", "-pix_fmt", "yuv420p", "-an")
	if up := h.upload("hevc.mp4", hevc); up["status"] != "err" {
		t.Errorf("hevc accepted: %v", up)
	}
}

func TestGetURL(t *testing.T) {
	src := pngBytes(30, 20)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(src) }))
	defer origin.Close()

	call := func(h *harness, auth, u string) map[string]any {
		req, _ := http.NewRequest(http.MethodGet, h.ts.URL+"/api/geturl.php?url="+url.QueryEscape(u), nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		r := h.do(req)
		var out map[string]any
		_ = json.Unmarshal(r.body, &out)
		out["_code"] = r.code
		return out
	}

	h := newHarness(t, nil)
	if out := call(h, "", origin.URL); out["_code"] != 403 || out["reason"] != "Access denied" {
		t.Errorf("no auth: %v", out)
	}
	if out := call(h, "Client-ID wrong", origin.URL); out["_code"] != 403 {
		t.Errorf("wrong auth: %v", out)
	}
	if out := call(h, "Client-ID secret", origin.URL); out["reason"] != "Could not download file" {
		t.Errorf("loopback fetch allowed: %v", out)
	}
	if out := call(h, "Client-ID secret", "file:///etc/passwd"); out["reason"] != "Invalid URL" {
		t.Errorf("file url: %v", out)
	}

	h2 := newHarness(t, func(c *Config, _ *store.Store) { c.AllowPrivateFetch = true })
	if out := call(h2, "Client-ID secret", origin.URL); out["status"] != "ok" || out["filetype"] != "png" {
		t.Errorf("fetch: %v", out)
	}
}

func TestPublicAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8": true, "2606:4700::1111": true,
		"127.0.0.1": false, "10.1.2.3": false, "172.16.0.1": false, "192.168.4.3": false, "169.254.169.254": false,
		"100.64.0.1": false, "0.0.0.0": false, "::1": false, "fc00::1": false, "fe80::1": false,
		"::ffff:127.0.0.1": false, "224.0.0.1": false, "64:ff9b::a00:1": false,
	} {
		if got := publicAddr(netip.MustParseAddr(addr)); got != want {
			t.Errorf("%s: %v", addr, got)
		}
	}
}

func TestReadOnly(t *testing.T) {
	var data string
	h := newHarness(t, func(c *Config, st *store.Store) {
		data = st.Root
		st.ReadOnly = true
		st.Overlay = filepath.Join(filepath.Dir(c.TmpDir), "overlay")
		_ = os.MkdirAll(st.Overlay, 0o755)
	})
	// a file put there by the other service
	name := "abcdef.png"
	_ = os.MkdirAll(filepath.Join(data, name), 0o755)
	_ = os.WriteFile(filepath.Join(data, name, name), pngBytes(40, 40), 0o644)
	_ = os.WriteFile(filepath.Join(data, name, "deletecode"), []byte("code"), 0o644)
	before, _ := os.ReadDir(filepath.Join(data, name))

	if r := h.get("/20/" + name); r.code != 200 {
		t.Fatalf("variant: %d", r.code)
	}
	if r := h.get("/delete_code/" + name); r.code != 503 {
		t.Errorf("delete in read-only: %d", r.code)
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "a.png")
	_, _ = fw.Write(pngBytes(3, 3))
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPost, h.ts.URL+"/api/upload.php", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Client-ID secret")
	if r := h.do(req); r.code != 503 {
		t.Errorf("upload in read-only: %d", r.code)
	}
	after, _ := os.ReadDir(filepath.Join(data, name))
	if len(after) != len(before) {
		t.Errorf("read-only wrote into the data dir: %d -> %d entries", len(before), len(after))
	}
	if _, err := os.Stat(filepath.Join(data, "sha1.csv")); err == nil {
		t.Error("read-only created sha1.csv")
	}
}
