package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spaceshelter/orbitar-media/mediaserver/internal/imaging"
	"github.com/spaceshelter/orbitar-media/mediaserver/internal/store"
)

var sizeRe = regexp.MustCompile(`^([0-9]{1,5})(?:x([0-9]{1,5}))?$`)

// parseSize accepts PictShare's size segments: "300" or "300x200" (positive integers).
func parseSize(seg string) (imaging.Size, bool) {
	m := sizeRe.FindStringSubmatch(seg)
	if m == nil {
		return imaging.Size{}, false
	}
	w, _ := strconv.Atoi(m[1])
	h := w
	if m[2] != "" {
		h, _ = strconv.Atoi(m[2])
	}
	if w < 1 || h < 1 {
		return imaging.Size{}, false
	}
	return imaging.Size{W: w, H: h}, true
}

func isRotation(seg string) bool { return seg == "upside" || seg == "left" || seg == "right" }

// imageModifiers reads the URL like PictShare's ImageController: size and rotation keep the
// position of their first appearance and the value of their last; filter segments are ignored
// (the colour filters are gone, the picture is served without them).
func imageModifiers(segs []string, typ string) imaging.Modifiers {
	var m imaging.Modifiers
	for _, seg := range segs {
		if sz, ok := parseSize(seg); ok {
			if !slices.Contains(m.Ops, "size") {
				m.Ops = append(m.Ops, "size")
			}
			m.Size, m.SizeRaw = sz, seg
		} else if isRotation(seg) {
			if !slices.Contains(m.Ops, "rotation") {
				m.Ops = append(m.Ops, "rotation")
			}
			m.Rotation = seg
		}
	}
	m.WebP = slices.Contains(segs, "webp") && typ != "webp"
	m.ForceSize = slices.Contains(segs, "forcesize") && m.SizeRaw != ""
	return m
}

func imageContentType(typ string) string {
	switch typ {
	case "jpg", "jpeg":
		return "image/jpeg"
	case "png", "x-png":
		return "image/png"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	}
	return "application/octet-stream"
}

func (s *Server) handleImage(w http.ResponseWriter, r *http.Request, hash string, segs []string) {
	typ := store.Ext(hash)
	path := s.store.Path(hash)

	if typ == "gif" {
		// animated GIFs are served untouched; /mp4/ turns them into a video
		if slices.Contains(segs, "mp4") {
			s.handleGifMP4(w, r, hash, segs)
			return
		}
		s.serveFile(w, r, path, imageContentType(typ))
		return
	}

	m := imageModifiers(segs, typ)
	if m.Empty() {
		s.serveFile(w, r, path, imageContentType(typ))
		return
	}
	outType := typ
	if m.WebP {
		outType = "webp"
	}
	variant := s.store.VariantPath(hash, m.VariantName(hash))
	if !fileExists(variant) {
		if err := s.generate(r.Context(), variant, func(ctx context.Context, tmp string) error {
			return s.img.Variant(ctx, path, tmp, outType, m)
		}); err != nil {
			log.Printf("image variant %s of %s: %v", variant, hash, err)
			msg := "could not encode image"
			if errors.Is(err, imaging.ErrDecode) {
				msg = "could not decode image"
			}
			s.plainError(w, http.StatusInternalServerError, msg)
			return
		}
	}
	s.serveFile(w, r, variant, imageContentType(outType))
}

// generate renders a cached file once: one generator per target, bounded by the image slots, and
// written atomically. The work is detached from the request, so a client hanging up doesn't waste
// a half-done render that the next request would have to repeat.
func (s *Server) generate(ctx context.Context, target string, gen func(ctx context.Context, tmp string) error) error {
	unlock := s.generating.Lock(target)
	defer unlock()
	if fileExists(target) {
		return nil
	}
	select {
	case s.imageSlots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(2 * time.Minute):
		return errors.New("timed out waiting for a render slot")
	}
	defer func() { <-s.imageSlots }()
	return store.WriteAtomic(target, func(tmp string) error {
		return gen(context.Background(), tmp)
	})
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// handleGifMP4 serves /mp4/<hash>.gif: the GIF converted to H.264, its raw file, a preview frame,
// a download or the player page.
func (s *Server) handleGifMP4(w http.ResponseWriter, r *http.Request, hash string, segs []string) {
	mp4 := s.store.VariantPath(hash, hash+"mp4")
	if !fileExists(mp4) {
		err := func() error {
			unlock := s.generating.Lock(mp4)
			defer unlock()
			if fileExists(mp4) {
				return nil
			}
			if !s.tryVideoSlot() {
				return errors.New("all conversion slots busy")
			}
			defer s.releaseVideoSlot()
			return s.vid.GifToMP4(context.Background(), s.store.Path(hash), mp4)
		}()
		if err != nil {
			log.Printf("gif->mp4 %s: %v", hash, err)
			s.plainError(w, http.StatusServiceUnavailable, "could not convert gif")
			return
		}
	}
	switch {
	case slices.Contains(segs, "raw"):
		s.serveFile(w, r, mp4, "video/mp4")
	case slices.Contains(segs, "preview"):
		preview := mp4 + "_preview.jpg"
		if err := s.generate(r.Context(), preview, func(ctx context.Context, tmp string) error {
			return s.vid.FirstFrame(ctx, mp4, tmp, nil)
		}); err != nil {
			log.Printf("preview %s: %v", preview, err)
			s.plainError(w, http.StatusServiceUnavailable, "could not render preview")
			return
		}
		s.serveFile(w, r, preview, "image/jpeg")
	case slices.Contains(segs, "download"):
		s.serveDownload(w, r, mp4, hash+"mp4")
	default:
		s.videoPage(w, r, hash, strings.Join(segs, "/"), mp4)
	}
}
