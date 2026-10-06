package leveldb

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/syndtr/goleveldb/leveldb/journal"
)

func uv(v uint64) []byte { return binary.AppendUvarint(nil, v) }

func lp(b []byte) []byte { return append(uv(uint64(len(b))), b...) }

func writeJournal(t *testing.T, path string, records ...[]byte) {
	t.Helper()
	f, err := os.Create(path) //nolint:gosec // test path under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	w := journal.NewWriter(f)
	for _, r := range records {
		jw, err := w.Next()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := jw.Write(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
}

func batch(seq uint64, kv ...string) []byte {
	b := binary.LittleEndian.AppendUint64(nil, seq)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(kv)/2)) //nolint:gosec // test helper, tiny counts
	for i := 0; i+1 < len(kv); i += 2 {
		b = append(b, typeValue)
		b = append(b, lp([]byte(kv[i]))...)
		b = append(b, lp([]byte(kv[i+1]))...)
	}
	return b
}

// TestPrevLogNumber hand-builds a manifest whose edit sets prev log number 3 and log number 5.
// Log 3 (prev) and 5 are read; log 2 (obsolete) is not.
func TestPrevLogNumber(t *testing.T) {
	dir := t.TempDir()
	edit := []byte{}
	edit = append(edit, uv(tagComparator)...)
	edit = append(edit, lp([]byte("idb_cmp1"))...)
	edit = append(edit, uv(tagLogNumber)...)
	edit = append(edit, uv(5)...)
	edit = append(edit, uv(tagPrevLogNumber)...)
	edit = append(edit, uv(3)...)
	edit = append(edit, uv(tagNextFileNumber)...)
	edit = append(edit, uv(7)...)
	edit = append(edit, uv(tagLastSequence)...)
	edit = append(edit, uv(10)...)
	edit = append(edit, uv(tagCompactPointer)...)
	edit = append(edit, uv(1)...)
	edit = append(edit, lp([]byte("ptr"))...)
	writeJournal(t, filepath.Join(dir, "MANIFEST-000001"), edit)
	if err := os.WriteFile(filepath.Join(dir, "CURRENT"), []byte("MANIFEST-000001\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeJournal(t, filepath.Join(dir, "000002.log"), batch(1, "old", "obsolete"))
	writeJournal(t, filepath.Join(dir, "000003.log"), batch(2, "prev", "p"))
	writeJournal(t, filepath.Join(dir, "000005.log"), batch(3, "cur", "c"))

	d := mustLoad(t, dir)
	if _, ok, _ := d.Get([]byte("prev")); !ok {
		t.Fatal("prev log not read")
	}
	if _, ok, _ := d.Get([]byte("cur")); !ok {
		t.Fatal("current log not read")
	}
	if _, ok, _ := d.Get([]byte("old")); ok {
		t.Fatal("obsolete log was read")
	}
	if st := d.Stats(); st.Logs != 2 || st.Comparator != "idb_cmp1" {
		t.Fatalf("stats %+v", st)
	}
}

func TestManifestDeleteAndAdd(t *testing.T) {
	m := &manifest{tables: map[uint64]struct{}{}}
	add := func(n uint64) []byte {
		e := append(uv(tagNewFile), uv(0)...)
		e = append(e, uv(n)...)
		e = append(e, uv(100)...)
		e = append(e, lp([]byte("a"))...)
		return append(e, lp([]byte("z"))...)
	}
	for _, n := range []uint64{1, 2, 3} {
		if err := m.apply(add(n)); err != nil {
			t.Fatal(err)
		}
	}
	del := append(append(uv(tagDeletedFile), uv(0)...), uv(2)...)
	if err := m.apply(del); err != nil {
		t.Fatal(err)
	}
	if len(m.tables) != 2 {
		t.Fatalf("tables = %v", m.tables)
	}
	if _, ok := m.tables[2]; ok {
		t.Fatal("table 2 should be gone")
	}
	if err := m.apply([]byte{0x7f}); err == nil {
		t.Fatal("unknown tag accepted")
	}
	if err := m.apply(append(uv(tagComparator), 0x80)); err == nil {
		t.Fatal("malformed record accepted")
	}
}
