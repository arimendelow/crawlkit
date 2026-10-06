// Package leveldb reads a copied Chromium LevelDB directory without depending on file order.
// The manifest decides which tables and logs are live; the record with the highest sequence
// number wins for each key. Keys are ordered bytewise regardless of the database's comparator.
//
// This code originates in https://github.com/ourostack/teamscrawl (internal/leveldb, MIT licensed) and is
// provider neutral: it knows nothing about any one application's databases.
package leveldb

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Stats summarizes what Load read. Counts only; never contains keys or values.
//
// Keys counts live retained keys: with a LoadOptions.Keep filter, keys the filter rejects are
// not counted. Skipped counts the table and log records (every version, puts and deletions) the
// filter rejected; it is 0 for an unfiltered load. Tables, Logs, TruncatedLogTails and
// Comparator describe the files and do not depend on the filter.
type Stats struct {
	Tables, Logs, Keys, TruncatedLogTails, Skipped int
	Comparator                                     string
}

// LoadOptions tunes Load.
type LoadOptions struct {
	// Keep reports whether a user key is retained. It must depend on the key alone. Records of
	// rejected keys are dropped as they are read, so their values are never held and the keys
	// are absent from Get and Scan. Nil retains every key.
	Keep func(key []byte) bool
	// Observe, when not nil, is called once for every record read (every version, puts and
	// deletions) before Keep is consulted, so it sees rejected keys too. valueLen is the
	// record's value length (0 for a deletion) and fromTable says whether it came from a table
	// rather than a log. The key and its backing array are only valid during the call. Observe
	// must not retain them.
	Observe func(key []byte, valueLen int, fromTable bool)
}

// EntryOverhead is the heap bytes a loaded DB holds per retained key beyond the key's and the
// value's own bytes: the map slot, the entry struct and the sorted key slice. It is calibrated
// by TestEntryOverheadCalibrated, which fails when the number drifts from a measurement.
const EntryOverhead = 176

// LazyMin is the smallest table value length that a DB does not hold in memory.
func LazyMin() int { return lazyMin }

// DB is a read-only view of a LevelDB directory. Keys and small values are held in memory;
// table values of lazyMin bytes or more are re-read from the table files on demand, so the
// directory must stay in place and unchanged while the DB is used. Table files opened for those
// re-reads stay open until Close, so call Close when done with the DB.
type DB struct {
	entries map[string]entry
	keys    []string // live keys, bytewise ascending
	tables  []tableRef
	cache   blockCache
	stats   Stats
}

// tableRef names a table file. f and size are set the first time a lazy value is read from the
// table and kept until Close (guarded by the block cache's mutex).
type tableRef struct {
	path, name string
	f          *os.File
	size       int64
}

// Load reads dir (a copy; it is never written) and returns its live contents. Large table
// values are re-read from dir on demand, so dir must stay in place and unchanged for as long as
// the DB is used; a value that can no longer be read is an error from Get or Scan.
func Load(dir string) (*DB, error) { return LoadWith(dir, LoadOptions{}) }

// LoadWith is Load with options: with a Keep filter only the chosen keys are held. The same
// directory-lifetime rule as Load applies.
func LoadWith(dir string, opts LoadOptions) (*DB, error) {
	cur, err := readCurrent(dir)
	if err != nil {
		return nil, err
	}
	m, err := readManifest(dir, cur)
	if err != nil {
		return nil, err
	}

	all := newStore(opts.Keep)
	all.observe = opts.Observe
	tableNums := sortedNums(m.tables)
	tables := make([]tableRef, 0, len(tableNums))
	for i, n := range tableNums {
		name, err := tableName(dir, n)
		if err != nil {
			return nil, err
		}
		path := filepath.Join(dir, name)
		tables = append(tables, tableRef{path: path, name: name})
		if err := readTable(path, name, i, all); err != nil {
			return nil, err
		}
	}

	logs, err := liveLogs(dir, m)
	if err != nil {
		return nil, err
	}
	stats := Stats{Tables: len(tableNums), Logs: len(logs), Comparator: m.comparator}
	for _, n := range logs {
		name := fmt.Sprintf("%06d.log", n)
		trunc, err := readLog(filepath.Join(dir, name), name, all)
		if err != nil {
			return nil, err
		}
		if trunc {
			stats.TruncatedLogTails++
		}
	}

	d := &DB{entries: all.m, tables: tables}
	for k, e := range all.m {
		if !e.deleted {
			d.keys = append(d.keys, k)
		}
	}
	sort.Strings(d.keys)
	stats.Keys = len(d.keys)
	stats.Skipped = all.skipped
	d.stats = stats
	return d, nil
}

func sortedNums(set map[uint64]struct{}) []uint64 {
	out := make([]uint64, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// tableName finds the on-disk name for table n (.ldb, or the legacy .sst).
func tableName(dir string, n uint64) (string, error) {
	ldb := fmt.Sprintf("%06d.ldb", n)
	sst := fmt.Sprintf("%06d.sst", n)
	for _, name := range []string{ldb, sst} {
		_, err := os.Stat(filepath.Join(dir, name))
		if err == nil {
			return name, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("leveldb: stat %s: %w", name, err)
		}
	}
	return "", &MissingFileError{Name: ldb}
}

// liveLogs returns the log numbers to replay, oldest first: every log numbered at or above the
// manifest's log number, plus the previous log when the manifest names one and it exists.
func liveLogs(dir string, m *manifest) ([]uint64, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("leveldb: read dir: %w", err)
	}
	var out []uint64
	for _, e := range ents {
		base, ok := strings.CutSuffix(e.Name(), ".log")
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(base, 10, 64)
		if err != nil {
			continue
		}
		if n >= m.logNumber || (m.prevLog != 0 && n == m.prevLog) {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// Get returns the live value for key. ok is false when the key is absent or deleted. err is set
// when a large value cannot be re-read from its table (the directory changed or vanished, or
// the block is damaged): that is a failure, never an absent key.
func (d *DB) Get(key []byte) (value []byte, ok bool, err error) {
	e, ok := d.entries[string(key)]
	if !ok || e.deleted {
		return nil, false, nil
	}
	v, err := d.value(e)
	if err != nil {
		return nil, false, err
	}
	return v, true, nil
}

// Scan calls fn for every live key with the given prefix, in bytewise ascending order.
// It stops at, and returns, the first error from fn or from re-reading a value.
func (d *DB) Scan(prefix []byte, fn func(key, value []byte) error) error {
	p := string(prefix)
	start := sort.SearchStrings(d.keys, p)
	for _, k := range d.keys[start:] {
		if !strings.HasPrefix(k, p) {
			break
		}
		v, err := d.value(d.entries[k])
		if err != nil {
			return err
		}
		if err := fn([]byte(k), v); err != nil {
			return err
		}
	}
	return nil
}

// value returns an entry's value, re-reading a lazy one from its table block.
func (d *DB) value(e entry) ([]byte, error) {
	if !e.lazy {
		return e.value, nil
	}
	b, err := d.cache.get(d.tables, e.loc.table, e.loc.block)
	if err != nil {
		return nil, err
	}
	if e.loc.off+e.loc.n > len(b) {
		return nil, fmt.Errorf("leveldb: table %s: block at %d changed since load", d.tables[e.loc.table].name, e.loc.block.off)
	}
	return b[e.loc.off : e.loc.off+e.loc.n : e.loc.off+e.loc.n], nil
}

// Close releases the table files that lazy value reads opened. It is safe to call more than
// once. After Close, Get and Scan fail with ErrClosed for any lazy value; held values are still
// served. Values already returned stay valid.
func (d *DB) Close() error { return d.cache.close(d.tables) }

// Stats reports counts describing the load.
func (d *DB) Stats() Stats { return d.stats }
