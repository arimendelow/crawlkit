package indexeddb

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	"github.com/openclaw/crawlkit/chromium/leveldb"
	gl "github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/util"
)

// writeTables writes kv into a fresh LevelDB in dir and compacts it, so every value lives in a
// table (and values of 512 bytes or more are re-read on demand).
func writeTables(t *testing.T, kv map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	db, err := gl.OpenFile(dir, &opt.Options{Compression: opt.SnappyCompression, BlockSize: 2048})
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range kv {
		if err := db.Put([]byte(k), v, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.CompactRange(util.Range{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

// padBlobEntry appends unreferenced external objects to a blob entry value until it is at
// least n bytes. The referenced indexes are unchanged.
func padBlobEntry(v []byte, n int) []byte {
	out := append([]byte(nil), v...)
	for len(out) < n {
		out = append(out, externalBlob)
		out = append(out, varint(999)...)
		out = append(out, idbString("application/x-padding-padding-padding")...)
		out = append(out, varint(1)...)
	}
	return out
}

func isBlobEntryKey(k []byte) bool {
	_, _, i, _, err := readPrefix(k)
	return err == nil && i == indexBlobEntries
}

// The fixture copied into tables, with blob entries padded past the on-demand threshold, decodes
// exactly like the log-only fixture, filtered or not: filtering and on-demand values together
// change nothing for the kept databases.
func TestTablesFilteredLazyMatchFixture(t *testing.T) {
	ldb, blob := fixtureDirs(t)
	src, err := leveldb.Load(ldb)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	kv := map[string][]byte{}
	large, padded := 0, 0
	_ = src.Scan(nil, func(k, v []byte) error {
		if isBlobEntryKey(k) {
			v = padBlobEntry(v, 600)
			padded++
		}
		if len(v) >= 512 {
			large++
		}
		kv[string(k)] = append([]byte(nil), v...)
		return nil
	})
	if large < 10 || padded == 0 {
		t.Fatalf("too few large values (%d) or blob entries (%d)", large, padded)
	}
	dir := writeTables(t, kv)

	ref := openFixture(t)
	for name, keep := range map[string]func(string) bool{"unfiltered": nil, "allowlisted": keepManagers} {
		o, err := OpenWith(dir, blob, OpenOptions{KeepDatabase: keep})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = o.Close() })
		dbs, err := ref.Databases()
		if err != nil {
			t.Fatal(err)
		}
		records := 0
		for _, db := range dbs {
			for _, s := range db.Stores {
				want := dumpStore(t, ref, db.ID, s.ID)
				got := dumpStore(t, o, db.ID, s.ID)
				if keep != nil && !keep(db.Name) {
					if len(got) != 0 {
						t.Errorf("%s: dropped database %d yielded %d records", name, db.ID, len(got))
					}
					continue
				}
				records += len(want)
				if len(got) != len(want) {
					t.Fatalf("%s: database %d store %d: %d records, want %d", name, db.ID, s.ID, len(got), len(want))
				}
				for i := range want {
					if want[i].Key != got[i].Key || !bytes.Equal(want[i].Raw, got[i].Raw) || !bytes.Equal(want[i].Canonical, got[i].Canonical) || want[i].DecodeErr != got[i].DecodeErr || want[i].Err != got[i].Err {
						t.Fatalf("%s: database %d store %d record %d differs", name, db.ID, s.ID, i)
					}
				}
			}
		}
		if records < 80 {
			t.Fatalf("%s: only %d records compared", name, records)
		}
	}
}

// A blob entry large enough to be re-read on demand that cannot be re-read is an error from
// Records, never a blob_missing omission.
func TestUnreadableBlobEntryIsAnError(t *testing.T) {
	const name = "app:chats:client:t:u:en-us"
	recKey := append([]byte{1}, idbString("rec")...)
	dataKey, _ := makePrefix(1, 1, indexData)
	blobKey, _ := makePrefix(1, 1, indexBlobEntries)
	entry := padBlobEntry(append(append([]byte{externalBlob}, varint(5)...), append(idbString("text/plain"), varint(10)...)...), 700)
	kv := map[string][]byte{
		string(dbNameKey("https_chat.example.test_0", name)): {1},
		string(storeNameKey(1, 1)):                           u16("replychains-2"),
		string(append(dataKey, recKey...)):                   {1, 0xff, 0x11, 0x01, 10, 0}, // version 1, blob 0 of size 10
		string(append(blobKey, recKey...)):                   entry,
	}
	dir := writeTables(t, kv)
	o, err := OpenWith(dir, "", OpenOptions{KeepDatabase: keepManagers})
	if err != nil {
		t.Fatal(err)
	}
	var raws [][]byte
	if err := o.Records(1, 1, func(r Record) error { raws = append(raws, r.Raw); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(raws) != 1 || !bytes.Equal(raws[0], []byte{0xff, 0x11, 0x01, 10, 5}) {
		t.Fatalf("resolved raw = %x, want blob number 5", raws)
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	if err := o.Records(1, 1, func(Record) error { return nil }); !errors.Is(err, leveldb.ErrClosed) {
		t.Fatalf("Records after Close = %v, want leveldb.ErrClosed", err)
	}

	// A fresh Origin, so the blob entry's block is not already cached; then its table goes away.
	o, err = OpenWith(dir, "", OpenOptions{KeepDatabase: keepManagers})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = o.Close() }()
	tabs, _ := filepath.Glob(filepath.Join(dir, "*.ldb"))
	for _, p := range tabs {
		if err := removeFile(p); err != nil {
			t.Fatal(err)
		}
	}
	called := 0
	err = o.Records(1, 1, func(Record) error { called++; return nil })
	var mf *leveldb.MissingFileError
	if !errors.As(err, &mf) || called != 0 {
		t.Fatalf("err = %v after %d records, want a MissingFileError and no record", err, called)
	}
}
