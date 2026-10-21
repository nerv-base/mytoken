package trae

import (
	"bytes"
	"crypto/aes"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The tests below build their own ciphertext so the decoder can be checked
// without a real Trae installation. encryptPage is the exact inverse of
// cipherKey.decrypt and signs the same bytes cipherKey.verify checks; the
// end-to-end proof that those bytes match real Trae output is a manual
// decryption of the shipped database, which this suite cannot carry.

var (
	testKey  = bytes.Repeat([]byte{0x11}, cipherKeySize)
	testSalt = []byte("0123456789abcdef")
)

// plainPage returns a page whose trailer is free, as SQLCipher leaves it.
func plainPage(seed byte, magic bool) []byte {
	page := make([]byte, cipherPageSize)
	for i := 0; i < cipherUsable; i++ {
		page[i] = seed + byte(i*7)
	}
	if magic {
		copy(page, sqliteMagic)
	}
	return page
}

func cbcEncrypt(t *testing.T, block interface{ Encrypt(dst, src []byte) }, iv, dst, src []byte) {
	t.Helper()
	if len(src) != len(dst) || len(src)%aes.BlockSize != 0 {
		t.Fatalf("cbc: %d bytes into %d", len(src), len(dst))
	}
	prev := append([]byte(nil), iv...)
	for off := 0; off < len(src); off += aes.BlockSize {
		chunk := make([]byte, aes.BlockSize)
		for i := range chunk {
			chunk[i] = src[off+i] ^ prev[i]
		}
		block.Encrypt(chunk, chunk)
		copy(dst[off:], chunk)
		prev = chunk
	}
}

// encryptPage produces one ciphertext page: salt on page 1, ciphertext in the
// usable area, then the IV and finally the HMAC over everything before it.
func encryptPage(t *testing.T, ck *cipherKey, plain []byte, pgno uint32, iv []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(ck.enc)
	if err != nil {
		t.Fatalf("aes: %v", err)
	}
	out := make([]byte, cipherPageSize)
	if pgno == 1 {
		copy(out[:cipherSaltSize], testSalt)
		cbcEncrypt(t, block, iv, out[cipherSaltSize:cipherUsable], plain[cipherSaltSize:cipherUsable])
	} else {
		cbcEncrypt(t, block, iv, out[:cipherUsable], plain[:cipherUsable])
	}
	copy(out[cipherIVOffset:cipherHMACOffset], iv)
	body := out[:cipherHMACOffset]
	if pgno == 1 {
		body = out[cipherSaltSize:cipherHMACOffset]
	}
	m := hmac.New(sha512.New, ck.mac)
	m.Write(body)
	var num [4]byte
	binary.LittleEndian.PutUint32(num[:], pgno)
	m.Write(num[:])
	copy(out[cipherHMACOffset:], m.Sum(nil))
	return out
}

func testKeySchedule(t *testing.T) *cipherKey {
	t.Helper()
	ck, err := newCipherKey(testKey, testSalt)
	if err != nil {
		t.Fatalf("newCipherKey: %v", err)
	}
	return ck
}

// testIV is deterministic so a failure is reproducible.
func testIV(pgno uint32) []byte {
	iv := bytes.Repeat([]byte{byte(pgno)}, cipherIVSize)
	iv[0] = 0xaa
	return iv
}

func encryptDatabase(t *testing.T, plains map[uint32][]byte, pages uint32) []byte {
	t.Helper()
	ck := testKeySchedule(t)
	out := make([]byte, 0, int(pages)*cipherPageSize)
	for pgno := uint32(1); pgno <= pages; pgno++ {
		out = append(out, encryptPage(t, ck, plains[pgno], pgno, testIV(pgno))...)
	}
	return out
}

// frame builds one write-ahead log frame.
func frame(t *testing.T, ck *cipherKey, salt []byte, pgno, dbsize uint32, plain []byte) []byte {
	t.Helper()
	out := make([]byte, walFrameHeaderSize)
	binary.BigEndian.PutUint32(out[0:4], pgno)
	binary.BigEndian.PutUint32(out[4:8], dbsize)
	copy(out[walFrameSaltOffset:walFrameSaltOffset+8], salt)
	return append(out, encryptPage(t, ck, plain, pgno, testIV(pgno))...)
}

func walHeader(salt []byte) []byte {
	out := make([]byte, walHeaderSize)
	binary.BigEndian.PutUint32(out[0:4], walMagicBE)
	binary.BigEndian.PutUint32(out[walPageSizeOffset:walPageSizeOffset+4], cipherPageSize)
	copy(out[walSaltOffset:walSaltOffset+8], salt)
	return out
}

func TestDecodeDatabaseRoundTrip(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, dbName)
	plains := map[uint32][]byte{1: plainPage(1, true), 2: plainPage(2, false), 3: plainPage(3, false)}
	if err := os.WriteFile(dbPath, encryptDatabase(t, plains, 3), 0o600); err != nil {
		t.Fatal(err)
	}

	pages, count, err := decodeDatabase(dbPath, dbPath+"-wal", testKey)
	if err != nil {
		t.Fatalf("decodeDatabase: %v", err)
	}
	if count != 3 {
		t.Fatalf("page count = %d, want 3", count)
	}
	for pgno, want := range plains {
		if got := pages[pgno]; !bytes.Equal(got, want) {
			t.Fatalf("page %d does not round-trip\n got %x\nwant %x", pgno, got[:32], want[:32])
		}
	}

	outPath := filepath.Join(dir, "plain.db")
	if err := writeDatabase(outPath, pages, count); err != nil {
		t.Fatalf("writeDatabase: %v", err)
	}
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := len(plains) * cipherPageSize; len(got) != want {
		t.Fatalf("wrote %d bytes, want %d", len(got), want)
	}
	if !bytes.HasPrefix(got, []byte(sqliteMagic)) {
		t.Fatalf("decoded database does not start with the sqlite header: %x", got[:16])
	}
}

func TestDecodeDatabaseRejectsWrongKey(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, dbName)
	plains := map[uint32][]byte{1: plainPage(1, true), 2: plainPage(2, false)}
	if err := os.WriteFile(dbPath, encryptDatabase(t, plains, 2), 0o600); err != nil {
		t.Fatal(err)
	}
	wrong := bytes.Repeat([]byte{0x22}, cipherKeySize)
	if _, _, err := decodeDatabase(dbPath, dbPath+"-wal", wrong); !errors.Is(err, errBadKey) {
		t.Fatalf("wrong key error = %v, want errBadKey", err)
	}
}

func TestDecodeDatabasePlaintext(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, dbName)
	page := plainPage(1, true)
	if err := os.WriteFile(dbPath, page, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := decodeDatabase(dbPath, dbPath+"-wal", testKey); !errors.Is(err, errNotEncrypted) {
		t.Fatalf("plaintext database error = %v, want errNotEncrypted", err)
	}
}

func TestDecodeDatabaseTruncated(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, dbName)
	if err := os.WriteFile(dbPath, make([]byte, cipherPageSize+cipherPageSize/2), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := decodeDatabase(dbPath, dbPath+"-wal", testKey); err == nil {
		t.Fatal("a file that is not whole pages must not decode")
	}
}

func TestApplyWAL(t *testing.T) {
	ck := testKeySchedule(t)
	salt := []byte{9, 9, 9, 9, 9, 9, 9, 9}
	walSalt := []byte{9, 9, 9, 9, 9, 9, 9, 9}

	newPage4 := plainPage(0x40, false)
	newPage5 := plainPage(0x50, false)
	stalePage6 := plainPage(0x60, false)

	wal := append([]byte(nil), walHeader(walSalt)...)
	wal = append(wal, frame(t, ck, salt, 4, 4, newPage4)...)                             // committed: grows the db to 4 pages
	wal = append(wal, frame(t, ck, salt, 5, 0, newPage5)...)                             // never committed
	wal = append(wal, frame(t, ck, []byte{1, 2, 3, 4, 5, 6, 7, 8}, 6, 6, stalePage6)...) // other log generation

	dir := t.TempDir()
	walPath := filepath.Join(dir, walName)
	if err := os.WriteFile(walPath, wal, 0o600); err != nil {
		t.Fatal(err)
	}

	pages := map[uint32][]byte{1: plainPage(1, true), 2: plainPage(2, false), 3: plainPage(3, false)}
	count, err := ck.applyWAL(walPath, pages, 3)
	if err != nil {
		t.Fatalf("applyWAL: %v", err)
	}
	if count != 4 {
		t.Fatalf("page count = %d, want 4 (the committed dbsize)", count)
	}
	if !bytes.Equal(pages[4], newPage4) {
		t.Fatal("the committed frame did not replace page 4")
	}
	if _, ok := pages[5]; ok {
		t.Fatal("a frame from an unfinished transaction must not be applied")
	}
	if _, ok := pages[6]; ok {
		t.Fatal("a frame from another log generation must not be applied")
	}
}

func TestApplyWALMissing(t *testing.T) {
	ck := testKeySchedule(t)
	pages := map[uint32][]byte{1: plainPage(1, true)}
	count, err := ck.applyWAL(filepath.Join(t.TempDir(), "absent.db-wal"), pages, 1)
	if err != nil {
		t.Fatalf("a missing log is not an error: %v", err)
	}
	if count != 1 {
		t.Fatalf("page count = %d, want 1", count)
	}
}

func TestDecodeKey(t *testing.T) {
	digits := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	for _, tc := range []struct {
		name  string
		in    string
		want  string
		fails bool
	}{
		{name: "bare hex", in: digits, want: digits},
		{name: "sqlcipher literal", in: "x'" + digits + "'", want: digits},
		{name: "with salt", in: "x'" + digits + "00112233445566778899aabbccddeeff'", want: digits},
		{name: "uppercase and spaces", in: " \n" + strings.ToUpper(digits) + "\t", want: digits},
		{name: "too short", in: "abcdef", fails: true},
		{name: "not hex", in: "x'zz" + digits + "'", fails: true},
		{name: "empty", in: "   ", fails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeKey(tc.in)
			if tc.fails {
				if err == nil {
					t.Fatalf("decodeKey(%q) = %x, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("decodeKey: %v", err)
			}
			if hex.EncodeToString(got) != tc.want {
				t.Fatalf("decodeKey = %x, want %s", got, tc.want)
			}
		})
	}
}

func TestNewCipherKeyRejectsShortKey(t *testing.T) {
	if _, err := newCipherKey([]byte("short"), testSalt); err == nil {
		t.Fatal("a key that is not 32 bytes must be rejected")
	}
}

// TestVerifyKey covers the page-1 proof that key capture relies on: the right
// key passes, a wrong one does not, and a plaintext page is named as such.
func TestVerifyKey(t *testing.T) {
	ck := testKeySchedule(t)
	page := encryptPage(t, ck, plainPage(1, true), 1, testIV(1))
	if err := VerifyKey(page, testKey); err != nil {
		t.Fatalf("VerifyKey(right key) = %v", err)
	}
	wrong := bytes.Repeat([]byte{0x22}, cipherKeySize)
	if err := VerifyKey(page, wrong); !errors.Is(err, errBadKey) {
		t.Fatalf("VerifyKey(wrong key) = %v, want errBadKey", err)
	}
	plain := make([]byte, cipherPageSize)
	copy(plain, sqliteMagic)
	if err := VerifyKey(plain, testKey); !errors.Is(err, errNotEncrypted) {
		t.Fatalf("VerifyKey(plaintext page) = %v, want errNotEncrypted", err)
	}
	if err := VerifyKey(page[:100], testKey); err == nil {
		t.Fatal("VerifyKey accepted a short page")
	}
}
