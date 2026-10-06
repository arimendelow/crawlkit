package indexeddb

import (
	"bytes"
	"math"
	"reflect"
	"testing"
	"time"
)

func TestKeyPrefix(t *testing.T) {
	cases := []struct {
		db, store, idx uint64
		end            []byte
		want           []byte
	}{
		{0, 0, 0, nil, []byte{0x00, 0x00, 0x00, 0x00}},
		{1, 1, 1, nil, []byte{0x00, 0x01, 0x01, 0x01}},
		{0, 0, 0, []byte{50}, []byte{0x00, 0x00, 0x00, 0x00, 50}},
		// db id needs two bytes: (2-1)<<5 = 0x20; little-endian 0x1234.
		{0x1234, 2, 1, nil, []byte{0x20, 0x34, 0x12, 0x02, 0x01}},
		// store id two bytes: (1<<2) = 0x04, index id 3 bytes adds 2.
		{3, 0x0102, 0x010203, nil, []byte{0x04 | 0x02, 0x03, 0x02, 0x01, 0x03, 0x02, 0x01}},
	}
	for _, c := range cases {
		got, err := makePrefix(c.db, c.store, c.idx, c.end...)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, c.want) {
			t.Errorf("makePrefix(%d,%d,%d) = % x, want % x", c.db, c.store, c.idx, got, c.want)
		}
		if len(c.end) == 0 {
			db, st, ix, n, err := readPrefix(got)
			if err != nil || db != c.db || st != c.store || ix != c.idx || n != len(got) {
				t.Errorf("readPrefix(% x) = %d %d %d %d %v", got, db, st, ix, n, err)
			}
		}
	}
	if _, err := makePrefix(1, 1, 1<<32, nil...); err == nil {
		t.Error("expected error for 5-byte index id")
	}
	if _, _, _, _, err := readPrefix([]byte{0x20, 0x01}); err == nil {
		t.Error("expected error for short prefix")
	}
}

func TestDatabaseNameKey(t *testing.T) {
	key := dbNameKey("https://x", "app:db")
	if !bytes.HasPrefix(key, []byte{0, 0, 0, 0, 201}) {
		t.Fatal("bad prefix")
	}
	f := fakeKV{string(key): {0x05}}
	f[string(storeNameKey(5, 1))] = u16("things")
	o := newTestOrigin(f, "")
	dbs, err := o.Databases()
	if err != nil {
		t.Fatal(err)
	}
	want := []Database{{ID: 5, Origin: "https://x", Name: "app:db", Stores: []Store{{ID: 1, Name: "things"}}}}
	if !reflect.DeepEqual(dbs, want) {
		t.Fatalf("got %+v want %+v", dbs, want)
	}
}

func TestObjectStoreNameKey(t *testing.T) {
	// Type 50 object store metadata; metadata type 0 is the name, others are ignored.
	f := fakeKV{
		string(dbNameKey("o", "d")):          {0x02},
		string(storeNameKey(2, 7)):           u16("alpha"),
		string([]byte{0, 2, 0, 0, 50, 7, 1}): {0x00},
		string(storeNameKey(2, 3)):           u16("béta"),
	}
	dbs, err := newTestOrigin(f, "").Databases()
	if err != nil {
		t.Fatal(err)
	}
	if len(dbs) != 1 || !reflect.DeepEqual(dbs[0].Stores, []Store{{3, "béta"}, {7, "alpha"}}) {
		t.Fatalf("got %+v", dbs)
	}
}

func TestIDBKeyDecoding(t *testing.T) {
	num := func(f float64) []byte {
		b := []byte{3, 0, 0, 0, 0, 0, 0, 0, 0}
		u := math.Float64bits(f)
		for i := 0; i < 8; i++ {
			b[1+i] = byte(u >> (8 * i)) //nolint:gosec // bounded by earlier length checks
		}
		return b
	}
	date := num(1e3)
	date[0] = 2
	str := append([]byte{1}, idbString("hi☃")...)
	bin := []byte{6, 3, 1, 2, 3}
	arr := append([]byte{4, 3}, append(append(append([]byte{}, num(1.5)...), str...), bin...)...)

	cases := []struct {
		name string
		in   []byte
		want any
	}{
		{"null", []byte{0}, nil},
		{"number", num(42.5), 42.5},
		{"string", str, "hi☃"},
		{"date", date, time.UnixMilli(1000).UTC()},
		{"binary", bin, []byte{1, 2, 3}},
		{"array", arr, []any{1.5, "hi☃", []byte{1, 2, 3}}},
	}
	for _, c := range cases {
		got, n, err := decodeKey(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if n != len(c.in) {
			t.Errorf("%s: consumed %d of %d", c.name, n, len(c.in))
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %#v want %#v", c.name, got, c.want)
		}
	}
	for _, bad := range [][]byte{{}, {1, 5, 0}, {3, 1, 2}, {5}, {9}, {6, 4, 1}} {
		if _, _, err := decodeKey(bad); err == nil {
			t.Errorf("expected error for % x", bad)
		}
	}
}
