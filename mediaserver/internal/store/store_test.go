package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidName(t *testing.T) {
	for _, ok := range []string{"2BKz24ZtrAsw23yMrFbtGpPwe2u3SwQYCC.jpg", "abc123.txt", "coub_9xYz.jpg", "a.webp", "x.x-png"} {
		if !ValidName(ok) {
			t.Errorf("rejected %q", ok)
		}
	}
	for _, bad := range []string{"", "sha1.csv.bak", "coub_cache", "../x.jpg", "a/b.jpg", "x.j", "x.jpegxx", ".jpg", "x..jpg", "x.jpg/", "x y.jpg"} {
		if ValidName(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestPutLookupDelete(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	src := filepath.Join(t.TempDir(), "up")
	os.WriteFile(src, []byte("hello"), 0o600)
	code, err := s.Put(src, "abc.txt")
	if err != nil || len(code) != 32 {
		t.Fatalf("put: %q %v", code, err)
	}
	if !s.Exists("abc.txt") || s.DeleteCode("abc.txt") != code {
		t.Fatal("not stored")
	}
	if got := s.LookupSHA1("aaf4c61ddcc5e8a2dabede0f3b482cd9aea9434d"); got != "abc.txt" {
		t.Fatalf("sha1 lookup: %q", got)
	}
	s.AddSHA1("other.txt", "aaf4c61ddcc5e8a2dabede0f3b482cd9aea9434d")
	b, _ := os.ReadFile(filepath.Join(s.Root, "sha1.csv"))
	if string(b) != "aaf4c61ddcc5e8a2dabede0f3b482cd9aea9434d;abc.txt\n" {
		t.Fatalf("sha1.csv: %q", b)
	}
	os.WriteFile(src, []byte("x"), 0o600)
	if _, err := s.Put(src, "abc.txt"); err != ErrExists {
		t.Fatalf("second put: %v", err)
	}
	if err := s.Delete("abc.txt"); err != nil || s.Exists("abc.txt") {
		t.Fatal("not deleted")
	}
}

func TestReadOnlyOverlay(t *testing.T) {
	s := &Store{Root: t.TempDir(), Overlay: filepath.Join(t.TempDir(), "overlay"), ReadOnly: true}
	os.MkdirAll(filepath.Join(s.Root, "a.jpg"), 0o755)
	os.WriteFile(filepath.Join(s.Root, "a.jpg", "a.jpg"), []byte("x"), 0o644)
	if p := s.VariantPath("a.jpg", "v_a.jpg"); filepath.Dir(filepath.Dir(p)) != s.Overlay {
		t.Fatalf("read-only miss went to %s", p)
	}
	os.WriteFile(filepath.Join(s.Root, "a.jpg", "v_a.jpg"), []byte("x"), 0o644)
	if p := s.VariantPath("a.jpg", "v_a.jpg"); filepath.Dir(p) != filepath.Join(s.Root, "a.jpg") {
		t.Fatal("existing variant not used")
	}
	src := filepath.Join(t.TempDir(), "up")
	os.WriteFile(src, []byte("restored"), 0o644)
	if _, err := s.Put(src, "b.jpg"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Root, "b.jpg")); err == nil {
		t.Fatal("wrote to the read-only root")
	}
	if !s.Exists("b.jpg") || filepath.Dir(s.Path("b.jpg")) != filepath.Join(s.Overlay, "b.jpg") {
		t.Fatal("overlay file not found")
	}
	if _, err := os.Stat(filepath.Join(s.Root, "sha1.csv")); err == nil {
		t.Fatal("sha1.csv written in read-only mode")
	}
}
