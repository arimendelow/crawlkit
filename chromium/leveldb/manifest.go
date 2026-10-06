package leveldb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/syndtr/goleveldb/leveldb/journal"
)

// Version-edit tags from the LevelDB manifest format.
const (
	tagComparator     = 1
	tagLogNumber      = 2
	tagNextFileNumber = 3
	tagLastSequence   = 4
	tagCompactPointer = 5
	tagDeletedFile    = 6
	tagNewFile        = 7
	tagPrevLogNumber  = 9
)

// manifest is the live state obtained by replaying every version edit.
type manifest struct {
	comparator string
	logNumber  uint64
	prevLog    uint64
	tables     map[uint64]struct{}
}

func readCurrent(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, "CURRENT")) //nolint:gosec // dir is the caller-chosen snapshot directory
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", &MissingFileError{Name: "CURRENT"}
		}
		return "", fmt.Errorf("leveldb: read CURRENT: %w", err)
	}
	name := strings.TrimSpace(string(b))
	if name == "" || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("leveldb: invalid CURRENT content")
	}
	return name, nil
}

func readManifest(dir, name string) (*manifest, error) {
	f, err := os.Open(filepath.Join(dir, name)) //nolint:gosec // name comes from CURRENT, validated to contain no separators
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &MissingFileError{Name: name}
		}
		return nil, fmt.Errorf("leveldb: open manifest: %w", err)
	}
	defer func() { _ = f.Close() }()

	m := &manifest{tables: map[uint64]struct{}{}}
	jr := journal.NewReader(f, nil, true, true)
	for {
		r, err := jr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, manifestReadError(name, err)
		}
		rec, err := io.ReadAll(r)
		if err != nil {
			return nil, manifestReadError(name, err)
		}
		if err := m.apply(rec); err != nil {
			return nil, fmt.Errorf("leveldb: manifest %s: %w", name, err)
		}
	}
	return m, nil
}

// manifestReadError classifies a journal read failure: a cut-off record is ErrManifestTruncated.
func manifestReadError(name string, err error) error {
	if isTornRead(err) {
		return fmt.Errorf("%w: %s: %w", ErrManifestTruncated, name, err)
	}
	return fmt.Errorf("leveldb: read manifest %s: %w", name, err)
}

type decoder struct {
	b   []byte
	err error
}

func (d *decoder) uvarint() uint64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Uvarint(d.b)
	if n <= 0 {
		d.err = errors.New("malformed varint")
		return 0
	}
	d.b = d.b[n:]
	return v
}

func (d *decoder) bytes() []byte {
	n := d.uvarint()
	if d.err != nil {
		return nil
	}
	if n > uint64(len(d.b)) {
		d.err = errors.New("length-prefixed field overruns record")
		return nil
	}
	out := d.b[:n]
	d.b = d.b[n:]
	return out
}

func (m *manifest) apply(rec []byte) error {
	d := &decoder{b: rec}
	for len(d.b) > 0 && d.err == nil {
		switch tag := d.uvarint(); tag {
		case tagComparator:
			m.comparator = string(d.bytes())
		case tagLogNumber:
			m.logNumber = d.uvarint()
		case tagPrevLogNumber:
			m.prevLog = d.uvarint()
		case tagNextFileNumber, tagLastSequence:
			d.uvarint()
		case tagCompactPointer:
			d.uvarint() // level
			d.bytes()   // internal key
		case tagDeletedFile:
			d.uvarint() // level
			delete(m.tables, d.uvarint())
		case tagNewFile:
			d.uvarint() // level
			num := d.uvarint()
			d.uvarint() // size
			d.bytes()   // smallest
			d.bytes()   // largest
			if d.err == nil {
				m.tables[num] = struct{}{}
			}
		default:
			if d.err == nil {
				d.err = fmt.Errorf("unknown version-edit tag %d", tag)
			}
		}
	}
	return d.err
}

// ErrManifestTruncated is returned when the MANIFEST ends in the middle of a record, which
// happens when the copy raced a write. Callers retry the copy.
var ErrManifestTruncated = errors.New("leveldb: manifest truncated mid-record")
