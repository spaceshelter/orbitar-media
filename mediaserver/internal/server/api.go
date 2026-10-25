package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spaceshelter/orbitar-media/mediaserver/internal/hashcodec"
	"github.com/spaceshelter/orbitar-media/mediaserver/internal/imaging"
	"github.com/spaceshelter/orbitar-media/mediaserver/internal/proc"
	"github.com/spaceshelter/orbitar-media/mediaserver/internal/store"
	"github.com/spaceshelter/orbitar-media/mediaserver/internal/video"
)

// answer is a controller's reply; ok answers get filetype, delete code and S3 push added.
type answer struct {
	ok     bool
	hash   string
	reason string
}

func (s *Server) maxUploadBytes() int64 { return int64(s.cfg.MaxUploadMB) * 1_000_000 }

func (s *Server) tooBig(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, r, kv{"status", "err"}, kv{"reason", fmt.Sprintf("File too big. %dmb max", s.cfg.MaxUploadMB)})
}

// handleUpload is /api/upload.php: a multipart "file" field (Caddy has checked the Client-ID).
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if s.store.ReadOnly {
		s.writeJSONStatus(w, r, http.StatusServiceUnavailable, kv{"status", "err"}, kv{"reason", "Uploads are disabled"})
		return
	}
	// the whole request, not only the file part, is bounded in size and time
	r.Body = http.MaxBytesReader(w, r.Body, s.maxUploadBytes()+1<<20)
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(uploadReadTimeout))
	tmp, err := s.receiveUpload(r)
	if tmp != "" {
		defer os.Remove(tmp)
	}
	switch {
	case errors.Is(err, errTooBig):
		s.tooBig(w, r)
		return
	case err != nil:
		if !errors.Is(err, errNoFile) {
			log.Printf("upload: %v", err)
		}
		s.writeJSON(w, r, kv{"status", "err"}, kv{"reason", "Upload error"})
		return
	}
	allowed := func(typ string) bool {
		return s.cfg.UploadImages && slices.Contains(imageExts, typ) ||
			s.cfg.UploadVideos && slices.Contains(videoExts, typ) ||
			s.cfg.UploadText && typ == "text"
	}
	s.ingest(w, r, tmp, allowed, func(typ any) []kv {
		return []kv{{"status", "err"}, {"reason", fmt.Sprintf("Unsupported filetype: %v", phpString(typ))}, {"filetype", typ}}
	})
}

// uploadReadTimeout bounds how long receiving one upload may take (50 MB at ~60 KB/s).
const uploadReadTimeout = 15 * time.Minute

var (
	errTooBig = errors.New("too big")
	errNoFile = errors.New("no file")
)

// receiveUpload streams the "file" part to a temp file, at most MAX_UPLOAD_SIZE. Other parts
// (PictShare's custom name "hash", for one) are ignored.
func (s *Server) receiveUpload(r *http.Request) (tmp string, err error) {
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if r.Method != http.MethodPost || ct != "multipart/form-data" {
		return "", errNoFile
	}
	mr, err := r.MultipartReader()
	if err != nil {
		return "", errNoFile
	}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
				err = errTooBig
			}
			return tmp, err
		}
		switch part.FormName() {
		case "file":
			if tmp != "" {
				continue
			}
			tmp, err = s.saveLimited(part)
			if err != nil {
				return tmp, err
			}
		}
		part.Close()
	}
	if tmp == "" {
		return "", errNoFile
	}
	return tmp, nil
}

// saveLimited copies r to a new temp file; errTooBig past the upload limit.
func (s *Server) saveLimited(r io.Reader) (string, error) {
	f, err := os.CreateTemp(s.cfg.TmpDir, "upload-*")
	if err != nil {
		return "", err
	}
	n, err := io.Copy(f, io.LimitReader(r, s.maxUploadBytes()+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		err = errTooBig
	}
	if err == nil && n > s.maxUploadBytes() {
		err = errTooBig
	}
	return f.Name(), err
}

// ingest is the part upload.php and geturl.php share: type detection, dedupe, the content
// controller, then the extra answer fields and the S3 copy.
func (s *Server) ingest(w http.ResponseWriter, r *http.Request, tmp string, allowed func(string) bool, unsupported func(any) []kv) {
	start := time.Now()
	typ, typOK := s.fileType(r.Context(), tmp)
	var typJSON any = typ
	if !typOK {
		typJSON = false
	}

	sum, err := store.FileSHA1(tmp)
	if err != nil {
		s.writeJSON(w, r, kv{"status", "err"}, kv{"reason", "Upload error"})
		return
	}
	if existing := s.store.LookupSHA1(sum); existing != "" && s.store.Exists(existing) {
		s.writeJSON(w, r, kv{"status", "ok"}, kv{"hash", existing}, kv{"filetype", typJSON}, kv{"url", s.cfg.URL + existing})
		return
	}
	if st, err := os.Stat(tmp); err == nil && st.Size() > s.maxUploadBytes() {
		s.tooBig(w, r)
		return
	}

	var ans *answer
	if typOK && allowed(typ) {
		switch {
		case slices.Contains(imageExts, typ):
			ans = s.uploadImage(r.Context(), tmp)
		case slices.Contains(videoExts, typ):
			ans = s.uploadVideo(r.Context(), tmp)
		case typ == "text":
			ans = s.put(tmp, s.randomName("txt"))
		}
	}
	if ans == nil {
		s.writeJSON(w, r, unsupported(typJSON)...)
		return
	}
	if !ans.ok {
		fields := []kv{{"status", "err"}}
		if ans.hash != "" {
			fields = append(fields, kv{"hash", ans.hash})
		}
		s.writeJSON(w, r, append(fields, kv{"reason", ans.reason})...)
		return
	}
	s.store.AddSHA1(ans.hash, sum)
	fields := []kv{{"status", "ok"}, {"hash", ans.hash}, {"url", s.cfg.URL + ans.hash}, {"filetype", typJSON}}
	if code := s.store.DeleteCode(ans.hash); code != "" {
		fields = append(fields, kv{"delete_code", code}, kv{"delete_url", s.cfg.URL + "delete_" + code + "/" + ans.hash})
	}
	s.queue.push(ans.hash, start)
	s.writeJSON(w, r, fields...)
}

// phpString renders a value the way PHP's string concatenation does (false -> "").
func phpString(v any) string {
	if b, ok := v.(bool); ok {
		if b {
			return "1"
		}
		return ""
	}
	return fmt.Sprint(v)
}

// fileType is PictShare's getTypeOfFile(): the subtype of `file -bi`, "text" for any text/*,
// "mp4" for octet-stream that is a playable MP4, false (ok=false) for an mp4 that isn't.
func (s *Server) fileType(ctx context.Context, path string) (typ string, ok bool) {
	_ = s.withSlot(ctx, s.probeSlots, func() error {
		typ, ok = s.detectType(ctx, path)
		return nil
	})
	return typ, ok
}

func (s *Server) detectType(ctx context.Context, path string) (string, bool) {
	res := proc.Run(ctx, proc.Limits{Timeout: 30 * time.Second}, "file", "-bi", path)
	out := strings.TrimSpace(string(res.Stdout))
	if res.Err != nil || !strings.Contains(out, "/") || !strings.Contains(out, ";") {
		return "", false
	}
	if strings.HasPrefix(out, "text") {
		return "text", true
	}
	mimeType, _, _ := strings.Cut(out, ";")
	_, sub, _ := strings.Cut(strings.TrimSpace(mimeType), "/")
	switch sub {
	case "octet-stream":
		if s.vid.Probe(ctx, path).IsProperMP4() {
			return "mp4", true
		}
	case "mp4":
		if !s.vid.Probe(ctx, path).IsProperMP4() {
			return "", false
		}
	}
	return sub, true
}

// newName picks a fresh name: a dimension hash when a key is configured, else random.
func (s *Server) newName(ext string, w, h int) (string, error) {
	if s.codec != nil {
		for range 100 {
			name, err := s.codec.Encode(ext, w, h)
			if err != nil {
				return "", err
			}
			if !s.store.Exists(name) && !dirExists(s.store.Dir(name)) {
				return name, nil
			}
		}
		return "", errors.New("no free name")
	}
	return s.randomName(ext), nil
}

func (s *Server) randomName(ext string) string {
	for n := 6; ; n++ {
		name := hashcodec.Random(n) + "." + ext
		if !dirExists(s.store.Dir(name)) {
			return name
		}
	}
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// claim picks the name for a new upload.
func (s *Server) claim(ext string, w, h int) (string, *answer) {
	name, err := s.newName(ext, w, h)
	if err != nil {
		return "", &answer{reason: "Could not create a name"}
	}
	return name, nil
}

func (s *Server) put(tmp, name string) *answer {
	if _, err := s.store.Put(tmp, name, ""); err != nil {
		log.Printf("store %s: %v", name, err)
		return &answer{reason: "Could not store file"}
	}
	return &answer{ok: true, hash: name}
}

// uploadImage is ImageController::handleUpload: JPEGs are re-encoded with the EXIF orientation
// applied and all metadata stripped; PNG, GIF and WebP are stored as they are.
func (s *Server) uploadImage(ctx context.Context, tmp string) *answer {
	ext := imaging.Sniff(tmp)
	switch ext {
	case "gif", "png", "webp":
	case "jpg":
		norm := filepath.Join(s.cfg.TmpDir, filepath.Base(store.TempName(tmp)))
		defer os.Remove(norm)
		if err := s.withImageSlot(ctx, func() error { return s.img.NormalizeJPEG(ctx, tmp, norm) }); err != nil {
			log.Printf("upload: jpeg normalisation failed: %v", err)
			return &answer{reason: "Not a valid image"}
		}
		if err := os.Rename(norm, tmp); err != nil {
			return &answer{reason: "Upload error"}
		}
	default:
		return &answer{reason: "Not a valid image"}
	}
	w, h, err := s.img.Dimensions(ctx, tmp)
	if err != nil {
		return &answer{reason: "Not a valid image"}
	}
	name, fail := s.claim(ext, w, h)
	if fail != nil {
		return fail
	}
	return s.put(tmp, name)
}

func (s *Server) withImageSlot(ctx context.Context, f func() error) error {
	return s.withSlot(ctx, s.imageSlots, f)
}

// withSlot runs f once slots has room (or gives up with ctx).
func (s *Server) withSlot(ctx context.Context, slots chan struct{}, f func() error) error {
	select {
	case slots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-slots }()
	return f()
}

// uploadVideo is VideoController::handleUpload. Container and audio fixes happen before the URL
// is handed out (stream copy, about as fast as copying the file); a real transcode runs in the
// background, admitted only while a conversion slot is free.
func (s *Server) uploadVideo(ctx context.Context, tmp string) *answer {
	var probe *video.Probe
	var plan *video.Plan
	if err := s.withSlot(ctx, s.probeSlots, func() error {
		probe = s.vid.Probe(ctx, tmp)
		plan = s.vid.Plan(ctx, tmp, probe)
		return nil
	}); err != nil {
		return &answer{reason: "Upload error"}
	}
	w, h, ok := 0, 0, false
	if probe != nil {
		w, h, ok = probe.Dimensions()
	}
	if !ok && s.codec != nil {
		return &answer{reason: "Can't get video dimensions"}
	}
	name, fail := s.claim("mp4", w, h)
	if fail != nil {
		return fail
	}
	busy := &answer{hash: name, reason: "System is busy, try again later"}

	switch {
	case plan != nil && plan.Video == "transcode":
		if !s.tryVideoSlot() {
			return busy
		}
		s.releaseVideoSlot()
		ans := s.put(tmp, name)
		if ans.ok {
			s.transcodeInBackground(name, plan)
		}
		return ans
	case plan != nil && plan.Needed:
		if !s.tryVideoSlot() {
			return busy
		}
		defer s.releaseVideoSlot()
		ans := s.put(tmp, name)
		if ans.ok {
			path := s.store.Path(name)
			// detached: the file is published already, finish it even if the client hangs up
			if s.vid.Normalize(context.Background(), path, plan) {
				if sum, err := store.FileSHA1(path); err == nil {
					s.store.AddSHA1(name, sum) // re-uploads of the normalised file dedupe too
				}
			} else {
				log.Printf("remux of %s failed, serving the original (%s)", name, strings.Join(plan.Reasons, "; "))
			}
		}
		return ans
	default:
		return s.put(tmp, name)
	}
}

// transcodeInBackground replaces the stored file with a transcoded one when a slot frees up
// (at most 10 minutes later), then refreshes the S3 copy.
func (s *Server) transcodeInBackground(name string, plan *video.Plan) {
	s.bgWG.Add(1)
	go func() {
		defer s.bgWG.Done()
		select {
		case s.videoSlots <- struct{}{}:
		case <-time.After(10 * time.Minute):
			log.Printf("transcode %s: no conversion slot within 10 minutes, keeping the original", name)
			return
		case <-s.bgCtx.Done():
			return
		}
		defer s.releaseVideoSlot()
		path := s.store.Path(name)
		start := time.Now()
		if !s.vid.Normalize(s.bgCtx, path, plan) {
			return
		}
		log.Printf("transcoded %s in %s (%s)", name, time.Since(start).Round(time.Second), strings.Join(plan.Reasons, "; "))
		if sum, err := store.FileSHA1(path); err == nil {
			s.store.AddSHA1(name, sum)
		}
		s.queue.push(name, start)
	}()
}

// handleGetURL is /api/geturl.php: the server downloads a URL and stores it like an upload.
// Requires the Client-ID secret itself as well (Caddy checks it too).
func (s *Server) handleGetURL(w http.ResponseWriter, r *http.Request) {
	if s.cfg.ClientID == "" || !s.authorized(r) { // a server-side fetch is never open
		s.writeJSONStatus(w, r, http.StatusForbidden, kv{"status", "err"}, kv{"reason", "Access denied"})
		return
	}
	if s.store.ReadOnly {
		s.writeJSONStatus(w, r, http.StatusServiceUnavailable, kv{"status", "err"}, kv{"reason", "Uploads are disabled"})
		return
	}
	_ = r.ParseForm()
	url := strings.TrimSpace(r.Form.Get("url"))
	if url == "" || !strings.HasPrefix(url, "http") {
		s.writeJSON(w, r, kv{"status", "err"}, kv{"reason", "Invalid URL"})
		return
	}
	tmp, err := s.download(r.Context(), url)
	if tmp != "" {
		defer os.Remove(tmp)
	}
	if errors.Is(err, errTooBig) {
		s.tooBig(w, r)
		return
	}
	if err != nil {
		log.Printf("geturl: %v", err)
		s.writeJSON(w, r, kv{"status", "err"}, kv{"reason", "Could not download file"})
		return
	}
	s.ingest(w, r, tmp, func(typ string) bool {
		return slices.Contains(imageExts, typ) || slices.Contains(videoExts, typ) || typ == "text"
	}, func(typ any) []kv {
		return []kv{{"status", "err"}, {"reason", "Unsupported filetype"}, {"filetype", typ}}
	})
}

// handleInfo is /api/info.php.
func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	if r.Form.Get("ip") == "pls" {
		w.Header().Set("Cache-Control", "no-store")
		s.writeBody(w, r, http.StatusOK, "text/html; charset=UTF-8", []byte(clientIP(r)))
		return
	}
	hash := r.Form.Get("hash")
	if !s.store.Exists(hash) {
		s.writeJSON(w, r, kv{"status", "err"}, kv{"reason", "File not found"})
		return
	}
	path := s.store.Path(hash)
	st, err := os.Stat(path)
	if err != nil {
		s.writeJSON(w, r, kv{"status", "err"}, kv{"reason", "File not found"})
		return
	}
	var mimeType any = nil
	if res := proc.Run(r.Context(), proc.Limits{Timeout: 30 * time.Second}, "file", "-bi", path); res.Err == nil {
		out := strings.TrimSpace(string(res.Stdout))
		if strings.Contains(out, "/") && strings.Contains(out, ";") {
			mimeType, _, _ = strings.Cut(out, ";")
		}
	}
	typ, ok := s.fileType(r.Context(), path)
	var typJSON any = typ
	if !ok {
		typJSON = false
	}
	s.writeJSON(w, r, kv{"hash", hash}, kv{"size_bytes", st.Size()}, kv{"size_interpreted", renderSize(st.Size())},
		kv{"type", mimeType}, kv{"type_interpreted", typJSON}, kv{"status", "ok"})
}
