package imaging

import (
	"crypto/md5"
	"encoding/hex"
)

func md5hex(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}
