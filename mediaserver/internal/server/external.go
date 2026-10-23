package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// newFetchClient is the HTTP client for geturl: public addresses only (checked on the resolved
// address of every connection, redirects included), bounded time and redirects.
func newFetchClient(allowPrivate bool) *http.Client {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			if allowPrivate {
				return nil
			}
			ap, err := netip.ParseAddrPort(address)
			if err != nil || !publicAddr(ap.Addr()) {
				return fmt.Errorf("refusing to connect to %s", address)
			}
			return nil
		},
	}
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
	}
	return &http.Client{
		Transport: tr,
		Timeout:   2 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return errors.New("redirect to a non-http URL")
			}
			return nil
		},
	}
}

var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64 can reach IPv4 internals
}

func publicAddr(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// download fetches url into a temp file, at most MAX_UPLOAD_SIZE.
func (s *Server) download(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return "", errors.New("not an http URL")
	}
	req.Header.Set("User-Agent", "orbitar-media")
	resp, err := s.fetch.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	if resp.ContentLength > s.maxUploadBytes() {
		return "", errTooBig
	}
	return s.saveLimited(resp.Body)
}

// getJSON fetches a small JSON document (coub, vimeo).
func (s *Server) getJSON(ctx context.Context, url string, v any) (status int, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := s.api.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode == http.StatusOK {
		err = json.Unmarshal(body, v)
	}
	return resp.StatusCode, err
}

const (
	coubTimeoutTTL  = 20 * time.Minute
	coubNotFoundTTL = 30 * 24 * time.Hour
)

func (s *Server) coubCachePath(id string) string {
	root := s.store.Root
	if s.store.ReadOnly {
		root = s.store.Overlay
	}
	return filepath.Join(root, "coub_cache", id+".txt")
}

// handleCoub redirects /coub/<id> to the coub's thumbnail, caching the answer (and failures for a
// while) in data/coub_cache like the PHP version.
func (s *Server) handleCoub(w http.ResponseWriter, r *http.Request, id string) {
	if len(id) < 3 {
		s.writeJSON(w, r, kv{"status", "err"}, kv{"reason", "Invalid URL"})
		return
	}
	cachePath := s.coubCachePath(id)
	readCache := func(p string) (string, bool) {
		st, err := os.Stat(p)
		if err != nil {
			return "", false
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return "", false
		}
		c := strings.TrimSpace(string(b))
		age := time.Since(st.ModTime())
		if c == "TIMEOUT" && age > coubTimeoutTTL || c == "404" && age > coubNotFoundTTL {
			if !s.store.ReadOnly {
				os.Remove(p)
			}
			return "", false
		}
		return c, true
	}
	cached, ok := readCache(cachePath)
	if !ok && s.store.ReadOnly {
		cached, ok = readCache(filepath.Join(s.store.Root, "coub_cache", id+".txt"))
	}
	if ok {
		switch cached {
		case "TIMEOUT":
			w.Header().Set("X-Coub-Cache", "HIT-TIMEOUT")
			s.writeJSONStatus(w, r, http.StatusGatewayTimeout, kv{"status", "err"}, kv{"reason", "Coub temporarily unavailable"})
		case "404":
			w.Header().Set("X-Coub-Cache", "HIT-404")
			s.writeJSONStatus(w, r, http.StatusNotFound, kv{"status", "err"}, kv{"reason", "Coub not found"})
		default:
			w.Header().Set("X-Coub-Cache", "HIT")
			http.Redirect(w, r, cached, http.StatusFound)
		}
		return
	}

	url := "https://coub.com/api/v2/coubs/" + id
	if p := s.cfg.CoubProxyURL; p != "" {
		url = strings.TrimRight(p, "/") + "/" + id
	}
	w.Header().Set("X-Coub-Proxy", fmt.Sprint(s.cfg.CoubProxyURL != ""))
	var data struct {
		Picture string `json:"picture"`
	}
	status, err := s.getJSON(r.Context(), url, &data)
	result := "TIMEOUT"
	switch {
	case status == http.StatusNotFound:
		result = "404"
	case status == http.StatusOK && err == nil && data.Picture != "":
		result = data.Picture
	case status == http.StatusOK && err == nil:
		result = "404" // no picture field: treated as not found
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o777); err == nil {
		if err := os.WriteFile(cachePath, []byte(result), 0o644); err != nil {
			log.Printf("coub cache %s: %v", id, err)
		}
	}
	switch result {
	case "TIMEOUT":
		w.Header().Set("X-Coub-Cache", "MISS-TIMEOUT")
		s.writeJSONStatus(w, r, http.StatusGatewayTimeout, kv{"status", "err"}, kv{"reason", "Coub temporarily unavailable"})
	case "404":
		w.Header().Set("X-Coub-Cache", "MISS-404")
		s.writeJSONStatus(w, r, http.StatusNotFound, kv{"status", "err"}, kv{"reason", "Coub not found"})
	default:
		w.Header().Set("X-Coub-Cache", "MISS")
		http.Redirect(w, r, result, http.StatusFound)
	}
}

// handleVimeo redirects /vimeo/<id> to the video's large thumbnail.
func (s *Server) handleVimeo(w http.ResponseWriter, r *http.Request, id string) {
	if len(id) < 3 {
		s.writeJSON(w, r, kv{"status", "err"}, kv{"reason", "Invalid URL"})
		return
	}
	var data []struct {
		ThumbnailLarge string `json:"thumbnail_large"`
	}
	if status, err := s.getJSON(r.Context(), "https://vimeo.com/api/v2/video/"+id+".json", &data); status == http.StatusOK && err == nil &&
		len(data) > 0 && data[0].ThumbnailLarge != "" {
		http.Redirect(w, r, data[0].ThumbnailLarge, http.StatusFound)
		return
	}
	s.writeJSON(w, r, kv{"status", "err"}, kv{"reason", "Picture not found"})
}
