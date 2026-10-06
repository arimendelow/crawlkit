package indexeddb

import (
	"encoding/hex"
	"fmt"
	"sort"
)

const (
	metaDatabaseName  = 201 // global key type: database name to id
	metaObjectStore   = 50  // per-database key type: object store metadata
	indexData         = 1
	indexBlobEntries  = 3
	storeMetaName     = 0
	externalBlob      = 0
	externalFile      = 1
	maxReasonableDBID = 1 << 40
)

// Database is one IndexedDB database in the origin.
type Database struct {
	ID     int64
	Origin string
	Name   string
	Stores []Store
}

// Store is one object store in a database.
type Store struct {
	ID   int64
	Name string
}

// Databases lists every database and its object stores, ordered by id.
func (o *Origin) Databases() ([]Database, error) {
	dbs, err := databaseNames(o.kv)
	if err != nil {
		return nil, err
	}
	for i := range dbs {
		stores, err := o.stores(dbs[i].ID)
		if err != nil {
			return nil, err
		}
		dbs[i].Stores = stores
	}
	sort.Slice(dbs, func(i, j int) bool { return dbs[i].ID < dbs[j].ID })
	return dbs, nil
}

// databaseNames lists every database's id, origin and name from the global metadata, without stores.
func databaseNames(kv kv) ([]Database, error) {
	prefix := []byte{0, 0, 0, 0, metaDatabaseName}
	var dbs []Database
	err := kv.Scan(prefix, func(k, v []byte) error {
		rest := k[len(prefix):]
		origin, n, err := readUTF16(rest)
		if err != nil {
			return fmt.Errorf("indexeddb: database name key: %w", err)
		}
		name, _, err := readUTF16(rest[n:])
		if err != nil {
			return fmt.Errorf("indexeddb: database name key: %w", err)
		}
		id, err := decodeTruncatedInt(v)
		if err != nil {
			return fmt.Errorf("indexeddb: database id: %w", err)
		}
		dbs = append(dbs, Database{ID: id, Origin: origin, Name: name})
		return nil
	})
	return dbs, err
}

func (o *Origin) stores(dbID int64) ([]Store, error) {
	if dbID < 0 || dbID > maxReasonableDBID {
		return nil, fmt.Errorf("indexeddb: database id %d out of range", dbID)
	}
	prefix := idPrefix(uint64(dbID), 0, 0, metaObjectStore)
	var out []Store
	err := o.kv.Scan(prefix, func(k, v []byte) error {
		rest := k[len(prefix):]
		id, n, err := readVarint(rest)
		if err != nil || n >= len(rest) {
			return fmt.Errorf("indexeddb: object store key: %w", errShort)
		}
		if rest[n] != storeMetaName || n+1 != len(rest) {
			return nil
		}
		out = append(out, Store{ID: int64(id), Name: decodeUTF16BE(v)}) //nolint:gosec // bounded by earlier length checks
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, err
}

// Record is one object store entry. Err is an *OmissionError (code bad_key)
// when the record key could not be decoded; Key is then nil. Raw is the stored
// value without its leading IndexedDB version varint; pass it to Decode.
// Raw aliases the LevelDB reader's copy of the value: treat it as read-only. Raw is empty for an
// empty value. For blob-replaced values Records rewrites the blob index to
// the blob number, so Raw is self-contained.
type Record struct {
	Key any
	Raw []byte
	Err error
}

// Records calls fn for every record in the object store, in key order.
func (o *Origin) Records(dbID, storeID int64, fn func(Record) error) error {
	if dbID < 0 || storeID < 0 {
		return fmt.Errorf("indexeddb: negative id")
	}
	prefix := idPrefix(uint64(dbID), uint64(storeID), indexData)
	return o.kv.Scan(prefix, func(k, v []byte) error {
		rawKey := k[len(prefix):]
		key, n, err := decodeKey(rawKey)
		if err == nil && n != len(rawKey) {
			err = fmt.Errorf("%d trailing bytes", len(rawKey)-n)
		}
		if err != nil {
			h := rawKey[:min(len(rawKey), envelopeHexBytes)]
			return fn(Record{Raw: v, Err: &OmissionError{Omission{Code: CodeBadKey, Detail: fmt.Sprintf("undecodable record key (%v); key %s", err, hex.EncodeToString(h))}}})
		}
		var raw []byte
		if len(v) > 0 {
			_, used, err := readVarint(v)
			if err != nil {
				return fmt.Errorf("indexeddb: record version: %w", err)
			}
			raw = v[used:]
			if EnvelopeKind(raw) == "blob" {
				resolved, err := o.resolveBlobRef(uint64(dbID), uint64(storeID), rawKey, raw) //nolint:gosec // bounded by earlier length checks
				if err != nil {
					return err
				}
				raw = resolved
			}
		}
		return fn(Record{Key: key, Raw: raw})
	})
}

// externalObjects parses the blob entry value for one record.
func externalObjects(v []byte) ([]uint64, error) {
	var nums []uint64
	for len(v) > 0 {
		typ := v[0]
		v = v[1:]
		if typ != externalBlob && typ != externalFile {
			return nil, fmt.Errorf("indexeddb: unsupported external object type %d", typ)
		}
		num, n, err := readVarint(v)
		if err != nil {
			return nil, err
		}
		v = v[n:]
		_, n, err = readUTF16(v) // mime type
		if err != nil {
			return nil, err
		}
		v = v[n:]
		_, n, err = readVarint(v) // size
		if err != nil {
			return nil, err
		}
		v = v[n:]
		if typ == externalFile {
			if _, n, err = readUTF16(v); err != nil { // file name
				return nil, err
			}
			v = v[n:]
			if _, n, err = readVarint(v); err != nil { // last modified
				return nil, err
			}
			v = v[n:]
		}
		nums = append(nums, num)
	}
	return nums, nil
}
