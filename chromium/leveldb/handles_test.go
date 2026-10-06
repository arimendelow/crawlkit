package leveldb

import (
	"errors"
	"os"
	"runtime"
	"sort"
	"testing"

	"github.com/syndtr/goleveldb/leveldb/opt"
)

// scanAll reads every value, so every table that holds a lazy value is opened.
func scanAll(t *testing.T, d *DB) {
	t.Helper()
	if err := d.Scan(nil, func(_, _ []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func openHandles(d *DB) (n int) {
	for _, tb := range d.tables {
		if tb.f != nil {
			n++
		}
	}
	return n
}

// Table files are opened once, on the first lazy read of that table, and then kept: reading
// every value twice, with a cache too small to keep anything, opens each table at most once.
func TestTableFilesStayOpen(t *testing.T) {
	old := cacheBytes
	cacheBytes = 1 // every block is larger than the bound, so nothing is cached
	t.Cleanup(func() { cacheBytes = old })
	dir, _ := buildMixed(t, opt.SnappyCompression)
	d := mustLoad(t, dir)
	scanAll(t, d)
	first := d.cache.reads
	var handles []*os.File
	for _, tb := range d.tables {
		if tb.f != nil {
			handles = append(handles, tb.f)
		}
	}
	if len(handles) == 0 || len(handles) > len(d.tables) {
		t.Fatalf("%d open handles for %d tables", len(handles), len(d.tables))
	}
	scanAll(t, d)
	if d.cache.reads <= first {
		t.Fatal("second pass did not re-read blocks; the test does not exercise reuse")
	}
	var again []*os.File
	for _, tb := range d.tables {
		if tb.f != nil {
			again = append(again, tb.f)
		}
	}
	if len(again) != len(handles) {
		t.Fatalf("handles %d then %d", len(handles), len(again))
	}
	for i := range handles {
		if handles[i] != again[i] {
			t.Fatal("a table file was reopened")
		}
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if openHandles(d) != 0 {
		t.Fatal("Close left table files open")
	}
	for _, f := range handles {
		if _, err := f.Stat(); err == nil || (!errors.Is(err, os.ErrClosed) && runtime.GOOS != "windows") {
			t.Fatalf("handle not closed: %v", err)
		}
	}
}

// Close is idempotent; a lazy read after Close is ErrClosed; held values still work; values
// handed out before Close stay valid.
func TestCloseSemantics(t *testing.T) {
	dir, want := buildMixed(t, opt.SnappyCompression)
	d := mustLoad(t, dir)
	var lazyKey, heldKey string
	for k, e := range d.entries {
		switch {
		case e.deleted:
		case e.lazy && lazyKey == "":
			lazyKey = k
		case !e.lazy && heldKey == "":
			heldKey = k
		}
	}
	before, ok, err := d.Get([]byte(lazyKey))
	if err != nil || !ok {
		t.Fatalf("Get = %v, %v", ok, err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if string(before) != string(want[lazyKey]) {
		t.Fatal("a value handed out before Close changed")
	}
	if _, _, err := d.Get([]byte(lazyKey)); !errors.Is(err, ErrClosed) {
		t.Fatalf("lazy Get after Close = %v, want ErrClosed", err)
	}
	if v, ok, err := d.Get([]byte(heldKey)); err != nil || !ok || string(v) != string(want[heldKey]) {
		t.Fatalf("held Get after Close = %v, %v", ok, err)
	}
}

// Close with no lazy read has nothing to release and reports no error.
func TestCloseWithoutReads(t *testing.T) {
	d := mustLoad(t, compactedDir(t))
	if openHandles(d) != 0 {
		t.Fatal("Load opened table handles that only lazy reads need")
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
}

// A close error from a table file is reported (the first one), and the rest still close.
func TestCloseReportsFirstError(t *testing.T) {
	dir, _ := buildMixed(t, opt.SnappyCompression)
	d := mustLoad(t, dir)
	scanAll(t, d)
	var opened []int
	for i, tb := range d.tables {
		if tb.f != nil {
			opened = append(opened, i)
		}
	}
	if len(opened) < 2 {
		t.Fatalf("need two open tables, have %d", len(opened))
	}
	_ = d.tables[opened[0]].f.Close() // a second close of this one fails
	_ = d.tables[opened[1]].f.Close()
	if err := d.Close(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Close = %v, want the file's close error", err)
	}
	if openHandles(d) != 0 {
		t.Fatal("a handle was left after a close error")
	}
}

// The cache never holds more than cacheBytes, evicts least recently used first, and blocks
// handed out before an eviction keep their contents.
func TestBlockCacheBoundedByBytes(t *testing.T) {
	dir, _ := buildMixed(t, opt.SnappyCompression)
	probe := mustLoad(t, dir)
	type blk struct {
		table int
		h     blockHandle
	}
	seen := map[blk]bool{}
	var blocks []blk
	for _, e := range probe.entries {
		if e.lazy && !seen[blk{e.loc.table, e.loc.block}] {
			b := blk{e.loc.table, e.loc.block}
			seen[b] = true
			blocks = append(blocks, b)
		}
	}
	sort.Slice(blocks, func(i, j int) bool {
		if blocks[i].table != blocks[j].table {
			return blocks[i].table < blocks[j].table
		}
		return blocks[i].h.off < blocks[j].h.off
	})
	if len(blocks) < 4 {
		t.Fatalf("only %d lazy blocks", len(blocks))
	}
	sizes := make([]int, len(blocks))
	for i, b := range blocks {
		v, err := probe.cache.get(probe.tables, b.table, b.h)
		if err != nil {
			t.Fatal(err)
		}
		sizes[i] = len(v)
	}
	_ = probe.Close()

	// Room for the two newest of any three blocks at most.
	old := cacheBytes
	t.Cleanup(func() { cacheBytes = old })
	cacheBytes = sizes[0] + sizes[1] + 1
	d := mustLoad(t, dir)
	defer func() { _ = d.Close() }()
	first, err := d.cache.get(d.tables, blocks[0].table, blocks[0].h)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := append([]byte(nil), first...)
	for _, b := range blocks[1:] {
		if _, err := d.cache.get(d.tables, b.table, b.h); err != nil {
			t.Fatal(err)
		}
		if d.cache.bytes > cacheBytes {
			t.Fatalf("cache holds %d bytes, bound %d", d.cache.bytes, cacheBytes)
		}
		sum := 0
		for el := d.cache.order.Front(); el != nil; el = el.Next() {
			sum += len(el.Value.(*cached).b)
		}
		if sum != d.cache.bytes || len(d.cache.blocks) != d.cache.order.Len() {
			t.Fatalf("accounting drifted: sum %d, bytes %d, map %d, list %d", sum, d.cache.bytes, len(d.cache.blocks), d.cache.order.Len())
		}
	}
	if _, ok := d.cache.blocks[blockKey{blocks[0].table, blocks[0].h.off}]; ok {
		t.Fatal("the least recently used block was not evicted")
	}
	if string(first) != string(snapshot) {
		t.Fatal("eviction changed a slice already handed out")
	}
	// A hit refreshes recency: touch the oldest cached block, add another, and it survives.
	last := blocks[len(blocks)-1]
	prev := blocks[len(blocks)-2]
	if _, err := d.cache.get(d.tables, prev.table, prev.h); err != nil {
		t.Fatal(err)
	}
	reads := d.cache.reads
	if _, err := d.cache.get(d.tables, last.table, last.h); err != nil || d.cache.reads != reads {
		t.Fatalf("a cached block was re-read: %v", err)
	}
	if _, err := d.cache.get(d.tables, blocks[0].table, blocks[0].h); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.cache.blocks[blockKey{last.table, last.h.off}]; !ok {
		t.Fatal("a recently used block was evicted before an older one")
	}
}

// A block larger than the whole cache is returned but not kept.
func TestBlockLargerThanCacheIsNotCached(t *testing.T) {
	old := cacheBytes
	cacheBytes = 8
	t.Cleanup(func() { cacheBytes = old })
	dir, want := buildMixed(t, opt.SnappyCompression)
	d := mustLoad(t, dir)
	defer func() { _ = d.Close() }()
	checkAgainst(t, d, want)
	if d.cache.bytes != 0 || len(d.cache.blocks) != 0 || d.cache.order.Len() != 0 {
		t.Fatalf("cache kept %d bytes in %d blocks", d.cache.bytes, len(d.cache.blocks))
	}
}

// The production bound is a few megabytes: measured on a real cache, more raised peak memory.
func TestCacheBoundIsFewMegabytes(t *testing.T) {
	if cacheBytes < 1<<20 || cacheBytes > 16<<20 {
		t.Fatalf("cacheBytes = %d", cacheBytes)
	}
}
