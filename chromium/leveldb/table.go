package leveldb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/golang/snappy"
	"github.com/syndtr/goleveldb/leveldb/util"
)

const (
	typeDeletion = 0
	typeValue    = 1
)

// ErrUnsupportedCompression is returned when a table block uses a compression type other than none or snappy.
var ErrUnsupportedCompression = errors.New("leveldb: unsupported block compression")

// MissingFileError reports a file named by the manifest (or CURRENT) that is absent from the directory.
type MissingFileError struct{ Name string }

func (e *MissingFileError) Error() string { return "leveldb: missing file " + e.Name }

// entry is the newest known record for a user key. A table value of at least lazyMin bytes is
// not held: loc says where to re-read it (see DB.value). Log values and small values are held.
type entry struct {
	seq     uint64
	deleted bool
	lazy    bool
	value   []byte
	loc     location
}

// location is a value inside a data block of a table: the block's handle and the value's
// offset and length in the block's uncompressed contents.
type location struct {
	table  int // index into DB.tables
	block  blockHandle
	off, n int
}

// lazyMin is the smallest table value that is re-read on demand instead of held in memory.
// The rule is by size only: most metadata and blob entries are small and held, but a large blob
// entry is re-read like any other value, and Get reports a failed re-read as an error.
var lazyMin = 512

// store holds the newest record for every retained key. Keys that keep rejects are never held,
// in any version, so their values are not retained; skipped counts the records dropped.
type store struct {
	m       map[string]entry
	keep    func(key []byte) bool
	observe func(key []byte, valueLen int, fromTable bool)
	skipped int
}

func newStore(keep func(key []byte) bool) *store {
	return &store{m: map[string]entry{}, keep: keep}
}

// put records a version of key if it is retained and newer than what is held. Whether a key is
// retained depends on the key alone, so every version of a retained key is compared here and
// newest-wins and deletions resolve exactly as in an unfiltered load.
//
// loc, when not nil, is where value lives in a table; a large value is then not copied.
func (s *store) put(key []byte, seq uint64, deleted bool, value []byte, loc *location) {
	if s.observe != nil {
		s.observe(key, len(value), loc != nil)
	}
	if s.keep != nil && !s.keep(key) {
		s.skipped++
		return
	}
	if cur, ok := s.m[string(key)]; ok && cur.seq >= seq {
		return
	}
	e := entry{seq: seq, deleted: deleted}
	switch {
	case deleted:
	case loc != nil && len(value) >= lazyMin:
		e.lazy, e.loc = true, *loc
	default:
		e.value = append([]byte(nil), value...)
	}
	s.m[string(key)] = e
}

func splitInternalKey(ik []byte) (user []byte, seq uint64, typ byte, ok bool) {
	if len(ik) < 8 {
		return nil, 0, 0, false
	}
	t := binary.LittleEndian.Uint64(ik[len(ik)-8:])
	return ik[:len(ik)-8], t >> 8, byte(t & 0xff), true
}

// Table format constants (LevelDB table_format.md).
const (
	blockTrailerLen   = 5 // compression type byte, masked CRC-32C
	footerLen         = 48
	tableMagic        = "\x57\xfb\x80\x8b\x24\x75\x47\xdb"
	compressionNone   = 0
	compressionSnappy = 1
)

// maxBlockLen caps a block's uncompressed size. LevelDB blocks are a few KiB (a single large
// value makes one larger block); a snappy header claiming more than this is corrupt, and is
// rejected before its claimed length is allocated.
const maxBlockLen = 64 << 20

// blockHandle locates a block (without its trailer) in a table file.
type blockHandle struct{ off, size uint64 }

func decodeHandle(b []byte) (blockHandle, int, error) {
	off, n := binary.Uvarint(b)
	if n <= 0 {
		return blockHandle{}, 0, errors.New("bad block handle")
	}
	size, m := binary.Uvarint(b[n:])
	if m <= 0 {
		return blockHandle{}, 0, errors.New("bad block handle")
	}
	return blockHandle{off: off, size: size}, n + m, nil
}

// readBlock reads one block, verifies its checksum and returns its uncompressed contents.
func readBlock(f io.ReaderAt, size int64, h blockHandle) ([]byte, error) {
	if h.size > uint64(size) || h.off > uint64(size)-h.size || uint64(size)-h.size-h.off < blockTrailerLen { //nolint:gosec // non-negative, bounded by earlier length checks
		return nil, fmt.Errorf("block handle %d+%d outside the %d-byte file", h.off, h.size, size)
	}
	raw := make([]byte, h.size+blockTrailerLen)
	if _, err := f.ReadAt(raw, int64(h.off)); err != nil { //nolint:gosec // bounded by the file size above
		return nil, err
	}
	n := h.size + 1
	if want, got := binary.LittleEndian.Uint32(raw[n:]), util.NewCRC(raw[:n]).Value(); want != got {
		return nil, fmt.Errorf("block at %d: checksum mismatch", h.off)
	}
	switch t := raw[h.size]; t {
	case compressionNone:
		return raw[:h.size], nil
	case compressionSnappy:
		n, err := snappy.DecodedLen(raw[:h.size])
		if err != nil {
			return nil, fmt.Errorf("block at %d: snappy: %w", h.off, err)
		}
		if n > maxBlockLen {
			return nil, fmt.Errorf("block at %d: decoded length %d exceeds %d", h.off, n, maxBlockLen)
		}
		out, err := snappy.Decode(nil, raw[:h.size])
		if err != nil {
			return nil, fmt.Errorf("block at %d: snappy: %w", h.off, err)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%w: block at %d has type %d", ErrUnsupportedCompression, h.off, t)
	}
}

// blockEntries calls fn for every entry of a block in stored order. key is valid only during
// the call; the value is b[off:off+n].
func blockEntries(b []byte, fn func(key []byte, off, n int) error) error {
	if len(b) < 4 {
		return errors.New("block too short")
	}
	restarts := binary.LittleEndian.Uint32(b[len(b)-4:])
	if uint64(restarts) > uint64(len(b)-4)/4 { //nolint:gosec // non-negative, bounded by earlier length checks
		return errors.New("bad restart count")
	}
	end := len(b) - 4 - 4*int(restarts)
	var key []byte
	for pos := 0; pos < end; {
		var hdr [3]uint64
		for i := range hdr {
			v, n := binary.Uvarint(b[pos:end])
			if n <= 0 {
				return errors.New("bad entry header")
			}
			hdr[i] = v
			pos += n
		}
		shared, unshared, vlen := hdr[0], hdr[1], hdr[2]
		if shared > uint64(len(key)) || unshared > uint64(end-pos) || vlen > uint64(end-pos)-unshared { //nolint:gosec // non-negative, bounded by earlier length checks
			return errors.New("entry overruns block")
		}
		u, n := int(unshared), int(vlen) //nolint:gosec // both fit: bounded by the block length above
		key = append(key[:shared], b[pos:pos+u]...)
		pos += u
		if err := fn(key, pos, n); err != nil {
			return err
		}
		pos += n
	}
	return nil
}

// statFile is f.Stat, a variable so a test can force the failure.
var statFile = func(f *os.File) (os.FileInfo, error) { return f.Stat() }

func openTable(path, name string) (*os.File, int64, error) {
	f, err := os.Open(path) //nolint:gosec // path is built from a manifest-derived file name inside the caller-chosen directory
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, &MissingFileError{Name: name}
		}
		return nil, 0, fmt.Errorf("leveldb: open %s: %w", name, err)
	}
	st, err := statFile(f)
	if err != nil {
		_ = f.Close()
		return nil, 0, fmt.Errorf("leveldb: stat %s: %w", name, err)
	}
	return f, st.Size(), nil
}

// readTable reads every entry of one table, in stored order, into the store. Stored order is
// the database comparator's, which this reader never needs: it walks the index block in order.
func readTable(path, name string, idx int, into *store) error {
	f, size, err := openTable(path, name)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := walkTable(f, size, idx, into); err != nil {
		return fmt.Errorf("leveldb: table %s: %w", name, err)
	}
	return nil
}

func walkTable(f io.ReaderAt, size int64, idx int, into *store) error {
	if size < footerLen {
		return errors.New("file too small for a footer")
	}
	var footer [footerLen]byte
	if _, err := f.ReadAt(footer[:], size-footerLen); err != nil {
		return err
	}
	if string(footer[footerLen-len(tableMagic):]) != tableMagic {
		return errors.New("bad magic number")
	}
	_, n, err := decodeHandle(footer[:]) // metaindex: filters only, not needed
	if err != nil {
		return err
	}
	ih, _, err := decodeHandle(footer[n:])
	if err != nil {
		return err
	}
	index, err := readBlock(f, size, ih)
	if err != nil {
		return fmt.Errorf("index block: %w", err)
	}
	return blockEntries(index, func(_ []byte, off, n int) error {
		h, used, err := decodeHandle(index[off : off+n])
		if err != nil || used != n {
			return errors.New("bad data block handle in the index")
		}
		data, err := readBlock(f, size, h)
		if err != nil {
			return err
		}
		return blockEntries(data, func(ik []byte, off, n int) error {
			user, seq, typ, ok := splitInternalKey(ik)
			if !ok {
				return errors.New("internal key too short")
			}
			into.put(user, seq, typ == typeDeletion, data[off:off+n], &location{table: idx, block: h, off: off, n: n})
			return nil
		})
	})
}
