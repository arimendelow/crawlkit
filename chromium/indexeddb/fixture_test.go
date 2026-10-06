package indexeddb

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/golang/snappy"
	"github.com/openclaw/crawlkit/chromium/v8"
	gl "github.com/syndtr/goleveldb/leveldb"
)

// The synthetic fixture is built in a temp dir by buildFixture: no real application data is
// involved. It has the shape of a Chromium app's IndexedDB origin: one LevelDB (log only, as a
// copy of a live profile is) and one blob directory, holding two accounts of two "manager"
// databases that a caller would keep, plus a decoy "auth" database it would not.
const (
	fixtureOrigin    = "https_chat.example.test_0.indexeddb"
	fixtureOriginURL = "https_chat.example.test_0"
	fixtureRecords   = 25
	decoyRecords     = 10
)

var fixtureDatabases = []string{
	"app:chats:client:acct-a",
	"app:chats:client:acct-b",
	"app:convs:client:acct-a",
	"app:convs:client:acct-b",
	"app:auth:client:acct-a", // the decoy
}

// fixtureText is the deterministic text of record i in database db; every third one is long
// enough to pass the lazy-value threshold even after compression.
func fixtureText(db, i int) string {
	r := rand.New(rand.NewSource(int64(db*1000 + i))) //nolint:gosec // deterministic test data
	n := 20
	if i%3 == 0 {
		n = 700
	}
	b := make([]byte, n)
	for j := range b {
		b[j] = "abcdefghijklmnopqrstuvwxyz0123456789"[r.Intn(36)]
	}
	return string(b)
}

func fixtureID(db, i int) string { return fmt.Sprintf("d%d-r%02d", db, i) }

// v8Object serializes {"id": id, "text": text} in V8 wire version 15.
func v8Object(id, text string) []byte {
	out := []byte{0xff, 0x0f, 'o'}
	for _, kv := range [][2]string{{"id", id}, {"text", text}} {
		for _, s := range kv {
			out = append(out, '"')
			out = append(out, varint(uint64(len(s)))...)
			out = append(out, s...)
		}
	}
	return append(out, '{', 2)
}

// buildFixture writes the synthetic origin and returns its LevelDB and blob directories.
func buildFixture(t testing.TB) (ldb, blob string) {
	t.Helper()
	root := t.TempDir()
	ldb = filepath.Join(root, fixtureOrigin+".leveldb")
	blob = filepath.Join(root, fixtureOrigin+".blob")
	db, err := gl.OpenFile(ldb, nil)
	if err != nil {
		t.Fatal(err)
	}
	put := func(k, v []byte) {
		t.Helper()
		if err := db.Put(k, v, nil); err != nil {
			t.Fatal(err)
		}
	}
	for d, name := range fixtureDatabases {
		id := d + 1
		put(dbNameKey(fixtureOriginURL, name), []byte{byte(id)}) //nolint:gosec // small ids
		put(storeNameKey(byte(id), 1), u16("items"))             //nolint:gosec // small ids
		count := fixtureRecords
		if strings.HasPrefix(name, "app:auth:") {
			count = decoyRecords
		}
		for i := 0; i < count; i++ {
			payload := v8Object(fixtureID(id, i), fixtureText(id, i))
			recKey := append([]byte{1}, idbString(fmt.Sprintf("rec%02d", i))...)
			dataKey := append(idPrefix(uint64(id), 1, indexData), recKey...)
			switch i % 5 {
			case 0: // snappy
				inner := append([]byte{0xff, 0x10}, payload...)
				put(dataKey, append([]byte{0x00, 0xff, 0x11, 0x02}, snappy.Encode(nil, inner)...))
			case 1: // blob
				num := uint64(0x100 + i)
				env := v21Envelope(payload, nil)
				dir := filepath.Join(blob, fmt.Sprintf("%x", id), fmt.Sprintf("%02x", num>>8))
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%x", num)), env, 0o600); err != nil {
					t.Fatal(err)
				}
				put(dataKey, append([]byte{0x00}, blobRefValue(uint64(len(env)), 0)...))
				ext := append([]byte{0}, varint(num)...)
				ext = append(ext, varint(0)...)
				ext = append(ext, varint(uint64(len(env)))...)
				put(append(idPrefix(uint64(id), 1, indexBlobEntries), recKey...), ext)
			case 2: // plain
				put(dataKey, append([]byte{0x00, 0xff, 0x10}, payload...))
			default: // v21 with a trailer
				put(dataKey, append([]byte{0x00}, v21Envelope(payload, []byte{0xa0, 1, 2})...))
			}
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return ldb, blob
}

func fixtureDirs(t testing.TB) (string, string) { return buildFixture(t) }

func openFixture(t testing.TB) *Origin {
	t.Helper()
	ldb, blob := fixtureDirs(t)
	o, err := Open(ldb, blob)
	if err != nil {
		t.Fatalf("Open fixture: %v", err)
	}
	t.Cleanup(func() { _ = o.Close() })
	return o
}

// wantedStores lists the decodable (database, store) pairs of one manager. The decoy auth
// database is never touched.
func wantedStores(t *testing.T, o *Origin, manager, store string) []struct {
	DB    Database
	Store Store
} {
	t.Helper()
	dbs, err := o.Databases()
	if err != nil {
		t.Fatalf("Databases: %v", err)
	}
	var out []struct {
		DB    Database
		Store Store
	}
	for _, db := range dbs {
		if !strings.HasPrefix(db.Name, "app:"+manager+":client:") {
			continue
		}
		for _, s := range db.Stores {
			if s.Name == store {
				out = append(out, struct {
					DB    Database
					Store Store
				}{db, s})
			}
		}
	}
	return out
}

// Every kept record decodes, covers every envelope kind and yields exactly the object the
// builder wrote.
func TestFixtureDecodesEveryEnvelope(t *testing.T) {
	o := openFixture(t)
	kinds := map[string]int{}
	total := 0
	for _, manager := range []string{"chats", "convs"} {
		found := wantedStores(t, o, manager, "items")
		if len(found) != 2 {
			t.Fatalf("%s: want 2 accounts, got %d", manager, len(found))
		}
		for _, f := range found {
			n := 0
			err := o.Records(f.DB.ID, f.Store.ID, func(r Record) error {
				if r.Err != nil {
					t.Errorf("bad key in %s: %v", f.DB.Name, r.Err)
					return nil
				}
				kinds[EnvelopeKind(r.Raw)]++
				v, err := o.Decode(f.DB.ID, r.Raw)
				if err != nil {
					t.Errorf("omission in %s: %v", f.DB.Name, err)
					return nil
				}
				c, err := v8.Canonical(v)
				if err != nil {
					return err
				}
				var got map[string]string
				if err := json.Unmarshal(c, &got); err != nil {
					return err
				}
				i := n
				if want := (map[string]string{"id": fixtureID(int(f.DB.ID), i), "text": fixtureText(int(f.DB.ID), i)}); fmt.Sprint(got) != fmt.Sprint(want) {
					t.Errorf("%s record %d: got %v", f.DB.Name, i, got)
				}
				n++
				total++
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if n != fixtureRecords {
				t.Errorf("%s: %d records, want %d", f.DB.Name, n, fixtureRecords)
			}
		}
	}
	for _, k := range []string{"snappy", "blob", "v21", "plain"} {
		if kinds[k] == 0 {
			t.Errorf("no %s envelope in fixture: %v", k, kinds)
		}
	}
	if kinds["unknown"] != 0 {
		t.Errorf("unknown envelopes: %v", kinds)
	}
	if total != 4*fixtureRecords {
		t.Errorf("decoded %d records", total)
	}
}

func TestBuildFixtureIsDeterministic(t *testing.T) {
	a, b := openFixture(t), openFixture(t)
	if a.Stats().Keys != b.Stats().Keys || a.Stats().Keys == 0 {
		t.Fatalf("stats %+v %+v", a.Stats(), b.Stats())
	}
	if !bytes.Equal(v8Object("a", "b"), v8Object("a", "b")) {
		t.Fatal("v8Object differs")
	}
}
