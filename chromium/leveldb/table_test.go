package leveldb

import (
	"encoding/binary"
	"errors"
	"os"
	"testing"

	"github.com/syndtr/goleveldb/leveldb/util"
)

// TestUnsupportedCompression patches the first data block's trailer type byte to 2 (and fixes
// its checksum so only the compression type is wrong).
func TestUnsupportedCompression(t *testing.T) {
	dir := t.TempDir()
	db := openGL(t, dir)
	_ = db.Put([]byte("k"), []byte("v"), nil)
	if err := db.CompactRange(util.Range{}); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	tabs := listExt(t, dir, ".ldb")
	if len(tabs) != 1 {
		t.Fatalf("want 1 table, got %d", len(tabs))
	}
	b, err := os.ReadFile(tabs[0])
	if err != nil {
		t.Fatal(err)
	}
	// Footer: metaindex handle (offset,len), index handle, padding, 8-byte magic (48 bytes total).
	foot := b[len(b)-48:]
	metaOff, _ := binary.Uvarint(foot)
	// With no filter block the single data block ends 5 bytes (trailer) before the metaindex.
	typeAt := int(metaOff) - 5 //nolint:gosec // small test table
	b[typeAt] = 2
	binary.LittleEndian.PutUint32(b[typeAt+1:], util.NewCRC(b[:typeAt+1]).Value())
	//nolint:gosec // path comes from a t.TempDir glob
	if err := os.WriteFile(tabs[0], b, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Load(dir)
	if !errors.Is(err, ErrUnsupportedCompression) {
		t.Fatalf("err = %v", err)
	}
}
