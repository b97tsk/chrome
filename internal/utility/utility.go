package utility

import (
	"encoding/base64"
)

func DecodeBase64String(s string) ([]byte, error) {
	enc := base64.StdEncoding
	if len(s)%4 != 0 {
		enc = base64.RawStdEncoding
	}
	return enc.DecodeString(s)
}
