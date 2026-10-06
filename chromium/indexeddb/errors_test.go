package indexeddb

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// errKV wraps a fakeKV and fails Get and/or Scan on demand.
type errKV struct {
	fakeKV
	getErr, scanErr error
}

func (e errKV) Get(k []byte) ([]byte, bool, error) {
	if e.getErr != nil {
		return nil, false, e.getErr
	}
	return e.fakeKV.Get(k)
}

func (e errKV) Scan(p []byte, fn func(k, v []byte) error) error {
	if e.scanErr != nil {
		return e.scanErr
	}
	return e.fakeKV.Scan(p, fn)
}

func TestParseBlobRefMalformed(t *testing.T) {
	good := blobRefValue(7, 3)
	size, number, n, ok := parseBlobRef(good)
	if !ok || size != 7 || number != 3 || n != len(good) {
		t.Fatalf("good ref = %d %d %d %v", size, number, n, ok)
	}
	bad := map[string][]byte{
		"too short":         {0xff, 0x11},
		"no blink tag":      {0x00, 0x11, 0x01, 0, 0},
		"version cut":       {0xff, 0x80, 0x80},
		"wrapper not blob":  {0xff, 0x11, 0x02, 0, 0},
		"size missing":      {0xff, 0x11, 0x01},
		"size unterminated": {0xff, 0x11, 0x01, 0x80},
		"number missing":    {0xff, 0x11, 0x01, 0x05},
		"number cut":        {0xff, 0x11, 0x01, 0x05, 0x80},
	}
	for name, b := range bad {
		if _, _, _, ok := parseBlobRef(b); ok {
			t.Errorf("%s: accepted", name)
		}
	}
	// version varint ends exactly at the end of the value
	if _, _, _, ok := parseBlobRef([]byte{0xff, 0x11, 0x00}); ok {
		t.Error("wrapper byte not 1 accepted")
	}
}

func TestEnvelopeKindTable(t *testing.T) {
	cases := map[string]struct {
		raw  []byte
		want string
	}{
		"empty":            {nil, "unknown"},
		"one byte":         {[]byte{0xff}, "unknown"},
		"no tag":           {[]byte{0x01, 0x0f}, "unknown"},
		"version cut":      {[]byte{0xff, 0x80}, "unknown"},
		"pseudo no kind":   {[]byte{0xff, 0x11}, "unknown"},
		"pseudo blob":      {[]byte{0xff, 0x11, 0x01}, "blob"},
		"pseudo snappy":    {[]byte{0xff, 0x11, 0x02}, "snappy"},
		"pseudo other":     {[]byte{0xff, 0x11, 0x09}, "unknown"},
		"v21 with trailer": {[]byte{0xff, 0x15, 0xfe}, "v21"},
		"v21 without":      {[]byte{0xff, 0x15, 0x00}, "unknown"},
		"v21 bare":         {[]byte{0xff, 0x15}, "unknown"},
		"plain":            {[]byte{0xff, 0x0f, 0x30}, "plain"},
		"version zero":     {[]byte{0xff, 0x00, 0x30}, "unknown"},
	}
	for name, c := range cases {
		if got := EnvelopeKind(c.raw); got != c.want {
			t.Errorf("%s: EnvelopeKind = %q, want %q", name, got, c.want)
		}
	}
}

func TestPayloadRejectsMalformedEnvelopes(t *testing.T) {
	o := newTestOrigin(fakeKV{}, t.TempDir())
	// A snappy wrapper nested inside itself past the depth limit.
	nested := []byte{0xff, 0x0f, 0x30}
	// snappy block: varint length then literal tag.
	wrap := func(inner []byte) []byte {
		enc := append(varint(uint64(len(inner))), byte((len(inner)-1)<<2)) //nolint:gosec // the test payload is a few bytes
		enc = append(enc, inner...)
		return append([]byte{0xff, 0x11, 0x02}, enc...)
	}
	for i := 0; i < maxEnvelopeDepth+2; i++ {
		nested = wrap(nested)
	}
	cases := map[string][]byte{
		"too deep":             nested,
		"no tag":               {0x00, 0x00},
		"version cut":          {0xff, 0x80},
		"pseudo no kind":       {0xff, 0x11},
		"malformed blob ref":   {0xff, 0x11, 0x01},
		"unknown wrapper kind": {0xff, 0x11, 0x09},
		"bad snappy":           {0xff, 0x11, 0x02, 0xff, 0xff, 0xff},
		"v21 short trailer":    {0xff, 0x15, 0xfe, 0},
		"v21 wrong tag":        append([]byte{0xff, 0x15, 0x00}, make([]byte, 12)...),
		"version zero":         {0xff, 0x00},
	}
	for name, raw := range cases {
		_, err := o.Payload(1, raw)
		var oe *OmissionError
		if !errors.As(err, &oe) || oe.Code != CodeUnknownEnvelope {
			t.Errorf("%s: err = %v, want unknown_envelope", name, err)
		}
	}
}

func TestReadBlobFailures(t *testing.T) {
	o := newTestOrigin(fakeKV{}, "")
	if _, err := o.readBlob(1, 5); omissionCode(t, err) != CodeBlobMissing || !strings.Contains(err.Error(), "no blob directory") {
		t.Fatalf("no blob dir: %v", err)
	}
	if _, err := o.readBlob(1, unresolvedBlob-2); omissionCode(t, err) != CodeBlobMissing || !strings.Contains(err.Error(), "index 2") {
		t.Fatalf("unresolved: %v", err)
	}
	o = newTestOrigin(fakeKV{}, t.TempDir())
	if _, err := o.readBlob(1, 5); omissionCode(t, err) != CodeBlobMissing || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("absent file: %v", err)
	}
}

func TestResolveBlobRefPaths(t *testing.T) {
	key := append([]byte{1}, idbString("k")...)
	ref := blobRefValue(9, 0)
	// A malformed ref is returned untouched.
	o := newTestOrigin(fakeKV{}, "")
	bad := []byte{0xff, 0x11, 0x01}
	if got, err := o.resolveBlobRef(1, 1, key, bad); err != nil || !bytes.Equal(got, bad) {
		t.Fatalf("malformed ref = % x, %v", got, err)
	}
	// A blob entry that cannot be read is an error, not blob_missing.
	boom := errors.New("disk")
	o = &Origin{kv: errKV{fakeKV: fakeKV{}, getErr: boom}}
	if _, err := o.resolveBlobRef(1, 1, key, ref); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	// A present but unparsable entry leaves the unresolved number, with the huge index folded.
	pfx := idPrefix(1, 1, indexBlobEntries)
	o = newTestOrigin(fakeKV{string(append(pfx, key...)): {9}}, "")
	got, err := o.resolveBlobRef(1, 1, key, blobRefValue(9, 1<<40))
	if err != nil {
		t.Fatal(err)
	}
	_, number, _, _ := parseBlobRef(got)
	if number != unresolvedBlob {
		t.Fatalf("number = %d, want the unresolved marker", number)
	}
	// An index beyond the entry's object list also stays unresolved (index 2 of a 1-item list).
	ext := append(append([]byte{0}, varint(0x1ff)...), append(varint(0), varint(7)...)...)
	o = newTestOrigin(fakeKV{string(append(pfx, key...)): ext}, "")
	got, _ = o.resolveBlobRef(1, 1, key, blobRefValue(9, 2))
	if _, number, _, _ := parseBlobRef(got); number != unresolvedBlob-2 {
		t.Fatalf("number = %d", number)
	}
}

func TestKeyHelpers(t *testing.T) {
	if _, _, err := readUTF16(nil); err == nil {
		t.Error("empty readUTF16 accepted")
	}
	if _, _, err := readUTF16([]byte{5, 0, 'a'}); err == nil {
		t.Error("overlong readUTF16 accepted")
	}
	for _, b := range [][]byte{nil, make([]byte, 9)} {
		if _, err := decodeTruncatedInt(b); err == nil {
			t.Errorf("decodeTruncatedInt(%d bytes) accepted", len(b))
		}
	}
	if v, err := decodeTruncatedInt([]byte{0x34, 0x12}); err != nil || v != 0x1234 {
		t.Fatalf("decodeTruncatedInt = %d, %v", v, err)
	}
	if _, _, _, _, err := readPrefix(nil); err == nil {
		t.Error("empty readPrefix accepted")
	}
}

func TestDecodeKeyMalformed(t *testing.T) {
	bad := map[string][]byte{
		"empty":             nil,
		"string cut":        {1, 5, 0},
		"number cut":        {3, 1, 2},
		"date cut":          {2, 1},
		"array count cut":   {4},
		"array count huge":  {4, 0x7f},
		"array item bad":    {4, 1, 9},
		"array item cut":    {4, 2, 0},
		"binary length cut": {6},
		"binary overruns":   {6, 5, 1},
		"unsupported type":  {9},
	}
	for name, b := range bad {
		if _, _, err := decodeKey(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A date key is milliseconds since the epoch, as a double.
	d := append([]byte{2}, make([]byte, 8)...)
	d[7], d[8] = 0x40, 0x8f // 1000.0? bytes little-endian of float64(1000) are 0,0,0,0,0,0x40,0x8f,0x40
	v, n, err := decodeKey([]byte{2, 0, 0, 0, 0, 0, 0x40, 0x8f, 0x40})
	if err != nil || n != 9 || !v.(time.Time).Equal(time.UnixMilli(1000)) {
		t.Fatalf("date = %v, %d, %v", v, n, err)
	}
}

func TestDatabasesErrors(t *testing.T) {
	boom := errors.New("scan failed")
	if _, err := (&Origin{kv: errKV{scanErr: boom}}).Databases(); !errors.Is(err, boom) {
		t.Fatalf("scan err = %v", err)
	}
	cases := map[string]fakeKV{
		"origin cut": {string([]byte{0, 0, 0, 0, 0xc9, 5, 0}): {1}},
		"name cut":   {string(append([]byte{0, 0, 0, 0, 0xc9}, idbString("o")...)): {1}},
		"id empty":   {string(dbNameKey("o", "n")): {}},
	}
	for name, f := range cases {
		if _, err := newTestOrigin(f, "").Databases(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A database id out of range fails loudly instead of scanning a bogus prefix.
	huge := fakeKV{string(dbNameKey("o", "n")): {0, 0, 0, 0, 0, 0, 0x10}}
	if _, err := newTestOrigin(huge, "").Databases(); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("huge id err = %v", err)
	}
	neg := fakeKV{string(dbNameKey("o", "n")): {0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}}
	if _, err := newTestOrigin(neg, "").Databases(); err == nil {
		t.Fatal("negative id accepted")
	}
	// A store-metadata key failing to scan.
	f := fakeKV{string(dbNameKey("o", "n")): {1}}
	if _, err := (&Origin{kv: scanFailsOnStores{f, boom}}).Databases(); !errors.Is(err, boom) {
		t.Fatalf("stores scan err = %v", err)
	}
}

// scanFailsOnStores serves the database-name scan and fails the per-database one.
type scanFailsOnStores struct {
	fakeKV
	err error
}

func (s scanFailsOnStores) Scan(p []byte, fn func(k, v []byte) error) error {
	if len(p) == 5 && p[4] == metaObjectStore {
		return s.err
	}
	return s.fakeKV.Scan(p, fn)
}

func TestStoresKeyShapes(t *testing.T) {
	pfx := idPrefix(1, 0, 0, metaObjectStore)
	f := fakeKV{
		string(dbNameKey("o", "n")): {1},
		// good store name entry: id 7, sub-key 0
		string(append(append([]byte(nil), pfx...), 7, storeMetaName)): u16("keep"),
		// other metadata sub-key for the same store is ignored
		string(append(append([]byte(nil), pfx...), 7, 1)): u16("ignored"),
		// longer sub-key starting with 0 is ignored too
		string(append(append([]byte(nil), pfx...), 8, storeMetaName, 9)): u16("ignored"),
	}
	dbs, err := newTestOrigin(f, "").Databases()
	if err != nil || len(dbs) != 1 || len(dbs[0].Stores) != 1 || dbs[0].Stores[0] != (Store{ID: 7, Name: "keep"}) {
		t.Fatalf("dbs = %+v, %v", dbs, err)
	}
	for name, k := range map[string][]byte{
		"id cut":     append(append([]byte(nil), pfx...), 0x80),
		"no sub-key": append(append([]byte(nil), pfx...), 7),
	} {
		g := fakeKV{string(dbNameKey("o", "n")): {1}, string(k): nil}
		if _, err := newTestOrigin(g, "").Databases(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRecordsErrors(t *testing.T) {
	o := newTestOrigin(fakeKV{}, "")
	if err := o.Records(-1, 1, nil); err == nil {
		t.Fatal("negative db id accepted")
	}
	if err := o.Records(1, -1, nil); err == nil {
		t.Fatal("negative store id accepted")
	}
	pfx := idPrefix(1, 1, indexData)
	key := append(append([]byte(nil), pfx...), append([]byte{1}, idbString("k")...)...)
	// A value whose version varint is cut is a hard error, not an omission.
	o = newTestOrigin(fakeKV{string(key): {0x80}}, "")
	if err := o.Records(1, 1, func(Record) error { return nil }); err == nil || !strings.Contains(err.Error(), "record version") {
		t.Fatalf("err = %v", err)
	}
	// An empty value yields a record with no Raw, for Decode to report as empty_value.
	o = newTestOrigin(fakeKV{string(key): {}}, "")
	var got []Record
	if err := o.Records(1, 1, func(r Record) error { got = append(got, r); return nil }); err != nil || len(got) != 1 || len(got[0].Raw) != 0 || got[0].Key != "k" {
		t.Fatalf("records = %+v, %v", got, err)
	}
	// A failing blob-entry lookup stops the walk with that error.
	boom := errors.New("disk")
	blobVal := append([]byte{0x01}, blobRefValue(4, 0)...)
	o = &Origin{kv: errKV{fakeKV: fakeKV{string(key): blobVal}, getErr: boom}}
	if err := o.Records(1, 1, func(Record) error { return nil }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	// An fn error propagates.
	stop := errors.New("stop")
	o = newTestOrigin(fakeKV{string(key): {}}, "")
	if err := o.Records(1, 1, func(Record) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("err = %v", err)
	}
}

func TestExternalObjectsMalformed(t *testing.T) {
	bad := map[string][]byte{
		"unknown type":   {7},
		"number cut":     {0, 0x80},
		"mime cut":       {0, 1, 5},
		"size cut":       {0, 1, 0, 0x80},
		"file name cut":  {1, 1, 0, 0, 5},
		"file mtime cut": {1, 1, 0, 0, 0, 0x80},
	}
	for name, b := range bad {
		if _, err := externalObjects(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	nums, err := externalObjects([]byte{1, 4, 0, 0, 0, 0})
	if err != nil || len(nums) != 1 || nums[0] != 4 {
		t.Fatalf("file entry = %v, %v", nums, err)
	}
}

func TestOpenWithErrors(t *testing.T) {
	empty := t.TempDir()
	if _, err := Open(empty, ""); err == nil {
		t.Fatal("Open of an empty directory accepted")
	}
	all := OpenOptions{KeepDatabase: func(string) bool { return true }}
	if _, err := OpenWith(empty, "", all); err == nil {
		t.Fatal("filtered Open of an empty directory accepted")
	}

	// A global database-name key that cannot be parsed fails the metadata pass.
	badName := writeTables(t, map[string][]byte{string([]byte{0, 0, 0, 0, 0xc9, 5, 0}): {1}})
	if _, err := OpenWith(badName, "", all); err == nil || !strings.Contains(err.Error(), "database name key") {
		t.Fatalf("bad name key err = %v", err)
	}

	// A key too short to carry a prefix is skipped in both passes, not an error.
	good := writeTables(t, map[string][]byte{
		string(dbNameKey("o", "n")): {1},
		"x":                         {9},
	})
	o, err := OpenWith(good, "", all)
	if err != nil {
		t.Fatal(err)
	}
	if got := o.Stats().Skipped; got < 1 {
		t.Fatalf("Skipped = %d, want the short key counted", got)
	}
	if dbs, err := o.Databases(); err != nil || len(dbs) != 1 || dbs[0].Name != "n" {
		t.Fatalf("Databases = %+v, %v", dbs, err)
	}

	// The single-pass KeepStore open skips a key too short to carry a prefix as well.
	o, err = OpenWith(good, "", OpenOptions{KeepStore: func(int64, int64) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	if got := o.Stats().Skipped; got < 1 {
		t.Fatalf("KeepStore Skipped = %d, want the short key counted", got)
	}

	// The data pass fails when a table disappears between the two passes.
	dir := writeTables(t, map[string][]byte{string(dbNameKey("o", "n")): {1}})
	removing := OpenOptions{KeepDatabase: func(string) bool {
		for _, p := range tablesIn(t, dir) {
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
		}
		return true
	}}
	if _, err := OpenWith(dir, "", removing); err == nil {
		t.Fatal("data pass over a vanished table accepted")
	}
}

func tablesIn(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "*.ldb"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPayloadMultiByteVersionAndNestedHeader(t *testing.T) {
	o := newTestOrigin(fakeKV{}, "")
	// Version 149 (a two-byte varint) is >= 21 but the payload would not sit at offset 15.
	raw := append([]byte{0xff, 0x95, 0x01, 0xfe}, make([]byte, 12)...)
	if _, err := o.Payload(1, raw); omissionCode(t, err) != CodeUnknownEnvelope || !strings.Contains(err.Error(), "trailer does not match") {
		t.Fatalf("err = %v", err)
	}
	// A plain version header followed by the V8 header yields the V8 part.
	got, err := o.Payload(1, []byte{0xff, 0x0f, 0xff, 0x0f, '0'})
	if err != nil || !bytes.Equal(got, []byte{0xff, 0x0f, '0'}) {
		t.Fatalf("payload = % x, %v", got, err)
	}
}

func TestPayloadPlainValueIsReturnedWhole(t *testing.T) {
	o := newTestOrigin(fakeKV{}, "")
	raw := []byte{0xff, 0x0f, '0'}
	got, err := o.Payload(1, raw)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("payload = % x, %v", got, err)
	}
}
