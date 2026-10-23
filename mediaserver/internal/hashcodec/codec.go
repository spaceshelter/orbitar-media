// Package hashcodec implements PictShare's file name ("hash") formats.
//
// Dimension-encoding hashes, used for every upload when HASH_DIMS_AES_KEY is set:
//
//	base62( "1" || AES-256-CBC(key, iv||iv, "<w>:<h>:<type>") || iv ) + "." + type
//
// with an 8-byte random iv (doubled to the 16-byte AES block), PKCS#7 padding, the key used the
// way OpenSSL does (NUL-padded or truncated to 32 bytes) and GMP's base62 alphabet 0-9A-Za-z.
// Text uploads and setups without a key get short random [0-9a-z] names.
package hashcodec

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

const version = '1'

// Codec encodes and decodes dimension hashes for one key.
type Codec struct {
	block cipher.Block
}

// New returns a codec for key, which is used like OpenSSL uses a passphrase-less raw key:
// NUL-padded to 32 bytes, or truncated.
func New(key string) (*Codec, error) {
	if key == "" {
		return nil, errors.New("empty key")
	}
	k := make([]byte, 32)
	copy(k, key)
	b, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return &Codec{block: b}, nil
}

// Info is what a dimension hash carries.
type Info struct {
	Width, Height int
	Type          string
}

// Encode returns a new hash for the given dimensions and type (the file extension).
func (c *Codec) Encode(typ string, w, h int) (string, error) {
	iv := make([]byte, 8)
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}
	return c.encodeWithIV(typ, w, h, iv), nil
}

func (c *Codec) encodeWithIV(typ string, w, h int, iv []byte) string {
	plain := pkcs7Pad([]byte(fmt.Sprintf("%d:%d:%s", w, h, typ)), aes.BlockSize)
	full := append(append([]byte{}, iv...), iv...)
	ct := make([]byte, len(plain))
	cipher.NewCBCEncrypter(c.block, full).CryptBlocks(ct, plain)

	raw := make([]byte, 0, 1+len(ct)+len(iv))
	raw = append(raw, version)
	raw = append(raw, ct...)
	raw = append(raw, iv...)
	return base62Encode(raw) + "." + typ
}

var plainRe = regexp.MustCompile(`^(\d{1,6}):(\d{1,6}):([a-z0-9-]{2,5})$`)

// Decode checks that hash was produced by this key and returns what it encodes. The extension of
// the hash must match the type inside it.
func (c *Codec) Decode(hash string) (Info, error) {
	name, ext, ok := strings.Cut(hash, ".")
	if !ok || strings.Contains(ext, ".") {
		return Info{}, errors.New("no extension")
	}
	raw, err := base62Decode(name)
	if err != nil {
		return Info{}, err
	}
	// version byte + 1 or 2 cipher blocks + iv
	if len(raw) != 1+16+8 && len(raw) != 1+32+8 || raw[0] != version {
		return Info{}, errors.New("not a dimension hash")
	}
	iv := raw[len(raw)-8:]
	ct := raw[1 : len(raw)-8]
	full := append(append([]byte{}, iv...), iv...)
	plain := make([]byte, len(ct))
	cipher.NewCBCDecrypter(c.block, full).CryptBlocks(plain, ct)
	plain, err = pkcs7Unpad(plain, aes.BlockSize)
	if err != nil {
		return Info{}, err
	}
	m := plainRe.FindSubmatch(plain)
	if m == nil || string(m[3]) != ext {
		return Info{}, errors.New("not a dimension hash")
	}
	w, _ := strconv.Atoi(string(m[1]))
	h, _ := strconv.Atoi(string(m[2]))
	return Info{Width: w, Height: h, Type: ext}, nil
}

// Random returns length random characters from [0-9a-z] (PictShare's getRandomString).
func Random(length int) string {
	const keyspace = "0123456789abcdefghijklmnopqrstuvwxyz"
	b := make([]byte, length)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		// 256 % 36 != 0: the bias is irrelevant for names and delete codes of this length
		b[i] = keyspace[int(b[i])%len(keyspace)]
	}
	return string(b)
}

func base62Encode(b []byte) string {
	n := new(big.Int).SetBytes(b)
	if n.Sign() == 0 {
		return "0"
	}
	base := big.NewInt(62)
	mod := new(big.Int)
	var out []byte
	for n.Sign() > 0 {
		n.DivMod(n, base, mod)
		out = append(out, alphabet[mod.Int64()])
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}

func base62Decode(s string) ([]byte, error) {
	if s == "" || len(s) > 80 {
		return nil, errors.New("bad length")
	}
	n := new(big.Int)
	base := big.NewInt(62)
	for i := 0; i < len(s); i++ {
		d := strings.IndexByte(alphabet, s[i])
		if d < 0 {
			return nil, errors.New("bad base62 digit")
		}
		n.Mul(n, base)
		n.Add(n, big.NewInt(int64(d)))
	}
	return n.Bytes(), nil
}

func pkcs7Pad(b []byte, size int) []byte {
	p := size - len(b)%size
	return append(b, bytes.Repeat([]byte{byte(p)}, p)...)
}

func pkcs7Unpad(b []byte, size int) ([]byte, error) {
	if len(b) == 0 || len(b)%size != 0 {
		return nil, errors.New("bad padding")
	}
	p := int(b[len(b)-1])
	if p == 0 || p > size || p > len(b) {
		return nil, errors.New("bad padding")
	}
	for _, c := range b[len(b)-p:] {
		if int(c) != p {
			return nil, errors.New("bad padding")
		}
	}
	return b[:len(b)-p], nil
}
