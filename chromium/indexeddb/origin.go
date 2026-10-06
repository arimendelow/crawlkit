// Package indexeddb reads Chromium's IndexedDB backing store (LevelDB key
// coding, Blink value envelopes and external blob files) from a copied
// profile directory and hands V8 payloads to the chromium/v8 package.
//
// Values that cannot be decoded are omissions, not failures. They are
// *OmissionError values whose Code is one of this complete list:
//
//	empty_value       the record has no value
//	unknown_envelope  the Blink envelope is not recognized (detail ends with the first 16 bytes in hex)
//	blob_missing      the external blob file or its blob entry is absent
//	bad_key           the record key could not be decoded (Record.Err; detail has the key in hex)
//	v8_unknown_tag, v8_host_object, v8_shared   passed through from chromium/v8
//	snappy_too_large  a Snappy envelope declares a decoded length more than 32 times its compressed size
//	v8_version        the V8 wire format version is unsupported
//	v8_malformed      any other V8 decoding error
//
// This code originates in https://github.com/ourostack/teamscrawl (internal/indexeddb, MIT licensed) and is
// provider neutral: it knows nothing about any one application's databases.
package indexeddb

import (
	"github.com/openclaw/crawlkit/chromium/leveldb"
)

// kv is the read-only view of the LevelDB the package needs.
type kv interface {
	Get(key []byte) ([]byte, bool, error)
	Scan(prefix []byte, fn func(key, value []byte) error) error
	Close() error
}

// Origin is one origin's IndexedDB: a LevelDB directory plus its blob directory.
type Origin struct {
	kv      kv
	blobDir string
	stats   leveldb.Stats
}

// Open loads a copied <origin>.indexeddb.leveldb directory. blobDir is the
// sibling .blob directory; it may be empty or absent, in which case blob-backed
// values decode as blob_missing. Large values are re-read from the LevelDB
// directory on demand, so it must stay in place and unchanged while the Origin
// is used; a value that can no longer be read makes Records fail. Close the Origin when done.
func Open(leveldbDir, blobDir string) (*Origin, error) {
	return OpenWith(leveldbDir, blobDir, OpenOptions{})
}

// OpenOptions tunes Open.
type OpenOptions struct {
	// KeepDatabase reports whether a database's records are loaded, by database name. Nil
	// loads every database. Metadata is always loaded, so Databases lists every database
	// with its object stores either way; Records of a database that is not kept yields
	// nothing, and its values are never held in memory.
	KeepDatabase func(name string) bool
	// KeepStore, when set, replaces KeepDatabase: it reports whether the records of one object
	// store (database id, object store id) are loaded. The caller already knows the ids (from
	// Census), so OpenWith skips the metadata-only pass and reads the LevelDB once. Databases
	// still works (metadata is always loaded).
	KeepStore func(dbID, storeID int64) bool
}

// OpenWith is Open with options. With KeepDatabase set it reads the LevelDB twice: first
// the global metadata alone, to map the kept database names to ids, then the metadata plus
// the object store records and blob entries of the kept databases. Index entries (secondary
// indexes, exists entries) are not kept: this package never reads them. Stats describes the
// second read: Keys counts the retained keys and Skipped the records dropped.
//
// The Origin re-reads large values from the LevelDB directory, which must stay in place while
// the Origin is used. Known cost: with snapshot validation, a sync reads every table three
// times (validation, this metadata pass, the data pass); folding validation into the metadata
// pass would save one.
func OpenWith(leveldbDir, blobDir string, opts OpenOptions) (*Origin, error) {
	if opts.KeepStore != nil {
		db, err := leveldb.LoadWith(leveldbDir, leveldb.LoadOptions{Keep: func(k []byte) bool {
			id, store, index, _, err := readPrefix(k)
			if err != nil {
				return false
			}
			switch {
			case id == 0, store == 0 && index == 0:
				return true
			case index != indexData && index != indexBlobEntries:
				return false
			default:
				return opts.KeepStore(int64(id), int64(store)) //nolint:gosec // ids are bounded well below int64
			}
		}})
		if err != nil {
			return nil, err
		}
		return &Origin{kv: db, blobDir: blobDir, stats: db.Stats()}, nil
	}
	if opts.KeepDatabase == nil {
		db, err := leveldb.Load(leveldbDir)
		if err != nil {
			return nil, err
		}
		return &Origin{kv: db, blobDir: blobDir, stats: db.Stats()}, nil
	}
	meta, err := leveldb.LoadWith(leveldbDir, leveldb.LoadOptions{Keep: func(k []byte) bool {
		id, store, index, _, err := readPrefix(k)
		return err == nil && id == 0 && store == 0 && index == 0
	}})
	if err != nil {
		return nil, err
	}
	names, err := databaseNames(meta)
	_ = meta.Close() // the metadata pass holds only small values; nothing was opened for re-reads
	if err != nil {
		return nil, err
	}
	kept := map[uint64]bool{}
	for _, d := range names {
		if opts.KeepDatabase(d.Name) {
			kept[uint64(d.ID)] = true //nolint:gosec // ids are non-negative truncated ints
		}
	}
	db, err := leveldb.LoadWith(leveldbDir, leveldb.LoadOptions{Keep: func(k []byte) bool {
		id, store, index, _, err := readPrefix(k)
		if err != nil {
			return false // too short for a prefix: no Scan or Get of this package can reach it
		}
		switch {
		case id == 0, store == 0 && index == 0:
			return true // global and per-database metadata
		case !kept[id]:
			return false
		default:
			return index == indexData || index == indexBlobEntries
		}
	}})
	if err != nil {
		return nil, err
	}
	return &Origin{kv: db, blobDir: blobDir, stats: db.Stats()}, nil
}

// Close releases the LevelDB table files the Origin keeps open for re-reading large values. Call
// it when done with the Origin, on every path. It is safe to call more than once; Records of a
// large value fails after Close.
func (o *Origin) Close() error { return o.kv.Close() }

// Stats reports what the LevelDB reader loaded.
func (o *Origin) Stats() leveldb.Stats { return o.stats }

// Census estimates, per database id, the heap bytes an OpenWith that keeps only that database
// would hold, without holding any record value. It loads the global and per-database metadata
// and observes every other record as it is read: a data or blob entry costs its key plus
// leveldb.EntryOverhead, plus its value when the value is held (any log value, or a table value
// shorter than the lazy threshold). Index entries are not counted because OpenWith never keeps
// them. The estimate counts every stored version of a record, so it can overstate a database
// that was rewritten since its last compaction, never understate it. dbs is what
// Origin.Databases returns for the directory. The directory is only read.
func Census(leveldbDir string) (held map[int64]int64, dbs []Database, err error) {
	held = map[int64]int64{}
	db, err := leveldb.LoadWith(leveldbDir, leveldb.LoadOptions{
		Keep: isMetadataKey,
		Observe: func(k []byte, valueLen int, fromTable bool) {
			id, store, index, _, err := readPrefix(k)
			if err != nil || id == 0 {
				return
			}
			var cost int
			switch {
			case store == 0 && index == 0: // per-database metadata: held by every keep-load
				cost = len(k) + leveldb.EntryOverhead + valueLen
			case index == indexData || index == indexBlobEntries:
				cost = len(k) + leveldb.EntryOverhead
				if !fromTable || valueLen < leveldb.LazyMin() {
					cost += valueLen
				}
			default:
				return
			}
			held[int64(id)] += int64(cost) //nolint:gosec // ids are bounded well below int64
		},
	})
	if err != nil {
		return nil, nil, err
	}
	o := &Origin{kv: db, stats: db.Stats()}
	defer func() { _ = o.Close() }()
	dbs, err = o.Databases()
	if err != nil {
		return nil, nil, err
	}
	return held, dbs, nil
}

// isMetadataKey keeps the global metadata and each database's own metadata (object store
// names and the like), which Databases needs, and nothing else.
func isMetadataKey(k []byte) bool {
	id, store, index, _, err := readPrefix(k)
	return err == nil && (id == 0 || (store == 0 && index == 0))
}
