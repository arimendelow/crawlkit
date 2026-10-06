package indexeddb

import (
	"errors"
	"fmt"
	"math"
	"time"
	"unicode/utf16"
)

var errShort = errors.New("indexeddb: truncated data")

// readVarint decodes a little-endian base-128 varint and returns the value and bytes used.
func readVarint(b []byte) (uint64, int, error) {
	var v uint64
	for i := 0; i < len(b) && i < 10; i++ {
		v |= uint64(b[i]&0x7f) << (7 * i)
		if b[i]&0x80 == 0 {
			return v, i + 1, nil
		}
	}
	return 0, 0, errShort
}

// readUTF16 reads a varint length (in UTF-16 code units) and that many big-endian units.
func readUTF16(b []byte) (string, int, error) {
	n, used, err := readVarint(b)
	if err != nil {
		return "", 0, err
	}
	if n > uint64(len(b)-used)/2 { //nolint:gosec // bounded by earlier length checks
		return "", 0, errShort
	}
	end := used + int(n)*2 //nolint:gosec // bounded by earlier length checks
	return decodeUTF16BE(b[used:end]), end, nil
}

func decodeUTF16BE(b []byte) string {
	units := make([]uint16, len(b)/2)
	for i := range units {
		units[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
	}
	return string(utf16.Decode(units))
}

// decodeTruncatedInt decodes Chromium's EncodeInt: little-endian, no length.
func decodeTruncatedInt(b []byte) (int64, error) {
	if len(b) == 0 || len(b) > 8 {
		return 0, errShort
	}
	var v uint64
	for i, c := range b {
		v |= uint64(c) << (8 * i)
	}
	return int64(v), nil
}

func byteCount(v uint64) int {
	n := 1
	for v >>= 8; v > 0; v >>= 8 {
		n++
	}
	return n
}

// makePrefix builds the key prefix: a packed length byte, then little-endian
// database, object store and index ids, then any extra bytes.
func makePrefix(db, store, index uint64, end ...byte) ([]byte, error) {
	d, s, i := byteCount(db), byteCount(store), byteCount(index)
	if i > 4 {
		return nil, fmt.Errorf("indexeddb: index id %d too large", index)
	}
	out := []byte{byte((d-1)<<5 | (s-1)<<2 | (i - 1))} //nolint:gosec // bounded by earlier length checks
	for _, p := range []struct {
		v uint64
		n int
	}{{db, d}, {store, s}, {index, i}} {
		for k := 0; k < p.n; k++ {
			out = append(out, byte(p.v>>(8*k))) //nolint:gosec // bounded by earlier length checks
		}
	}
	return append(out, end...), nil
}

// idPrefix is makePrefix for the index ids this package builds itself, which are one byte, so
// the only failure makePrefix has (an index id of more than four bytes) cannot happen.
func idPrefix(db, store uint64, index byte, end ...byte) []byte {
	p, _ := makePrefix(db, store, uint64(index), end...)
	return p
}

// readPrefix parses a key prefix and returns the ids and the prefix length.
func readPrefix(b []byte) (db, store, index uint64, n int, err error) {
	if len(b) < 1 {
		return 0, 0, 0, 0, errShort
	}
	d := int(b[0]>>5&7) + 1
	s := int(b[0]>>2&7) + 1
	i := int(b[0]&3) + 1
	n = 1 + d + s + i
	if len(b) < n {
		return 0, 0, 0, 0, errShort
	}
	le := func(p []byte) (v uint64) {
		for k, c := range p {
			v |= uint64(c) << (8 * k)
		}
		return v
	}
	return le(b[1 : 1+d]), le(b[1+d : 1+d+s]), le(b[1+d+s : n]), n, nil
}

// decodeKey decodes one IndexedDB key and returns it with the bytes consumed.
// Null is nil, String is string, Date is time.Time (UTC), Number is float64,
// Binary is []byte and Array is []any.
func decodeKey(b []byte) (any, int, error) {
	if len(b) == 0 {
		return nil, 0, errShort
	}
	rest := b[1:]
	switch b[0] {
	case 0:
		return nil, 1, nil
	case 1:
		s, n, err := readUTF16(rest)
		return s, 1 + n, err
	case 2, 3:
		if len(rest) < 8 {
			return nil, 0, errShort
		}
		var u uint64
		for i := 0; i < 8; i++ {
			u |= uint64(rest[i]) << (8 * i)
		}
		f := math.Float64frombits(u)
		if b[0] == 3 {
			return f, 9, nil
		}
		return time.UnixMicro(int64(f * 1000)).UTC(), 9, nil
	case 4:
		cnt, used, err := readVarint(rest)
		if err != nil {
			return nil, 0, err
		}
		if cnt > uint64(len(rest)) {
			return nil, 0, errShort
		}
		total := 1 + used
		arr := make([]any, 0, cnt)
		for i := uint64(0); i < cnt; i++ {
			v, n, err := decodeKey(b[total:])
			if err != nil {
				return nil, 0, err
			}
			arr = append(arr, v)
			total += n
		}
		return arr, total, nil
	case 6:
		ln, used, err := readVarint(rest)
		if err != nil {
			return nil, 0, err
		}
		if ln > uint64(len(rest)-used) { //nolint:gosec // bounded by earlier length checks
			return nil, 0, errShort
		}
		out := append([]byte(nil), rest[used:used+int(ln)]...) //nolint:gosec // bounded by earlier length checks
		return out, 1 + used + int(ln), nil                    //nolint:gosec // bounded by earlier length checks
	}
	return nil, 0, fmt.Errorf("indexeddb: unsupported key type %d", b[0])
}
