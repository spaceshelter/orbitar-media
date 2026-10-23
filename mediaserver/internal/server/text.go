package server

import (
	"bytes"
	"net/http"
	"os"
	"slices"
)

func (s *Server) handleText(w http.ResponseWriter, r *http.Request, hash string, segs []string) {
	path := s.store.Path(hash)
	switch {
	case slices.Contains(segs, "raw"):
		b, err := os.ReadFile(path)
		if err != nil {
			s.plainError(w, http.StatusNotFound, "file not found")
			return
		}
		s.writeBody(w, r, http.StatusOK, "text/plain; charset=utf-8", b)
	case slices.Contains(segs, "download"):
		s.serveDownload(w, r, path, hash)
	default:
		b, err := os.ReadFile(path)
		if err != nil {
			s.plainError(w, http.StatusNotFound, "file not found")
			return
		}
		var out bytes.Buffer
		if err := textTemplate.Execute(&out, map[string]any{"Title": s.cfg.Title, "Hash": hash, "Content": string(b)}); err != nil {
			s.plainError(w, http.StatusInternalServerError, "template error")
			return
		}
		s.writeBody(w, r, http.StatusOK, "text/html; charset=UTF-8", out.Bytes())
	}
}
