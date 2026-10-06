package leveldb

import "testing"

func TestApplyBatchMalformed(t *testing.T) {
	s := newStore(nil)
	good := batch(1, "a", "1", "b", "2")
	if !applyBatch(good, s) || len(s.m) != 2 {
		t.Fatalf("good batch: %v", s)
	}
	s2 := newStore(nil)
	// Cut mid-record: earlier records survive, result reports malformed.
	if applyBatch(good[:len(good)-2], s2) {
		t.Fatal("truncated batch accepted")
	}
	if _, ok := s2.m["a"]; !ok {
		t.Fatal("record before defect lost")
	}
	if applyBatch([]byte{1, 2, 3}, newStore(nil)) {
		t.Fatal("short batch accepted")
	}
	bad := append([]byte(nil), good[:12]...)
	bad = append(bad, 9) // unknown record type
	if applyBatch(bad, newStore(nil)) {
		t.Fatal("unknown type accepted")
	}
}
