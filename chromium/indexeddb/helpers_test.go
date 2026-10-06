package indexeddb

import (
	"bytes"
	"os"
	"sort"
	"strings"
	"unicode/utf16"

	"github.com/openclaw/crawlkit/chromium/leveldb"
)

// fakeKV is an in-memory key-value store that satisfies the kv interface.
type fakeKV map[string][]byte

func (f fakeKV) Get(k []byte) ([]byte, bool, error) {
	v, ok := f[string(k)]
	return v, ok, nil
}

func (f fakeKV) Scan(prefix []byte, fn func(k, v []byte) error) error {
	keys := make([]string, 0, len(f))
	for k := range f {
		if strings.HasPrefix(k, string(prefix)) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := fn([]byte(k), f[k]); err != nil {
			return err
		}
	}
	return nil
}

func (f fakeKV) Close() error { return nil }

func newTestOrigin(f fakeKV, blobDir string) *Origin {
	return &Origin{kv: f, blobDir: blobDir, stats: leveldb.Stats{Keys: len(f)}}
}

func varint(n uint64) []byte {
	var out []byte
	for n >= 0x80 {
		out = append(out, byte(n)|0x80)
		n >>= 7
	}
	return append(out, byte(n))
}

func u16(s string) []byte {
	var out []byte
	for _, u := range utf16.Encode([]rune(s)) {
		out = append(out, byte(u>>8), byte(u)) //nolint:gosec // bounded by earlier length checks
	}
	return out
}

func idbString(s string) []byte {
	return bytes.Join([][]byte{varint(uint64(len(utf16.Encode([]rune(s))))), u16(s)}, nil)
}

func dbNameKey(origin, name string) []byte {
	return bytes.Join([][]byte{{0, 0, 0, 0, 0xc9}, idbString(origin), idbString(name)}, nil)
}

func storeNameKey(db, store byte) []byte {
	return []byte{0x00, db, 0, 0, 50, store, 0}
}

func removeFile(p string) error { return os.Remove(p) }
