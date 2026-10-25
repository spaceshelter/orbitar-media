// Package store is PictShare's on-disk layout, shared with the PHP version so that either can run
// on the same volume:
//
//	data/<hash>/<hash>       the original (or normalised) upload
//	data/<hash>/deletecode   32 chars [0-9a-z]
//	data/<hash>/<variant>    generated files (resized images, previews, gif->mp4)
//	data/sha1.csv            "sha1;hash\n" lines, append-only, for dedupe
package store

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spaceshelter/orbitar-media/mediaserver/internal/hashcodec"
)

// Store is the data directory.
type Store struct {
	Root string // data directory

	// ReadOnly never writes to Root (side-by-side evaluation next to the PHP service): files
	// restored from S3 and generated variants go to Overlay instead, which is scratch space.
	ReadOnly bool
	Overlay  string

	// TouchInterval > 0 bumps a hash directory's mtime at most this often when it is served,
	// so the cleanup service (oldest mtime first) evicts by last use instead of by upload time.
	TouchInterval time.Duration
	touched       sync.Map // hash -> time.Time

	sha1Mu sync.Mutex
}

// nameRe: one dot, 2-5 chars after it (PictShare's mightBeAHash), and nothing that could leave
// the data directory.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}\.[A-Za-z0-9-]{2,5}$`)

// ValidName reports whether s has the shape of a stored file name.
func ValidName(s string) bool { return nameRe.MatchString(s) }

// Ext returns the extension of a valid name.
func Ext(hash string) string {
	if i := strings.LastIndexByte(hash, '.'); i >= 0 {
		return hash[i+1:]
	}
	return ""
}

// Dir is the directory holding hash and its variants.
func (s *Store) Dir(hash string) string {
	if s.ReadOnly && !isFile(filepath.Join(s.Root, hash, hash)) {
		return filepath.Join(s.Overlay, hash)
	}
	return filepath.Join(s.Root, hash)
}

// Path is the stored file itself.
func (s *Store) Path(hash string) string { return filepath.Join(s.Dir(hash), hash) }

func (s *Store) writeRoot() string {
	if s.ReadOnly {
		return s.Overlay
	}
	return s.Root
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// Exists reports whether hash is stored locally.
func (s *Store) Exists(hash string) bool {
	return ValidName(hash) && isFile(s.Path(hash))
}

// Touch records a use of hash (see TouchInterval).
func (s *Store) Touch(hash string) {
	if s.ReadOnly || s.TouchInterval <= 0 {
		return
	}
	now := time.Now()
	if v, ok := s.touched.Load(hash); ok && now.Sub(v.(time.Time)) < s.TouchInterval {
		return
	}
	s.touched.Store(hash, now)
	_ = os.Chtimes(s.Dir(hash), now, now)
}

// VariantPath returns where the generated file name for hash lives (or is to be written). In
// read-only mode a variant that already exists in Root is used, new ones go to the overlay.
func (s *Store) VariantPath(hash, name string) string {
	if s.ReadOnly {
		if p := filepath.Join(s.Root, hash, name); isFile(p) {
			return p
		}
		dir := filepath.Join(s.Overlay, hash)
		_ = os.MkdirAll(dir, 0o755)
		return filepath.Join(dir, name)
	}
	return filepath.Join(s.Root, hash, name)
}

// FindVariant returns where the generated file name of hash is, "" when it doesn't exist (unlike
// VariantPath, never creates anything).
func (s *Store) FindVariant(hash, name string) string {
	if !ValidName(hash) || strings.ContainsAny(name, "/\\") {
		return ""
	}
	if p := filepath.Join(s.Root, hash, name); isFile(p) {
		return p
	}
	if p := filepath.Join(s.Overlay, hash, name); s.ReadOnly && isFile(p) {
		return p
	}
	return ""
}

// PutVariant moves src into place as the generated file name of hash (a video preview restored
// from S3, possibly without the video; in read-only mode always to the overlay) and returns
// where it went.
func (s *Store) PutVariant(src, hash, name string) (string, error) {
	if !ValidName(hash) || strings.ContainsAny(name, "/\\") || name == hash {
		return "", fmt.Errorf("invalid variant %q of %q", name, hash)
	}
	dst := filepath.Join(s.writeRoot(), hash, name)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	tmp := TempName(dst)
	defer os.Remove(tmp)
	if err := moveOrCopy(src, tmp); err != nil {
		return "", err
	}
	return dst, os.Rename(tmp, dst)
}

// DropVariants removes what is left in hash's directory when hash itself isn't stored (variants
// of an earlier file under that name), so a new file under the name doesn't inherit them.
func (s *Store) DropVariants(hash string) error {
	if !ValidName(hash) {
		return fmt.Errorf("invalid name %q", hash)
	}
	if s.Exists(hash) {
		return nil
	}
	return os.RemoveAll(filepath.Join(s.writeRoot(), hash))
}

// TempName is a per-call temp name next to target (same filesystem, so rename is atomic).
func TempName(target string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s.%d.%s.tmp", target, os.Getpid(), hex.EncodeToString(b))
}

// WriteAtomic lets gen write to a temp file next to target and renames it into place only when
// gen succeeded and produced a non-empty file, so nobody ever serves a partial or empty one.
func WriteAtomic(target string, gen func(tmp string) error) error {
	tmp := TempName(target)
	defer os.Remove(tmp)
	if err := gen(tmp); err != nil {
		return err
	}
	st, err := os.Stat(tmp)
	if err != nil {
		return err
	}
	if st.Size() == 0 {
		return errors.New("generated an empty file")
	}
	return os.Rename(tmp, target)
}

// ErrExists is returned by Put when the hash is already taken.
var ErrExists = errors.New("hash already exists")

// Put stores src under hash (moving it when possible), creates the delete code and records the
// stored file's sha1. Returns the delete code.
func (s *Store) Put(src, hash string) (string, error) {
	if !ValidName(hash) {
		return "", fmt.Errorf("invalid name %q", hash)
	}
	dir := filepath.Join(s.writeRoot(), hash)
	if err := os.MkdirAll(s.writeRoot(), 0o755); err != nil {
		return "", err
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		if os.IsExist(err) && s.Exists(hash) {
			return "", ErrExists
		}
		if !os.IsExist(err) {
			return "", err
		}
	}
	dst := filepath.Join(dir, hash)
	tmp := TempName(dst)
	defer os.Remove(tmp)
	if err := moveOrCopy(src, tmp); err != nil {
		return "", err
	}
	if st, err := os.Stat(tmp); err != nil {
		return "", err
	} else if st.Size() == 0 {
		return "", errors.New("empty file")
	}
	// link, unlike rename, fails when dst exists: of two writers racing for one name (two
	// uploads, or a restore here and in the PHP service) only one publishes, and only that one
	// writes the delete code
	if err := os.Link(tmp, dst); err != nil {
		if os.IsExist(err) {
			return "", ErrExists
		}
		return "", err
	}
	code := hashcodec.Random(32)
	if err := WriteAtomic(filepath.Join(dir, "deletecode"), func(tmp string) error {
		return os.WriteFile(tmp, []byte(code), 0o644)
	}); err != nil {
		return "", err
	}
	if sum, err := FileSHA1(dst); err == nil {
		s.AddSHA1(hash, sum)
	}
	return code, nil
}

func moveOrCopy(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return os.Chmod(dst, 0o644)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	os.Remove(src)
	return nil
}

// DeleteCode returns the delete code of hash ("" when there is none).
func (s *Store) DeleteCode(hash string) string {
	b, err := os.ReadFile(filepath.Join(s.Dir(hash), "deletecode"))
	if err != nil {
		return ""
	}
	return string(b)
}

// Delete removes hash and all its variants.
func (s *Store) Delete(hash string) error {
	if s.ReadOnly {
		return errors.New("read-only")
	}
	if !ValidName(hash) {
		return fmt.Errorf("invalid name %q", hash)
	}
	return os.RemoveAll(filepath.Join(s.Root, hash))
}

// FileSHA1 returns the hex sha1 of a file.
func FileSHA1(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha1.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *Store) sha1File() string { return filepath.Join(s.Root, "sha1.csv") }

// LookupSHA1 returns the hash first recorded for sum, or "".
func (s *Store) LookupSHA1(sum string) string {
	f, err := os.Open(s.sha1File())
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if len(line) > 41 && line[:40] == sum {
			return strings.TrimSpace(line[41:])
		}
	}
	return ""
}

// AddSHA1 records sum -> hash unless sum is already known. Appends under flock, like the PHP
// version, so lines stay whole when both write.
func (s *Store) AddSHA1(hash, sum string) {
	if s.ReadOnly {
		return
	}
	s.sha1Mu.Lock()
	defer s.sha1Mu.Unlock()
	if s.LookupSHA1(sum) != "" {
		return
	}
	f, err := os.OpenFile(s.sha1File(), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err == nil {
		defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	}
	_, _ = f.WriteString(sum + ";" + hash + "\n")
}
