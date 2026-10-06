package leveldb

import "testing"

func mustLoad(t *testing.T, dir string) *DB {
	t.Helper()
	d, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func mustLoadWith(t *testing.T, dir string, opts LoadOptions) *DB {
	t.Helper()
	d, err := LoadWith(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}
