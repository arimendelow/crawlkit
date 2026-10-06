package indexeddb

import (
	"bytes"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/openclaw/crawlkit/chromium/v8"
)

// keepManagers keeps the databases whose manager is one of the three a caller decodes.
func keepManagers(name string) bool {
	for _, m := range []string{"chats", "convs", "activity"} {
		if strings.HasPrefix(name, "app:"+m+":") {
			return true
		}
	}
	return false
}

func openFiltered(t testing.TB, keep func(string) bool) *Origin {
	t.Helper()
	ldb, blob := fixtureDirs(t)
	o, err := OpenWith(ldb, blob, OpenOptions{KeepDatabase: keep})
	if err != nil {
		t.Fatalf("OpenWith fixture: %v", err)
	}
	t.Cleanup(func() { _ = o.Close() })
	return o
}

type recordDump struct {
	Key       string
	Raw       []byte
	Err       string
	Canonical []byte
	DecodeErr string
}

func dumpStore(t *testing.T, o *Origin, dbID, storeID int64) []recordDump {
	t.Helper()
	var out []recordDump
	err := o.Records(dbID, storeID, func(r Record) error {
		d := recordDump{Key: fmt.Sprintf("%#v", r.Key), Raw: append([]byte(nil), r.Raw...)}
		if r.Err != nil {
			d.Err = r.Err.Error()
			out = append(out, d)
			return nil
		}
		v, err := o.Decode(dbID, r.Raw)
		if err != nil {
			d.DecodeErr = err.Error()
		} else if d.Canonical, err = v8.Canonical(v); err != nil {
			return err
		}
		out = append(out, d)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Keeping only the allowlisted databases changes nothing a reader of those databases sees:
// the database list is identical and every record decodes to the same bytes. Databases that
// are not kept yield no records, and their values are never held.
func TestOpenWithKeepDatabaseMatchesUnfiltered(t *testing.T) {
	full := openFixture(t)
	filt := openFiltered(t, keepManagers)

	fdbs, err := full.Databases()
	if err != nil {
		t.Fatal(err)
	}
	gdbs, err := filt.Databases()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fdbs, gdbs) {
		t.Fatalf("Databases differ:\n%+v\n%+v", fdbs, gdbs)
	}

	kept, dropped, records := 0, 0, 0
	for _, db := range fdbs {
		for _, s := range db.Stores {
			want := dumpStore(t, full, db.ID, s.ID)
			got := dumpStore(t, filt, db.ID, s.ID)
			if !keepManagers(db.Name) {
				if len(got) != 0 {
					t.Errorf("database %d store %d not kept but yielded %d records", db.ID, s.ID, len(got))
				}
				dropped += len(want)
				continue
			}
			kept++
			records += len(want)
			if !reflect.DeepEqual(want, got) {
				t.Errorf("database %d store %d: filtered records differ from unfiltered", db.ID, s.ID)
			}
			for i := range want {
				if i < len(got) && !bytes.Equal(want[i].Canonical, got[i].Canonical) {
					t.Errorf("database %d store %d record %d: decoded bytes differ", db.ID, s.ID, i)
				}
			}
		}
	}
	if kept == 0 || records < 80 || dropped == 0 {
		t.Fatalf("fixture coverage too thin: %d kept stores, %d records, %d dropped records", kept, records, dropped)
	}

	fs, gs := full.Stats(), filt.Stats()
	if gs.Skipped == 0 || fs.Skipped != 0 {
		t.Errorf("Skipped: filtered %d, unfiltered %d", gs.Skipped, fs.Skipped)
	}
	if gs.Keys >= fs.Keys || gs.Keys == 0 {
		t.Errorf("Keys: filtered %d, unfiltered %d", gs.Keys, fs.Keys)
	}
	if gs.Tables != fs.Tables || gs.Logs != fs.Logs || gs.TruncatedLogTails != fs.TruncatedLogTails {
		t.Errorf("file stats differ: %+v vs %+v", gs, fs)
	}
}

// A nil KeepDatabase is the unfiltered Open.
func TestOpenWithNilKeepIsOpen(t *testing.T) {
	full := openFixture(t)
	o := openFiltered(t, nil)
	if !reflect.DeepEqual(full.Stats(), o.Stats()) {
		t.Fatalf("stats %+v vs %+v", o.Stats(), full.Stats())
	}
}

// The decoy auth database is never loaded when it is not kept: no key under its id is held.
func TestOpenWithNeverHoldsDroppedDatabase(t *testing.T) {
	o := openFiltered(t, keepManagers)
	dbs, err := o.Databases()
	if err != nil {
		t.Fatal(err)
	}
	for _, db := range dbs {
		if !strings.HasPrefix(db.Name, "app:auth:") {
			continue
		}
		prefix, err := makePrefix(uint64(db.ID), 0, 0) //nolint:gosec // fixture ids are small
		if err != nil {
			t.Fatal(err)
		}
		held := 0
		_ = o.kv.Scan(prefix[:1+byteCount(uint64(db.ID))], func(k, _ []byte) error { //nolint:gosec // fixture ids are small
			if _, s, i, _, err := readPrefix(k); err == nil && (s != 0 || i != 0) {
				held++
			}
			return nil
		})
		if held != 0 {
			t.Errorf("auth database %d: %d data keys held", db.ID, held)
		}
		return
	}
	t.Fatal("fixture lost its decoy auth database")
}

// measure returns the bytes allocated while opening and the bytes still live afterwards.
func measure(t testing.TB, open func() *Origin) (total, retained uint64) {
	t.Helper()
	var before, after, live runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	o := open()
	runtime.ReadMemStats(&after)
	runtime.GC()
	runtime.ReadMemStats(&live)
	runtime.KeepAlive(o)
	return after.TotalAlloc - before.TotalAlloc, live.HeapAlloc - min(live.HeapAlloc, before.HeapAlloc)
}

// heldBytes sums the key and value bytes the origin's LevelDB holds.
func heldBytes(o *Origin) (n int) {
	_ = o.kv.Scan(nil, func(k, v []byte) error { n += len(k) + len(v); return nil })
	return n
}

// Reports allocated and live heap bytes for an unfiltered and a filtered open of the fixture,
// and requires the filtered open to hold fewer key and value bytes.
func TestOpenWithRetainsLess(t *testing.T) {
	var full, filt *Origin
	fullTotal, fullLive := measure(t, func() *Origin { full = openFixture(t); return full })
	filtTotal, filtLive := measure(t, func() *Origin { filt = openFiltered(t, keepManagers); return filt })
	fh, gh := heldBytes(full), heldBytes(filt)
	t.Logf("fixture open: unfiltered allocated %d bytes, live heap %d, holds %d key+value bytes in %d keys; filtered allocated %d bytes, live heap %d, holds %d bytes in %d keys (%d records skipped)",
		fullTotal, fullLive, fh, full.Stats().Keys, filtTotal, filtLive, gh, filt.Stats().Keys, filt.Stats().Skipped)
	if gh >= fh {
		t.Errorf("filtered open holds %d bytes, unfiltered %d", gh, fh)
	}
}

func BenchmarkOpenFixture(b *testing.B) {
	for _, c := range []struct {
		name string
		keep func(string) bool
	}{{"unfiltered", nil}, {"allowlisted", keepManagers}} {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				openFiltered(b, c.keep)
			}
		})
	}
}

// KeepStore keeps exactly the named object stores, reading the LevelDB once: the kept stores
// decode as in the unfiltered origin and every other store yields nothing.
func TestOpenWithKeepStoreMatchesUnfiltered(t *testing.T) {
	full := openFixture(t)
	dbs, err := full.Databases()
	if err != nil {
		t.Fatal(err)
	}
	keep := map[[2]int64]bool{}
	for _, db := range dbs {
		if keepManagers(db.Name) && len(db.Stores) > 0 {
			keep[[2]int64{db.ID, db.Stores[0].ID}] = true
		}
	}
	ldb, blob := fixtureDirs(t)
	o, err := OpenWith(ldb, blob, OpenOptions{KeepStore: func(d, s int64) bool { return keep[[2]int64{d, s}] }})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := o.Databases(); err != nil || !reflect.DeepEqual(got, dbs) {
		t.Fatalf("Databases = %+v, %v", got, err)
	}
	kept, dropped := 0, 0
	for _, db := range dbs {
		for _, s := range db.Stores {
			want, got := dumpStore(t, full, db.ID, s.ID), dumpStore(t, o, db.ID, s.ID)
			if keep[[2]int64{db.ID, s.ID}] {
				kept += len(want)
				if !reflect.DeepEqual(want, got) {
					t.Errorf("db %d store %d: kept store differs", db.ID, s.ID)
				}
			} else {
				dropped += len(want)
				if len(got) != 0 {
					t.Errorf("db %d store %d: not kept but yielded %d records", db.ID, s.ID, len(got))
				}
			}
		}
	}
	if kept == 0 || dropped == 0 {
		t.Fatalf("fixture coverage too thin: kept %d dropped %d", kept, dropped)
	}
	if _, err := OpenWith(t.TempDir()+"/missing", "", OpenOptions{KeepStore: func(int64, int64) bool { return true }}); err == nil {
		t.Fatal("missing directory opened")
	}
}
