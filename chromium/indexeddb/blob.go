package indexeddb

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
)

// unresolvedBlob is the blob number Records writes when a record's blob entry
// is absent or unreadable. No file has this number, so Decode reports blob_missing.
// The index is encoded as unresolvedBlob-index so the detail can name it.
const unresolvedBlob = math.MaxUint64

const maxUnresolvedIndex = 1 << 32

// resolveBlobRef rewrites a kReplaceWithBlob value (ff 11 01 size index) so
// the index becomes the real blob number taken from the record's blob entry.
// An absent or unparsable blob entry leaves an unresolved number (blob_missing
// later); a blob entry that exists but cannot be read is an error.
func (o *Origin) resolveBlobRef(dbID, storeID uint64, rawKey, raw []byte) ([]byte, error) {
	size, index, _, ok := parseBlobRef(raw)
	if !ok {
		return raw, nil
	}
	number := uint64(unresolvedBlob)
	if index < maxUnresolvedIndex {
		number -= index
	}
	v, found, err := o.kv.Get(append(idPrefix(dbID, storeID, indexBlobEntries), rawKey...))
	if err != nil {
		return nil, err
	}
	if found {
		if nums, err := externalObjects(v); err == nil && index < uint64(len(nums)) {
			number = nums[index]
		}
	}
	out := []byte{0xff, 0x11, 0x01}
	out = appendVarint(out, size)
	return appendVarint(out, number), nil
}

func appendVarint(b []byte, n uint64) []byte {
	for n >= 0x80 {
		b = append(b, byte(n)|0x80)
		n >>= 7
	}
	return append(b, byte(n))
}

// parseBlobRef reads ff 11 01 <size> <number>; ok is false when it is malformed.
func parseBlobRef(raw []byte) (size, number uint64, n int, ok bool) {
	if len(raw) < 3 || raw[0] != 0xff {
		return 0, 0, 0, false
	}
	_, used, err := readVarint(raw[1:])
	if err != nil {
		return 0, 0, 0, false
	}
	pos := 1 + used
	if pos >= len(raw) || raw[pos] != 0x01 {
		return 0, 0, 0, false
	}
	pos++
	size, used, err = readVarint(raw[pos:])
	if err != nil {
		return 0, 0, 0, false
	}
	pos += used
	number, used, err = readVarint(raw[pos:])
	if err != nil {
		return 0, 0, 0, false
	}
	return size, number, pos + used, true
}

// readBlob reads <blobDir>/<db id hex>/<(n>>8) as 2 hex digits>/<n hex>.
func (o *Origin) readBlob(dbID int64, number uint64) ([]byte, error) {
	if number > unresolvedBlob-maxUnresolvedIndex {
		return nil, &OmissionError{Omission{Code: CodeBlobMissing, Detail: fmt.Sprintf("blob entry missing for index %d of database %x", unresolvedBlob-number, dbID)}}
	}
	if o.blobDir == "" {
		return nil, &OmissionError{Omission{Code: CodeBlobMissing, Detail: "no blob directory"}}
	}
	path := filepath.Join(o.blobDir, fmt.Sprintf("%x", dbID), fmt.Sprintf("%02x", number>>8), fmt.Sprintf("%x", number))
	b, err := os.ReadFile(path) //nolint:gosec // path is built from numeric ids under the copied blob dir
	if err != nil {
		return nil, &OmissionError{Omission{Code: CodeBlobMissing, Detail: fmt.Sprintf("blob %x of database %x unreadable", number, dbID)}}
	}
	return b, nil
}
