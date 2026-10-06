package leveldb

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/syndtr/goleveldb/leveldb/util"
)

func keepPrefix(p string) func([]byte) bool {
	return func(k []byte) bool { return bytes.HasPrefix(k, []byte(p)) }
}

// A filtered load keeps exactly the chosen keys, with the same newest-wins and deletion
// resolution across tables and logs as an unfiltered load, and never holds the others' values.
func TestLoadWithKeepFilters(t *testing.T) {
	dir := t.TempDir()
	db := openGL(t, dir)
	put := func(k, v string) {
		t.Helper()
		if err := db.Put([]byte(k), []byte(v), nil); err != nil {
			t.Fatal(err)
		}
	}
	del := func(k string) {
		t.Helper()
		if err := db.Delete([]byte(k), nil); err != nil {
			t.Fatal(err)
		}
	}
	// Table generation 1.
	put("a/over", "old")
	put("a/gone", "v")
	put("a/back", "v1")
	put("a/tabledel", "v")
	put("b/over", "old")
	put("b/gone", "v")
	if err := db.CompactRange(util.Range{}); err != nil {
		t.Fatal(err)
	}
	// Second compaction: deletions merged into the tables, then a revival in the log.
	del("a/back")
	del("a/tabledel")
	put("b/x", "1")
	if err := db.CompactRange(util.Range{}); err != nil {
		t.Fatal(err)
	}
	// Log only.
	put("a/over", "new")
	del("a/gone")
	put("a/back", "v2")
	put("a/new", "fresh")
	put("b/over", "new")
	del("b/gone")
	put("b/y", "2")
	_ = db.Close()

	full := mustLoad(t, dir)
	d := mustLoadWith(t, dir, LoadOptions{Keep: keepPrefix("a/")})

	want := map[string]string{"a/over": "new", "a/back": "v2", "a/new": "fresh"}
	for k, v := range want {
		if got, ok, _ := d.Get([]byte(k)); !ok || string(got) != v {
			t.Errorf("Get(%s) = %q,%v want %q", k, got, ok, v)
		}
		if got, _, _ := full.Get([]byte(k)); string(got) != v {
			t.Errorf("unfiltered Get(%s) = %q want %q", k, got, v)
		}
	}
	for _, k := range []string{"a/gone", "a/tabledel", "b/over", "b/x", "b/y", "b/gone"} {
		if v, ok, _ := d.Get([]byte(k)); ok {
			t.Errorf("Get(%s) = %q, want absent", k, v)
		}
	}
	var scanned, fullScanned []string
	_ = d.Scan(nil, func(k, v []byte) error { scanned = append(scanned, string(k)+"="+string(v)); return nil })
	_ = full.Scan([]byte("a/"), func(k, v []byte) error { fullScanned = append(fullScanned, string(k)+"="+string(v)); return nil })
	if fmt.Sprint(scanned) != fmt.Sprint(fullScanned) {
		t.Errorf("filtered scan %v, unfiltered a/ scan %v", scanned, fullScanned)
	}

	st, fst := d.Stats(), full.Stats()
	if st.Keys != len(want) {
		t.Errorf("Keys = %d want %d", st.Keys, len(want))
	}
	// b/over twice, b/gone put and delete, b/x, b/y: every version of every dropped key counts.
	if st.Skipped != 6 {
		t.Errorf("Skipped = %d want 6", st.Skipped)
	}
	if fst.Skipped != 0 {
		t.Errorf("unfiltered Skipped = %d", fst.Skipped)
	}
	if st.Tables != fst.Tables || st.Logs != fst.Logs || st.Comparator != fst.Comparator || st.TruncatedLogTails != fst.TruncatedLogTails {
		t.Errorf("file stats differ: %+v vs %+v", st, fst)
	}
	for k := range d.entries {
		if !bytes.HasPrefix([]byte(k), []byte("a/")) {
			t.Errorf("store holds dropped key %q", k)
		}
	}
}

// Keep sees user keys only (never internal keys) and a nil Keep is an unfiltered load.
func TestLoadWithKeepSeesUserKeys(t *testing.T) {
	dir := compactedDir(t)
	seen := map[string]bool{}
	d := mustLoadWith(t, dir, LoadOptions{Keep: func(k []byte) bool { seen[string(k)] = true; return false }})
	if len(seen) != 50 || !seen["k7"] {
		t.Fatalf("Keep saw %d distinct keys", len(seen))
	}
	if d.Stats().Keys != 0 || d.Stats().Skipped != 50 {
		t.Fatalf("stats %+v", d.Stats())
	}
	n := mustLoadWith(t, dir, LoadOptions{})
	if n.Stats().Keys != 50 {
		t.Fatalf("nil Keep: %+v", n.Stats())
	}
}
