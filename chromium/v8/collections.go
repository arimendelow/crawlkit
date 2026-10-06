package v8

import (
	"sort"
	"strconv"
)

// readKey reads a property key: a string, or a number converted as JS would.
func (d *decoder) readKey() (string, error) {
	start := d.pos
	v, err := d.readObject()
	if err != nil {
		return "", err
	}
	switch k := v.(type) {
	case string:
		return k, nil
	case int64:
		return strconv.FormatInt(k, 10), nil
	case float64:
		return FormatES(k), nil
	}
	d.pos = start
	return "", d.errorf("property key is %T, not a string or number", v)
}

// readProperties reads key/value pairs until endTag and returns how many there were.
func (d *decoder) readProperties(endTag byte, each func(key string, value any) error) (uint32, error) {
	var n uint32
	for {
		t, ok := d.peekTag()
		if !ok {
			return 0, d.truncated()
		}
		if t == endTag {
			d.pos++
			return n, nil
		}
		key, err := d.readKey()
		if err != nil {
			return 0, err
		}
		value, err := d.readObject()
		if err != nil {
			return 0, err
		}
		if err := each(key, value); err != nil {
			return 0, err
		}
		n++
	}
}

func (d *decoder) expectCount(what string, want uint64) error {
	got, err := d.readVarint()
	if err != nil {
		return err
	}
	if got != want {
		return d.errorf("%s count is %d, expected %d", what, want, got)
	}
	return nil
}

func (d *decoder) readPlainObject() (any, error) {
	obj := &Object{}
	d.register(obj, false)
	index := map[string]int{}
	n, err := d.readProperties(tagEndObject, func(key string, value any) error {
		if i, dup := index[key]; dup {
			obj.Values[i] = value
			return nil
		}
		index[key] = len(obj.Keys)
		obj.Keys = append(obj.Keys, key)
		obj.Values = append(obj.Values, value)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := d.expectCount("property", uint64(n)); err != nil {
		return nil, err
	}
	sortJSKeyOrder(obj)
	return obj, nil
}

// arrayIndex parses s as a canonical array index (0 to 2^32-2).
func arrayIndex(s string) (uint32, bool) {
	if s == "" || len(s) > 10 || (s[0] == '0' && len(s) > 1) {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil || n > 0xfffffffe {
		return 0, false
	}
	return uint32(n), true
}

// sortJSKeyOrder puts integer-like keys first in ascending order, then the
// other keys in their existing (insertion) order, as JS enumerates them.
func sortJSKeyOrder(o *Object) {
	type entry struct {
		key   string
		value any
		index uint32
		isInt bool
	}
	entries := make([]entry, len(o.Keys))
	anyInt, sorted := false, true
	for i, k := range o.Keys {
		idx, ok := arrayIndex(k)
		entries[i] = entry{k, o.Values[i], idx, ok}
		if ok {
			anyInt = true
		}
	}
	if !anyInt {
		return
	}
	for i := 1; i < len(entries); i++ {
		a, b := entries[i-1], entries[i]
		if (!a.isInt && b.isInt) || (a.isInt && b.isInt && a.index > b.index) {
			sorted = false
			break
		}
	}
	if sorted {
		return
	}
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.isInt != b.isInt {
			return a.isInt
		}
		return a.isInt && a.index < b.index
	})
	for i, e := range entries {
		o.Keys[i], o.Values[i] = e.key, e.value
	}
}

// arrayProperties reads the trailing properties of an array. Index keys
// overwrite elements; named properties are collected, in order, into props
// (nil when there are none).
func (d *decoder) arrayProperties(arr []any, endTag byte) (props *Object, n uint32, err error) {
	index := map[string]int{}
	n, err = d.readProperties(endTag, func(key string, value any) error {
		idx, ok := arrayIndex(key)
		if !ok {
			if props == nil {
				props = &Object{}
			}
			if i, dup := index[key]; dup {
				props.Values[i] = value
				return nil
			}
			index[key] = len(props.Keys)
			props.Keys = append(props.Keys, key)
			props.Values = append(props.Values, value)
			return nil
		}
		if uint64(idx) >= uint64(len(arr)) { //nolint:gosec // len is never negative
			return d.errorf("array index %d beyond length %d", idx, len(arr))
		}
		arr[idx] = value
		return nil
	})
	return props, n, err
}

// newArray makes the slice for an array. Zero-length arrays get capacity 1 so
// every array has distinct storage to identify it by when it is part of a cycle.
func newArray(length uint32) []any {
	if length == 0 {
		return make([]any, 0, 1)
	}
	return make([]any, length)
}

// finishArray returns arr, or an *ArrayWithProps when it carries named properties.
func finishArray(arr []any, props *Object) any {
	if props == nil {
		return arr
	}
	return &ArrayWithProps{Items: arr, Props: props}
}

func (d *decoder) readSparseArray() (any, error) {
	length, err := d.readVarint32()
	if err != nil {
		return nil, err
	}
	if length > maxSparseLength {
		return nil, d.errorf("sparse array length %d above the supported %d", length, maxSparseLength)
	}
	arr := newArray(length)
	for i := range arr {
		arr[i] = Hole{}
	}
	d.register(arr, false)
	props, n, err := d.arrayProperties(arr, tagEndSparse)
	if err != nil {
		return nil, err
	}
	if err := d.expectCount("property", uint64(n)); err != nil {
		return nil, err
	}
	if err := d.expectCount("array length", uint64(length)); err != nil {
		return nil, err
	}
	return finishArray(arr, props), nil
}

func (d *decoder) readDenseArray() (any, error) {
	length, err := d.readVarint32()
	if err != nil {
		return nil, err
	}
	// Each element takes at least one byte.
	if uint64(length) > uint64(len(d.b)-d.pos) { //nolint:gosec // the remaining length is never negative
		return nil, d.errorf("dense array length %d exceeds remaining data", length)
	}
	arr := newArray(length)
	d.register(arr, false)
	for i := range arr {
		if t, ok := d.peekTag(); ok && t == tagTheHole {
			d.pos++
			arr[i] = Hole{}
			continue
		}
		v, err := d.readObject()
		if err != nil {
			return nil, err
		}
		arr[i] = v
	}
	props, n, err := d.arrayProperties(arr, tagEndDense)
	if err != nil {
		return nil, err
	}
	if err := d.expectCount("property", uint64(n)); err != nil {
		return nil, err
	}
	if err := d.expectCount("array length", uint64(length)); err != nil {
		return nil, err
	}
	return finishArray(arr, props), nil
}

func (d *decoder) readMap() (any, error) {
	m := &Map{}
	d.register(m, false)
	for {
		t, ok := d.peekTag()
		if !ok {
			return nil, d.truncated()
		}
		if t == tagEndMap {
			d.pos++
			break
		}
		k, err := d.readObject()
		if err != nil {
			return nil, err
		}
		v, err := d.readObject()
		if err != nil {
			return nil, err
		}
		m.Entries = append(m.Entries, [2]any{k, v})
	}
	if err := d.expectCount("map entry", uint64(len(m.Entries))*2); err != nil {
		return nil, err
	}
	return m, nil
}

func (d *decoder) readSet() (any, error) {
	s := &Set{}
	d.register(s, false)
	for {
		t, ok := d.peekTag()
		if !ok {
			return nil, d.truncated()
		}
		if t == tagEndSet {
			d.pos++
			break
		}
		v, err := d.readObject()
		if err != nil {
			return nil, err
		}
		s.Items = append(s.Items, v)
	}
	if err := d.expectCount("set item", uint64(len(s.Items))); err != nil {
		return nil, err
	}
	return s, nil
}
