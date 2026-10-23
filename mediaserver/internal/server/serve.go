package server

import (
	"fmt"
	"net/http"
	"os"
	"syscall"
)

// statETag is the strong ETag PictShare sends since #11: inode-mtime-size in hex (Apache's
// recipe), so a file replaced in place gets a new tag.
func statETag(st os.FileInfo) string {
	var ino uint64
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		ino = uint64(sys.Ino)
	}
	return fmt.Sprintf(`"%x-%x-%x"`, ino, st.ModTime().Unix(), st.Size())
}

// serveFile sends path with validators, 304 handling and byte ranges (If-Range included). The
// file is opened first and described by fstat of that handle, so an atomic replacement can never
// split one response across two versions.
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, path, contentType string) {
	f, err := os.Open(path)
	if err != nil {
		s.plainError(w, http.StatusNotFound, "file not found")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		s.plainError(w, http.StatusInternalServerError, "file not readable")
		return
	}
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Cache-Control", "public, max-age=31536000")
	h.Set("ETag", statETag(st))
	http.ServeContent(w, r, "", st.ModTime(), f)
}

// serveDownload is PictShare's /download/: the file as an attachment named after the hash.
func (s *Server) serveDownload(w http.ResponseWriter, r *http.Request, path, name string) {
	f, err := os.Open(path)
	if err != nil {
		s.plainError(w, http.StatusNotFound, "file not found")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		s.plainError(w, http.StatusInternalServerError, "file not readable")
		return
	}
	h := w.Header()
	h.Set("Content-Description", "File Transfer")
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", `attachment; filename="`+name+`"`)
	h.Set("Expires", "0")
	h.Set("Cache-Control", "must-revalidate")
	h.Set("Pragma", "public")
	http.ServeContent(w, r, "", st.ModTime(), f)
}
