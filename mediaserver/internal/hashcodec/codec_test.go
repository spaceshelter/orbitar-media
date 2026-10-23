package hashcodec

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// testdata/golden.json was produced by PictShare's getNewCryptoHash/base62_encode on PHP 8.4.
type golden struct {
	Key  string `json:"key"`
	W    int    `json:"w"`
	H    int    `json:"h"`
	Type string `json:"type"`
	Hash string `json:"hash"`
}

func loadGolden(t *testing.T) []golden {
	b, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var g []golden
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func TestGoldenDecodeAndReencode(t *testing.T) {
	for _, g := range loadGolden(t) {
		c, err := New(g.Key)
		if err != nil {
			t.Fatal(err)
		}
		info, err := c.Decode(g.Hash)
		if err != nil {
			t.Fatalf("%s (key %q): %v", g.Hash, g.Key, err)
		}
		if info.Width != g.W || info.Height != g.H || info.Type != g.Type {
			t.Fatalf("%s: got %+v, want %dx%d %s", g.Hash, info, g.W, g.H, g.Type)
		}
		// same iv must give the same bytes PHP produced
		raw, _ := base62Decode(strings.TrimSuffix(g.Hash, "."+g.Type))
		if got := c.encodeWithIV(g.Type, g.W, g.H, raw[len(raw)-8:]); got != g.Hash {
			t.Fatalf("re-encode: got %s, want %s", got, g.Hash)
		}
	}
}

func TestWrongKeyOrExtensionRejected(t *testing.T) {
	g := loadGolden(t)[0]
	other, _ := New(g.Key + "x")
	if _, err := other.Decode(g.Hash); err == nil {
		t.Fatal("decoded with the wrong key")
	}
	c, _ := New(g.Key)
	if _, err := c.Decode(strings.TrimSuffix(g.Hash, g.Type) + "png"); err == nil {
		t.Fatal("extension mismatch accepted")
	}
	for _, bad := range []string{"", "x", "abc.jpg", "../../etc.jpg", "2abc.jpg.jpg", strings.Repeat("z", 200) + ".jpg"} {
		if _, err := c.Decode(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestEncodeRoundTrip(t *testing.T) {
	c, _ := New("roundtrip")
	for _, typ := range []string{"jpg", "png", "gif", "webp", "mp4"} {
		h, err := c.Encode(typ, 4032, 3024)
		if err != nil {
			t.Fatal(err)
		}
		info, err := c.Decode(h)
		if err != nil || info != (Info{4032, 3024, typ}) {
			t.Fatalf("%s: %+v %v", h, info, err)
		}
	}
}

func TestRandom(t *testing.T) {
	r := Random(32)
	if len(r) != 32 || strings.Trim(r, "0123456789abcdefghijklmnopqrstuvwxyz") != "" {
		t.Fatalf("bad random string %q", r)
	}
}
