package leveldb

import (
	"container/list"
	"errors"
	"fmt"
	"sync"
)

// ErrClosed is returned when a lazy value is read from a DB after Close.
var ErrClosed = errors.New("leveldb: DB is closed")

// cacheBytes bounds the uncompressed block bytes a DB keeps for re-reading lazy values. Scan
// visits keys in bytewise order, which is not the tables' stored order, so neighbouring values
// often come from unrelated blocks; the old 16-block cache re-read and re-decompressed the same
// blocks over and over. 4 MiB holds about a thousand typical 4 KiB blocks. It was chosen by
// measuring a sync of a real 140 MB cache: 2, 4, 8 and 32 MiB all ran within a second of each
// other, but peak resident memory grew by about 30 MiB at 8 MiB and above (the cache is live heap
// that every collection must keep), while 4 MiB stayed level with the old build. Memory must not
// rise, so the bound stays small. It applies to the one DB open at a time, per DB. A variable so a
// test can shrink it.
var cacheBytes = 4 << 20

type blockKey struct {
	table int
	off   uint64
}

type cached struct {
	key blockKey
	b   []byte
}

// blockCache is a least-recently-used cache of uncompressed blocks, bounded by bytes. Values
// handed out alias a cached block; eviction only drops the cache's reference, so they stay
// valid. It also owns the open table files, which stay open until close.
type blockCache struct {
	mu     sync.Mutex
	blocks map[blockKey]*list.Element // element value is *cached
	order  list.List                  // most recently used first
	bytes  int                        // sum of len(b) over cached blocks
	reads  int                        // blocks read from disk, for tests
	closed bool
}

func (c *blockCache) get(tables []tableRef, table int, h blockHandle) ([]byte, error) {
	k := blockKey{table: table, off: h.off}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClosed
	}
	if el, ok := c.blocks[k]; ok {
		c.order.MoveToFront(el)
		return el.Value.(*cached).b, nil
	}
	if table < 0 || table >= len(tables) {
		return nil, fmt.Errorf("leveldb: table index %d out of range", table)
	}
	t := &tables[table]
	if t.f == nil {
		f, size, err := openTable(t.path, t.name)
		if err != nil {
			return nil, err
		}
		t.f, t.size = f, size
	}
	b, err := readBlock(t.f, t.size, h)
	if err != nil {
		return nil, fmt.Errorf("leveldb: table %s: %w", t.name, err)
	}
	c.reads++
	if len(b) > cacheBytes {
		return b, nil // larger than the whole cache: hand it out uncached
	}
	if c.blocks == nil {
		c.blocks = map[blockKey]*list.Element{}
	}
	c.blocks[k] = c.order.PushFront(&cached{key: k, b: b})
	c.bytes += len(b)
	for c.bytes > cacheBytes {
		last := c.order.Back()
		old := c.order.Remove(last).(*cached)
		delete(c.blocks, old.key)
		c.bytes -= len(old.b)
	}
	return b, nil
}

// close drops the cache and closes every table file that was opened. It is idempotent and
// returns the first close error.
func (c *blockCache) close(tables []tableRef) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.blocks, c.bytes = nil, 0
	c.order.Init()
	var first error
	for i := range tables {
		if f := tables[i].f; f != nil {
			tables[i].f = nil
			if err := f.Close(); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}
