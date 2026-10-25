// Command mediaserver serves Orbitar's media: uploads, images and their variants, MP4 video and text,
// with the same URLs, storage layout and S3 bucket as the PictShare service it replaces.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spaceshelter/orbitar-media/mediaserver/internal/hashcodec"
	"github.com/spaceshelter/orbitar-media/mediaserver/internal/imaging"
	"github.com/spaceshelter/orbitar-media/mediaserver/internal/s3store"
	"github.com/spaceshelter/orbitar-media/mediaserver/internal/server"
	"github.com/spaceshelter/orbitar-media/mediaserver/internal/store"
	"github.com/spaceshelter/orbitar-media/mediaserver/internal/video"
)

func env(name, def string) string {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
	}
	return def
}

func envInt(name string, def int) int {
	v := env(name, "")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Fatalf("%s: not a number: %q", name, v)
	}
	return n
}

func envBool(name string) bool {
	switch strings.ToLower(env(name, "")) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	dataDir := env("DATA_DIR", "/var/www/data")
	tmpDir := env("TMP_DIR", filepath.Join(os.TempDir(), "mediaserver"))
	readOnly := envBool("READ_ONLY")

	st := &store.Store{Root: dataDir, ReadOnly: readOnly}
	if readOnly {
		// Scratch space for S3 restores and variants; never the shared data directory.
		st.Overlay = env("OVERLAY_DIR", filepath.Join(tmpDir, "overlay"))
		if err := os.RemoveAll(st.Overlay); err != nil {
			log.Fatalf("clearing overlay: %v", err)
		}
		if err := os.MkdirAll(st.Overlay, 0o755); err != nil {
			log.Fatalf("overlay: %v", err)
		}
	} else {
		if err := os.MkdirAll(dataDir, 0o755); err != nil {
			log.Fatalf("data dir: %v", err)
		}
		f, err := os.OpenFile(filepath.Join(dataDir, "sha1.csv"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			log.Fatalf("sha1.csv: %v", err)
		}
		f.Close()
	}
	if m := envInt("TOUCH_ON_ACCESS_MINUTES", 0); m > 0 && !readOnly {
		st.TouchInterval = time.Duration(m) * time.Minute
	}
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		log.Fatalf("tmp dir: %v", err)
	}

	var codec *hashcodec.Codec
	if key := env("HASH_DIMS_AES_KEY", ""); key != "" {
		var err error
		if codec, err = hashcodec.New(key); err != nil {
			log.Fatalf("HASH_DIMS_AES_KEY: %v", err)
		}
	}

	ffmpegMB := envInt("FFMPEG_MEMORY_LIMIT_MB", 2560)
	img := &imaging.Processor{
		MemoryMB:  envInt("VIPS_MEMORY_LIMIT_MB", 1024),
		Timeout:   2 * time.Minute,
		MaxPixels: int64(envInt("MAX_IMAGE_MEGAPIXELS", 200)) * 1_000_000,
		TmpDir:    tmpDir,
	}
	vid := &video.Tools{FFmpeg: "ffmpeg", FFprobe: "ffprobe", MemoryMB: ffmpegMB}

	s3 := s3store.New(s3store.Config{
		Bucket:    env("S3_BUCKET", ""),
		Region:    env("S3_REGION", ""),
		AccessKey: env("S3_ACCESS_KEY", ""),
		SecretKey: env("S3_SECRET_KEY", ""),
		Endpoint:  env("S3_ENDPOINT", ""),
	})

	cfg := server.Config{
		URL:              env("URL", ""),
		Title:            env("TITLE", "PictShare"),
		ClientID:         env("CLIENT_ID", ""),
		MaxUploadMB:      envInt("MAX_UPLOAD_SIZE", 50),
		MasterDeleteCode: env("MASTER_DELETE_CODE", ""),
		MasterDeleteIP:   env("MASTER_DELETE_IP", ""),
		CoubProxyURL:     env("COUB_PROXY_URL", ""),
		StaticDir:        env("STATIC_DIR", "/srv/static"),
		TmpDir:           tmpDir,
		VideoSlots:       envInt("MAX_CONCURRENT_VIDEO_HANDLERS", 2),
		ImageSlots:       envInt("MAX_CONCURRENT_IMAGE_HANDLERS", 2),
	}
	controllers := strings.ToUpper(env("CONTENTCONTROLLERS", ""))
	all := controllers == ""
	cfg.UploadImages = all || strings.Contains(controllers, "IMAGE")
	cfg.UploadVideos = all || strings.Contains(controllers, "VIDEO")
	cfg.UploadText = all || strings.Contains(controllers, "TEXT")

	srv := server.New(cfg, st, s3, codec, img, vid)
	srv.Start()

	hs := &http.Server{
		Addr:              env("LISTEN", ":80"),
		Handler:           srv,
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	log.Printf("mediaserver listening on %s (data %s, read-only %v, s3 %v)", hs.Addr, dataDir, readOnly, s3 != nil)

	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	case s := <-sig:
		log.Printf("%v: shutting down", s)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = hs.Shutdown(ctx)
	srv.Shutdown(ctx)
}
