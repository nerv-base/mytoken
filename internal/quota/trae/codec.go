package trae

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
)

// errBadCodec reports a stored value that is not in the client's codec.
var errBadCodec = errors.New("trae: unrecognized credential codec")

// codecSalts are the two 64-byte salts of the client's codec, keyed by the hex
// of the six-byte header. The values are the byte-wise XOR of the two constant
// arrays in packages/trae/index.mjs (CODEC at lines 868-878).
var codecSalts = map[string][64]byte{
	"746305100000": {
		77, 212, 194, 230, 184, 49, 98, 9, 14, 82, 179, 199, 166, 115, 59, 164,
		28, 178, 70, 43, 130, 154, 181, 138, 25, 107, 57, 219, 87, 23, 117, 36,
		244, 155, 175, 127, 8, 232, 214, 141, 38, 167, 46, 55, 193, 169, 90, 47,
		31, 5, 165, 24, 146, 174, 242, 148, 151, 50, 182, 42, 56, 170, 221, 88,
	},
	"123920200203": {
		73, 12, 194, 18, 146, 176, 93, 12, 192, 108, 203, 233, 31, 185, 46, 33,
		181, 167, 197, 56, 129, 29, 200, 94, 39, 169, 110, 40, 16, 78, 28, 246,
		156, 113, 102, 239, 171, 35, 40, 7, 57, 13, 167, 138, 139, 145, 228, 156,
		68, 180, 154, 67, 254, 185, 65, 161, 228, 111, 38, 86, 230, 19, 228, 123,
	},
}

// clientDecode undoes the client's codec: a six-byte salt id, 32 random bytes,
// then AES-128-CBC under sha512(sha512(random)||salt). The plaintext is led by
// its own sha512, which is verified before the body is returned.
func clientDecode(b64 string) ([]byte, error) {
	buf, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil || len(buf) <= 102 {
		return nil, errBadCodec
	}
	salt, ok := codecSalts[hex.EncodeToString(buf[:6])]
	if !ok {
		return nil, errBadCodec
	}
	inner := sha512.Sum512(buf[6:38])
	outer := sha512.New()
	outer.Write(inner[:])
	outer.Write(salt[:])
	key := outer.Sum(nil)

	block, err := aes.NewCipher(key[:16])
	if err != nil {
		return nil, errBadCodec
	}
	out := make([]byte, len(buf)-38)
	cipher.NewCBCDecrypter(block, key[16:32]).CryptBlocks(out, buf[38:])
	out, err = pkcs7Unpad(out, aes.BlockSize)
	if err != nil || len(out) < 64 {
		return nil, errBadCodec
	}
	body := out[64:]
	sum := sha512.Sum512(body)
	if subtle.ConstantTimeCompare(sum[:], out[:64]) != 1 {
		return nil, errBadCodec
	}
	return body, nil
}

func pkcs7Unpad(b []byte, size int) ([]byte, error) {
	if len(b) == 0 || len(b)%size != 0 {
		return nil, errBadCodec
	}
	n := int(b[len(b)-1])
	if n == 0 || n > size || n > len(b) {
		return nil, errBadCodec
	}
	for _, c := range b[len(b)-n:] {
		if int(c) != n {
			return nil, errBadCodec
		}
	}
	return b[:len(b)-n], nil
}
