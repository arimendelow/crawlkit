package leveldb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/golang/snappy"
	"github.com/syndtr/goleveldb/leveldb/util"
)

// minimalDir writes a CURRENT and a MANIFEST that names no tables and log number 1.
func minimalDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	edit := append(uv(tagLogNumber), uv(1)...)
	writeJournal(t, filepath.Join(dir, "MANIFEST-000001"), edit)
	if err := os.WriteFile(filepath.Join(dir, "CURRENT"), []byte("MANIFEST-000001\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not restrict root")
	}
}

func requirePermissionSimulation(t *testing.T, err error, what string) {
	t.Helper()
	switch {
	case err == nil:
		t.Skipf("%s permissions are not enforced", what)
	case errors.Is(err, fs.ErrNotExist):
		t.Skipf("%s masks unreadable paths as not found", what)
	case !errors.Is(err, fs.ErrPermission):
		t.Fatalf("%s: got %v, want a permission error", what, err)
	}
}

// failReader fails every read, as a damaged disk would.
type failReader struct{ err error }

func (f failReader) ReadAt([]byte, int64) (int, error) { return 0, f.err }

func trailer(b []byte, typ byte) []byte {
	out := append(append([]byte(nil), b...), typ)
	return binary.LittleEndian.AppendUint32(out, util.NewCRC(out).Value())
}

// buildBlock makes an uncompressed block (single restart) from key/value pairs, without trailer.
func buildBlock(kvs ...[2]string) []byte {
	var b []byte
	for _, kv := range kvs {
		b = append(b, uv(0)...)
		b = append(b, uv(uint64(len(kv[0])))...)
		b = append(b, uv(uint64(len(kv[1])))...)
		b = append(b, kv[0]...)
		b = append(b, kv[1]...)
	}
	b = binary.LittleEndian.AppendUint32(b, 0)
	return binary.LittleEndian.AppendUint32(b, 1)
}

// buildTable lays out data block, index block and footer; the index block's entries are given
// as raw values so a test can plant a bad block handle.
func buildTable(data []byte, indexVal string) []byte {
	db := trailer(data, compressionNone)
	idx := buildBlock([2]string{"k", indexVal})
	ib := trailer(idx, compressionNone)
	footer := append(uv(0), uv(0)...)
	footer = append(footer, uv(uint64(len(db)))...)
	footer = append(footer, uv(uint64(len(idx)))...)
	for len(footer) < footerLen-len(tableMagic) {
		footer = append(footer, 0)
	}
	footer = append(footer, tableMagic...)
	return append(append(db, ib...), footer...)
}

func walk(tab []byte) error {
	return walkTable(bytes.NewReader(tab), int64(len(tab)), 0, newStore(nil))
}

func TestWalkTableSynthetic(t *testing.T) {
	ik := func(user string) string {
		return user + string(binary.LittleEndian.AppendUint64(nil, 1<<8|typeValue))
	}
	good := buildBlock([2]string{ik("a"), "va"})
	// The index entry points the data block at offset 0 with the data block's size (without trailer).
	handle := string(uv(0)) + string(uv(uint64(len(good))))
	s := newStore(nil)
	tab := buildTable(good, handle)
	if err := walkTable(bytes.NewReader(tab), int64(len(tab)), 0, s); err != nil {
		t.Fatal(err)
	}
	if e := s.m["a"]; string(e.value) != "va" || e.seq != 1 {
		t.Fatalf("entry = %+v", e)
	}

	for name, tc := range map[string]struct {
		tab  []byte
		want string
	}{
		"index handle junk":           {buildTable(good, "\xff"), "bad data block handle"},
		"index handle trailing bytes": {buildTable(good, handle+"\x00"), "bad data block handle"},
		"data block outside file":     {buildTable(good, string(uv(1<<20))+string(uv(4))), "outside"},
		"short internal key":          {buildTable(buildBlock([2]string{"abc", "v"}), handle2(buildBlock([2]string{"abc", "v"}))), "internal key too short"},
	} {
		err := walk(tc.tab)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

func handle2(block []byte) string { return string(uv(0)) + string(uv(uint64(len(block)))) }

func TestWalkTableFooterAndIndexErrors(t *testing.T) {
	// A reader that fails on the footer read.
	boom := errors.New("boom")
	if err := walkTable(failReader{boom}, 100, 0, newStore(nil)); !errors.Is(err, boom) {
		t.Fatalf("footer read err = %v", err)
	}
	footer := func(handles []byte) []byte {
		f := append([]byte(nil), handles...)
		for len(f) < footerLen-len(tableMagic) {
			f = append(f, 0)
		}
		return append(f, tableMagic...)
	}
	bad := bytes.Repeat([]byte{0xff}, 11)
	for name, f := range map[string][]byte{
		"metaindex handle junk": footer(bad),
		"index handle junk":     footer(append(append(uv(0), uv(0)...), bad...)),
	} {
		if err := walk(f); err == nil || !strings.Contains(err.Error(), "bad block handle") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// A footer whose index handle points past the file.
	if err := walk(footer(append(append(uv(0), uv(0)...), append(uv(1000), uv(5)...)...))); err == nil || !strings.Contains(err.Error(), "index block") {
		t.Errorf("index outside file: err = %v", err)
	}
}

func TestDecodeHandleRejectsJunk(t *testing.T) {
	for name, b := range map[string][]byte{
		"empty":      nil,
		"no size":    uv(5),
		"bad offset": bytes.Repeat([]byte{0xff}, 11),
		"bad size":   append(uv(5), bytes.Repeat([]byte{0xff}, 11)...),
	} {
		if _, _, err := decodeHandle(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	h, n, err := decodeHandle(append(uv(300), uv(7)...))
	if err != nil || h != (blockHandle{off: 300, size: 7}) || n != 3 {
		t.Fatalf("decodeHandle = %+v, %d, %v", h, n, err)
	}
}

func TestSplitInternalKey(t *testing.T) {
	if _, _, _, ok := splitInternalKey([]byte("short")); ok {
		t.Fatal("short key accepted")
	}
	ik := append([]byte("user"), binary.LittleEndian.AppendUint64(nil, 7<<8|typeDeletion)...)
	u, seq, typ, ok := splitInternalKey(ik)
	if !ok || string(u) != "user" || seq != 7 || typ != typeDeletion {
		t.Fatalf("split = %q %d %d %v", u, seq, typ, ok)
	}
}

func TestReadBlockErrors(t *testing.T) {
	body := []byte("hello")
	raw := trailer(body, compressionNone)
	h := blockHandle{off: 0, size: uint64(len(body))}
	if got, err := readBlock(bytes.NewReader(raw), int64(len(raw)), h); err != nil || string(got) != "hello" {
		t.Fatalf("plain block = %q, %v", got, err)
	}
	if _, err := readBlock(bytes.NewReader(raw), int64(len(raw)), blockHandle{off: 0, size: 99}); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("oversized handle err = %v", err)
	}
	boom := errors.New("boom")
	if _, err := readBlock(failReader{boom}, int64(len(raw)), h); !errors.Is(err, boom) {
		t.Fatalf("read error = %v", err)
	}
	// Snappy block whose declared length is fine but whose body is not a valid stream.
	bad := append(uv(10), 0xff, 0xff)
	rawBad := trailer(bad, compressionSnappy)
	if _, err := readBlock(bytes.NewReader(rawBad), int64(len(rawBad)), blockHandle{size: uint64(len(bad))}); err == nil || !strings.Contains(err.Error(), "snappy") {
		t.Fatalf("bad snappy body err = %v", err)
	}
	// Snappy block whose header is not even a varint.
	junk := bytes.Repeat([]byte{0xff}, 11)
	rawJunk := trailer(junk, compressionSnappy)
	if _, err := readBlock(bytes.NewReader(rawJunk), int64(len(rawJunk)), blockHandle{size: uint64(len(junk))}); err == nil || !strings.Contains(err.Error(), "snappy") {
		t.Fatalf("bad snappy header err = %v", err)
	}
	// A valid snappy block decodes.
	enc := snappy.Encode(nil, []byte("compressed payload"))
	rawOK := trailer(enc, compressionSnappy)
	if got, err := readBlock(bytes.NewReader(rawOK), int64(len(rawOK)), blockHandle{size: uint64(len(enc))}); err != nil || string(got) != "compressed payload" {
		t.Fatalf("snappy block = %q, %v", got, err)
	}
}

func TestBlockEntriesBadHeaderAndCallbackError(t *testing.T) {
	// One byte of entry area that is an unterminated varint, then zero restarts.
	b := append([]byte{0x80}, 0, 0, 0, 0)
	if err := blockEntries(b, func([]byte, int, int) error { return nil }); err == nil || !strings.Contains(err.Error(), "bad entry header") {
		t.Fatalf("err = %v", err)
	}
	stop := errors.New("stop")
	if err := blockEntries(buildBlock([2]string{"k", "v"}), func([]byte, int, int) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("callback error lost: %v", err)
	}
}

func TestOpenTableErrors(t *testing.T) {
	dir := t.TempDir()
	var mf *MissingFileError
	if _, _, err := openTable(filepath.Join(dir, "x.ldb"), "x.ldb"); !errors.As(err, &mf) || mf.Name != "x.ldb" {
		t.Fatalf("missing table err = %v", err)
	}
	if got := mf.Error(); !strings.Contains(got, "x.ldb") {
		t.Fatalf("Error() = %q", got)
	}
	p := filepath.Join(dir, "t.ldb")
	if err := os.WriteFile(p, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("fstat failed")
	old := statFile
	statFile = func(*os.File) (os.FileInfo, error) { return nil, boom }
	defer func() { statFile = old }()
	if _, _, err := openTable(p, "t.ldb"); !errors.Is(err, boom) {
		t.Fatalf("stat failure err = %v", err)
	}
	statFile = old
	skipIfRoot(t)
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	probe, err := os.Open(p) //nolint:gosec // test temp path created in this test
	if err == nil {
		_ = probe.Close()
		t.Skipf("%s permissions are not enforced", p)
	}
	requirePermissionSimulation(t, err, p)
	if _, _, err := openTable(p, "t.ldb"); err == nil || errors.As(err, &mf) {
		t.Fatalf("unreadable table must not read as missing: %v", err)
	}
}

func TestBlockCacheErrors(t *testing.T) {
	d := mustLoad(t, compactedDir(t))
	if _, err := d.cache.get(d.tables, 5, blockHandle{}); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("bad table index err = %v", err)
	}
	if _, err := d.cache.get(d.tables, -1, blockHandle{}); err == nil {
		t.Fatal("negative table index accepted")
	}
	if _, err := d.cache.get(d.tables, 0, blockHandle{off: 1 << 40, size: 8}); err == nil || !strings.Contains(err.Error(), d.tables[0].name) {
		t.Fatalf("bad block err = %v", err)
	}
}

func TestValueDetectsChangedBlock(t *testing.T) {
	dir, _ := buildMixed(t, 0)
	d := mustLoad(t, dir)
	for _, e := range d.entries {
		if !e.lazy {
			continue
		}
		e.loc.off = 1 << 30 // as if the block in the file were shorter than at load time
		if _, err := d.value(e); err == nil || !strings.Contains(err.Error(), "changed since load") {
			t.Fatalf("err = %v", err)
		}
		return
	}
	t.Fatal("no lazy entry")
}

func TestReadCurrentErrors(t *testing.T) {
	var mf *MissingFileError
	if _, err := readCurrent(t.TempDir()); !errors.As(err, &mf) || mf.Name != "CURRENT" {
		t.Fatalf("absent CURRENT err = %v", err)
	}
	// CURRENT is a directory: a read error that is not "missing".
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "CURRENT"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := readCurrent(dir); err == nil || errors.As(err, &mf) {
		t.Fatalf("directory CURRENT err = %v", err)
	}
	for name, content := range map[string]string{"empty": " \n", "slash": "../MANIFEST-000001\n", "backslash": `a\b`} {
		d := t.TempDir()
		if err := os.WriteFile(filepath.Join(d, "CURRENT"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readCurrent(d); err == nil || !strings.Contains(err.Error(), "invalid CURRENT") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("Load accepted a directory CURRENT")
	}
}

func TestReadManifestErrors(t *testing.T) {
	var mf *MissingFileError
	if _, err := readManifest(t.TempDir(), "MANIFEST-000001"); !errors.As(err, &mf) {
		t.Fatalf("absent manifest err = %v", err)
	}
	// A directory opens, then fails to read: an I/O error, not truncation.
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "MANIFEST-000001"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := readManifest(dir, "MANIFEST-000001"); err == nil || errors.Is(err, ErrManifestTruncated) || errors.As(err, &mf) {
		t.Fatalf("unreadable manifest err = %v", err)
	}
	// An edit with an unknown tag fails the load with the manifest named.
	d := t.TempDir()
	writeJournal(t, filepath.Join(d, "MANIFEST-000001"), []byte{0x7f})
	if _, err := readManifest(d, "MANIFEST-000001"); err == nil || !strings.Contains(err.Error(), "MANIFEST-000001") || !strings.Contains(err.Error(), "unknown version-edit tag") {
		t.Fatalf("bad edit err = %v", err)
	}
	// Unreadable file (permission) is not "missing".
	skipIfRoot(t)
	p := filepath.Join(t.TempDir(), "MANIFEST-000001")
	if err := os.WriteFile(p, nil, 0); err != nil {
		t.Fatal(err)
	}
	probe, err := os.Open(p) //nolint:gosec // test temp path created in this test
	if err == nil {
		_ = probe.Close()
		t.Skipf("%s permissions are not enforced", p)
	}
	requirePermissionSimulation(t, err, p)
	if _, err := readManifest(filepath.Dir(p), "MANIFEST-000001"); err == nil || errors.As(err, &mf) {
		t.Fatalf("permission err = %v", err)
	}
}

// A record spanning journal blocks whose second chunk is cut off fails the manifest read as
// truncated (the copy raced a write), not as a generic error.
func TestManifestCutInsideLaterChunk(t *testing.T) {
	dir := t.TempDir()
	big := append(uv(tagComparator), lp(bytes.Repeat([]byte("c"), 40000))...)
	p := filepath.Join(dir, "MANIFEST-000001")
	writeJournal(t, p, big)
	b, err := os.ReadFile(p) //nolint:gosec // test temp dir
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b[:32768+20], 0o600); err != nil { //nolint:gosec // test temp dir
		t.Fatal(err)
	}
	if _, err := readManifest(dir, "MANIFEST-000001"); !errors.Is(err, ErrManifestTruncated) {
		t.Fatalf("err = %v, want ErrManifestTruncated", err)
	}
}

func TestDecoderAfterErrorAndOverrun(t *testing.T) {
	d := &decoder{b: []byte{0x80}}
	if d.uvarint() != 0 || d.err == nil {
		t.Fatal("malformed varint not reported")
	}
	if d.uvarint() != 0 || d.bytes() != nil {
		t.Fatal("decoder kept reading after an error")
	}
	d = &decoder{b: append(uv(10), 'a')}
	if d.bytes() != nil || d.err == nil || !strings.Contains(d.err.Error(), "overruns") {
		t.Fatalf("overrun err = %v", d.err)
	}
	m := &manifest{tables: map[uint64]struct{}{}}
	if err := m.apply(append(uv(tagDeletedFile), 0x80)); err == nil {
		t.Fatal("truncated deleted-file edit accepted")
	}
	if err := m.apply(append(append(uv(tagNewFile), uv(0)...), 0x80)); err == nil || len(m.tables) != 0 {
		t.Fatal("truncated new-file edit accepted or recorded a table")
	}
}

func TestLoadWithErrorPaths(t *testing.T) {
	// Manifest error propagates from Load.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "CURRENT"), []byte("MANIFEST-000009\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var mf *MissingFileError
	if _, err := Load(dir); !errors.As(err, &mf) || mf.Name != "MANIFEST-000009" {
		t.Fatalf("err = %v", err)
	}

	// A corrupt table named by the manifest fails the load.
	cd := compactedDir(t)
	tab := listExt(t, cd, ".ldb")[0]
	if err := os.WriteFile(tab, []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(cd); err == nil || !strings.Contains(err.Error(), filepath.Base(tab)) {
		t.Fatalf("corrupt table err = %v", err)
	}

	// A table the manifest names but the directory lacks is a MissingFileError.
	edit := append(append(append(append(uv(tagNewFile), uv(0)...), uv(7)...), uv(1)...), append(lp([]byte("a")), lp([]byte("z"))...)...)
	md := t.TempDir()
	writeJournal(t, filepath.Join(md, "MANIFEST-000001"), edit)
	if err := os.WriteFile(filepath.Join(md, "CURRENT"), []byte("MANIFEST-000001\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(md); !errors.As(err, &mf) || mf.Name != "000007.ldb" {
		t.Fatalf("missing table err = %v", err)
	}
}

func TestTableNameStatError(t *testing.T) {
	// The "directory" is a regular file, so stat of a name inside it fails with ENOTDIR, which
	// is not "not exist" and must not be reported as a missing table.
	f := filepath.Join(t.TempDir(), "plain")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := tableName(f, 1)
	var mf *MissingFileError
	if goruntime.GOOS == "windows" {
		if !errors.As(err, &mf) || mf.Name != "000001.ldb" {
			t.Fatalf("err = %v", err)
		}
	} else if err == nil || errors.As(err, &mf) {
		t.Fatalf("err = %v", err)
	}
	// The legacy .sst name is found when there is no .ldb.
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "000004.sst"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := tableName(d, 4); err != nil || n != "000004.sst" {
		t.Fatalf("tableName = %q, %v", n, err)
	}
}

func TestLiveLogsEdgeCases(t *testing.T) {
	if _, err := liveLogs(filepath.Join(t.TempDir(), "gone"), &manifest{}); err == nil {
		t.Fatal("unreadable directory accepted")
	}
	dir := t.TempDir()
	for _, n := range []string{"notanumber.log", "000009.log", "readme.txt"} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := liveLogs(dir, &manifest{logNumber: 1})
	if err != nil || len(got) != 1 || got[0] != 9 {
		t.Fatalf("liveLogs = %v, %v", got, err)
	}
}

// Load fails when the directory cannot be listed, even though CURRENT and the manifest read.
func TestLoadDirectoryNotListable(t *testing.T) {
	skipIfRoot(t)
	dir := minimalDir(t)
	if err := os.Chmod(dir, 0o100); err != nil { //nolint:gosec // search but not read is the point of the test
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0o700) }() //nolint:gosec // restoring a test temp dir so cleanup can remove it
	_, probeErr := os.ReadDir(dir)
	requirePermissionSimulation(t, probeErr, dir)
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "read dir") {
		t.Fatalf("err = %v", err)
	}
}

func TestReadLogErrors(t *testing.T) {
	dir := t.TempDir()
	var mf *MissingFileError
	if _, err := readLog(filepath.Join(dir, "000001.log"), "000001.log", newStore(nil)); !errors.As(err, &mf) {
		t.Fatalf("absent log err = %v", err)
	}
	// A log that is really a directory fails to read with an I/O error. It must not be taken
	// for a torn tail, which would silently drop every record after it.
	if err := os.Mkdir(filepath.Join(dir, "000002.log"), 0o700); err != nil {
		t.Fatal(err)
	}
	if trunc, err := readLog(filepath.Join(dir, "000002.log"), "000002.log", newStore(nil)); err == nil || trunc || errors.As(err, &mf) {
		t.Fatalf("directory log = %v, %v", trunc, err)
	}
	// And so does a Load that finds one.
	md := minimalDir(t)
	if err := os.Mkdir(filepath.Join(md, "000003.log"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(md); err == nil {
		t.Fatal("Load accepted an unreadable log")
	}
	skipIfRoot(t)
	p := filepath.Join(dir, "000004.log")
	if err := os.WriteFile(p, nil, 0); err != nil {
		t.Fatal(err)
	}
	probe, err := os.Open(p) //nolint:gosec // test temp path created in this test
	if err == nil {
		_ = probe.Close()
		t.Skipf("%s permissions are not enforced", p)
	}
	requirePermissionSimulation(t, err, p)
	if _, err := readLog(p, "000004.log", newStore(nil)); err == nil || errors.As(err, &mf) {
		t.Fatalf("permission err = %v", err)
	}
}

// A multi-chunk record cut off in its second chunk is a torn tail: the records before it are
// kept and the cut is counted, not an error.
func TestReadLogCutInsideLaterChunk(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "000001.log")
	big := batch(2, "big", strings.Repeat("x", 40000))
	writeJournal(t, p, batch(1, "small", "s"), big)
	b, err := os.ReadFile(p) //nolint:gosec // test temp dir
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b[:32768+20], 0o600); err != nil { //nolint:gosec // test temp dir
		t.Fatal(err)
	}
	s := newStore(nil)
	trunc, err := readLog(p, "000001.log", s)
	if err != nil || !trunc {
		t.Fatalf("readLog = %v, %v", trunc, err)
	}
	if string(s.m["small"].value) != "s" {
		t.Fatal("record before the cut lost")
	}
	if _, ok := s.m["big"]; ok {
		t.Fatal("cut record applied")
	}
}

func TestIsTornRead(t *testing.T) {
	if isTornRead(io.EOF) || isTornRead(errors.New("EIO")) {
		t.Fatal("plain errors classed as torn")
	}
	if !isTornRead(io.ErrUnexpectedEOF) {
		t.Fatal("unexpected EOF not classed as torn")
	}
}

func TestApplyBatchMalformedRecords(t *testing.T) {
	head := func(n uint32) []byte {
		b := binary.LittleEndian.AppendUint64(nil, 1)
		return binary.LittleEndian.AppendUint32(b, n)
	}
	cases := map[string][]byte{
		"deletion key cut":  append(head(1), typeDeletion, 9, 'a'),
		"value cut":         append(append(head(1), typeValue), append(lp([]byte("k")), 9, 'v')...),
		"value length gone": append(append(head(1), typeValue), lp([]byte("k"))...),
	}
	for name, b := range cases {
		if applyBatch(b, newStore(nil)) {
			t.Errorf("%s: accepted", name)
		}
	}
	s := newStore(nil)
	b := append(head(1), typeDeletion)
	b = append(b, lp([]byte("gone"))...)
	if !applyBatch(b, s) || !s.m["gone"].deleted {
		t.Fatalf("deletion not applied: %+v", s.m)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func journalBytes(t *testing.T, records ...[]byte) []byte {
	t.Helper()
	p := filepath.Join(t.TempDir(), "j")
	writeJournal(t, p, records...)
	b, err := os.ReadFile(p) //nolint:gosec // test temp dir
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestReplayLogReadFailures(t *testing.T) {
	eio := errors.New("input/output error")
	// The disk fails before the first record: an error, never an empty log.
	if trunc, err := replayLog(errReader{eio}, "000001.log", newStore(nil)); !errors.Is(err, eio) || trunc {
		t.Fatalf("replayLog = %v, %v", trunc, err)
	}
	// The disk fails inside a record that already began: still an error.
	j := journalBytes(t, batch(1, "k", strings.Repeat("v", 40000)))
	r := io.MultiReader(bytes.NewReader(j[:32768]), errReader{eio})
	if trunc, err := replayLog(r, "000001.log", newStore(nil)); !errors.Is(err, eio) || trunc {
		t.Fatalf("mid-record failure = %v, %v", trunc, err)
	}
}

// A well-formed journal record holding a malformed batch ends the replay as a torn tail and
// keeps what was applied before it.
func TestReplayLogMalformedBatch(t *testing.T) {
	s := newStore(nil)
	j := journalBytes(t, batch(1, "a", "1"), []byte{1, 2, 3}, batch(5, "never", "x"))
	trunc, err := replayLog(bytes.NewReader(j), "000001.log", s)
	if err != nil || !trunc {
		t.Fatalf("replayLog = %v, %v", trunc, err)
	}
	if string(s.m["a"].value) != "1" {
		t.Fatal("record before the defect lost")
	}
	if _, ok := s.m["never"]; ok {
		t.Fatal("records after the defect were applied")
	}
}

func TestApplyBatchCountExceedsRecordsAndUnknownType(t *testing.T) {
	head := func(n uint32) []byte {
		b := binary.LittleEndian.AppendUint64(nil, 1)
		return binary.LittleEndian.AppendUint32(b, n)
	}
	s := newStore(nil)
	if applyBatch(append(head(2), append([]byte{typeValue}, append(lp([]byte("k")), lp([]byte("v"))...)...)...), s) {
		t.Fatal("batch claiming more records than it holds accepted")
	}
	if string(s.m["k"].value) != "v" {
		t.Fatal("record before the defect lost")
	}
	unknown := append(append(head(1), 9), lp([]byte("k"))...)
	if applyBatch(unknown, newStore(nil)) {
		t.Fatal("unknown record type accepted")
	}
}
