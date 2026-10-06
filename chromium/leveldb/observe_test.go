package leveldb

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/syndtr/goleveldb/leveldb/util"
)

// Observe sees every record Keep rejects, with the right table-or-log origin and value length.
func TestLoadObserveSeesRejectedKeys(t *testing.T) {
	dir := t.TempDir()
	db := openGL(t, dir)
	big := make([]byte, lazyMin+10)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(db.Put([]byte("tab1"), []byte("v1"), nil))
	must(db.Put([]byte("tab2"), big, nil))
	must(db.CompactRange(util.Range{}))
	must(db.Put([]byte("log1"), []byte("abc"), nil))
	must(db.Delete([]byte("log2"), nil))
	must(db.Close())

	type seen struct {
		n         int
		fromTable bool
	}
	got := map[string]seen{}
	d := mustLoadWith(t, dir, LoadOptions{
		Keep:    func([]byte) bool { return false },
		Observe: func(k []byte, n int, ft bool) { got[string(k)] = seen{n, ft} },
	})
	want := map[string]seen{
		"tab1": {2, true}, "tab2": {len(big), true}, "log1": {3, false}, "log2": {0, false},
	}
	if len(got) != len(want) {
		t.Fatalf("observed %v, want %v", got, want)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: observed %v, want %v", k, got[k], w)
		}
	}
	if d.Stats().Keys != 0 || d.Stats().Skipped != 4 {
		t.Errorf("stats %+v: Keep should have rejected all 4", d.Stats())
	}
}

// Nil Observe is a plain load.
func TestLoadObserveNilIsPlain(t *testing.T) {
	dir := t.TempDir()
	db := openGL(t, dir)
	_ = db.Put([]byte("k"), []byte("v"), nil)
	_ = db.Close()
	d := mustLoadWith(t, dir, LoadOptions{})
	if d.Stats().Keys != 1 {
		t.Fatalf("load: %+v", d)
	}
}

// EntryOverhead matches the measured live heap per retained entry.
func TestEntryOverheadCalibrated(t *testing.T) {
	const n = 50000
	dir := t.TempDir()
	db := openGL(t, dir)
	payload := 0
	for i := 0; i < n; i++ {
		// Odd lengths (23 and 37 bytes) so allocator size-class rounding is in the measurement.
		k := fmt.Sprintf("key-%019d", i)
		v := fmt.Sprintf("val-%033d", i)
		payload += len(k) + len(v)
		if err := db.Put([]byte(k), []byte(v), nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.CompactRange(util.Range{}); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	d := mustLoad(t, dir)
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(d)
	if d.Stats().Keys != n {
		t.Fatalf("keys %d", d.Stats().Keys)
	}
	per := (float64(after.HeapAlloc) - float64(before.HeapAlloc) - float64(payload)) / n
	t.Logf("measured %.1f overhead bytes per entry; EntryOverhead = %d", per, EntryOverhead)
	// Under-estimating is the dangerous direction: allow no more than 5% below the measurement
	// and up to 50% above it.
	if lo, hi := per*0.95, per*1.5; float64(EntryOverhead) < lo || float64(EntryOverhead) > hi {
		t.Errorf("EntryOverhead = %d, measured %.1f per entry (allowed %.1f to %.1f)", EntryOverhead, per, lo, hi)
	}
}

func TestLazyMinIsTheThreshold(t *testing.T) {
	if LazyMin() != lazyMin {
		t.Errorf("LazyMin() = %d, lazyMin = %d", LazyMin(), lazyMin)
	}
}
