package server

import (
	"bytes"
	"net/http"
	"os"
	"slices"
)

// texts larger than this are read into memory only cap(textSlots) at a time
const bigText = 1 << 20

func (s *Server) handleText(w http.ResponseWriter, r *http.Request, hash string, segs []string) {
	path := s.store.Path(hash)
	switch {
	case slices.Contains(segs, "raw"):
		s.withText(w, r, path, "text/plain; charset=utf-8", func(b []byte) {
			s.writeBody(w, r, http.StatusOK, "text/plain; charset=utf-8", b)
		})
	case slices.Contains(segs, "download"):
		s.serveDownload(w, r, path, hash)
	default:
		s.withText(w, r, path, "text/html; charset=UTF-8", func(b []byte) {
			var out bytes.Buffer
			if err := textTemplate.Execute(&out, map[string]any{"Title": s.cfg.Title, "Hash": hash, "Content": string(b)}); err != nil {
				s.plainError(w, http.StatusInternalServerError, "template error")
				return
			}
			s.writeBody(w, r, http.StatusOK, "text/html; charset=UTF-8", out.Bytes())
		})
	}
}

// withText reads the text file and hands it to render. HEAD answers without reading it (and
// so without a Content-Length).
func (s *Server) withText(w http.ResponseWriter, r *http.Request, path, contentType string, render func([]byte)) {
	st, err := os.Stat(path)
	if err != nil {
		s.plainError(w, http.StatusNotFound, "file not found")
		return
	}
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Type", contentType)
		w.Header().Add("Vary", "Accept-Encoding")
		w.WriteHeader(http.StatusOK)
		return
	}
	if st.Size() > bigText {
		select {
		case s.textSlots <- struct{}{}:
			defer func() { <-s.textSlots }()
		case <-r.Context().Done():
			s.plainError(w, http.StatusServiceUnavailable, "busy")
			return
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		s.plainError(w, http.StatusNotFound, "file not found")
		return
	}
	render(b)
}
