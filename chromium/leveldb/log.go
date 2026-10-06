package leveldb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	glerrors "github.com/syndtr/goleveldb/leveldb/errors"
	"github.com/syndtr/goleveldb/leveldb/journal"
)

type dropCounter struct{ n int }

func (d *dropCounter) Drop(error) { d.n++ }

// readLog replays one write-ahead log into the store. It reports whether the log ended in a
// truncated or corrupt tail, which is expected when the app was writing during the copy.
func readLog(path, name string, into *store) (truncated bool, err error) {
	f, err := os.Open(path) //nolint:gosec // path is built from a manifest-derived file name inside the caller-chosen directory
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, &MissingFileError{Name: name}
		}
		return false, fmt.Errorf("leveldb: open %s: %w", name, err)
	}
	defer func() { _ = f.Close() }()
	return replayLog(f, name, into)
}

// replayLog applies every record read from r, which is the content of the log called name.
func replayLog(r io.Reader, name string, into *store) (truncated bool, err error) {
	dc := &dropCounter{}
	jr := journal.NewReader(r, dc, false, true)
	for {
		rr, nerr := jr.Next()
		if errors.Is(nerr, io.EOF) {
			break
		}
		if nerr != nil {
			// A non-strict journal reader reports damage through the dropper, not from Next,
			// so an error here is a real read failure.
			return false, fmt.Errorf("leveldb: read %s: %w", name, nerr)
		}
		rec, rerr := io.ReadAll(rr)
		if rerr != nil {
			if !isTornRead(rerr) {
				return false, fmt.Errorf("leveldb: read %s: %w", name, rerr)
			}
			truncated = true
			break
		}
		if !applyBatch(rec, into) {
			truncated = true
			break
		}
	}
	if dc.n > 0 {
		truncated = true
	}
	return truncated, nil
}

// isTornRead reports whether a journal read error means the log was cut off or damaged, which is
// the expected result of copying while the app writes. Any other error is a real I/O failure and
// must not be mistaken for a torn tail, or the records after it would be dropped silently.
func isTornRead(err error) bool {
	var jc *journal.ErrCorrupted
	return glerrors.IsCorrupted(err) || errors.As(err, &jc) || errors.Is(err, io.ErrUnexpectedEOF)
}

// applyBatch applies a write batch: 8-byte sequence, 4-byte count, then records. It returns false
// when the batch is malformed; records decoded before the defect are kept.
func applyBatch(b []byte, into *store) bool {
	if len(b) < 12 {
		return false
	}
	seq := binary.LittleEndian.Uint64(b)
	count := binary.LittleEndian.Uint32(b[8:])
	d := &decoder{b: b[12:]}
	for i := uint32(0); i < count; i++ {
		if len(d.b) == 0 {
			return false
		}
		typ := d.b[0]
		d.b = d.b[1:]
		key := d.bytes()
		if d.err != nil {
			return false
		}
		switch typ {
		case typeValue:
			val := d.bytes()
			if d.err != nil {
				return false
			}
			into.put(key, seq+uint64(i), false, val, nil)
		case typeDeletion:
			into.put(key, seq+uint64(i), true, nil, nil)
		default:
			return false
		}
	}
	return true
}
