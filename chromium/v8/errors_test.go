package v8

import (
	"errors"
	"strings"
	"testing"
)

// obj builds the wire bytes of an object whose properties all hold the int 1.
func obj(keys ...string) []byte {
	b := []byte{0xff, 15, 'o'}
	for _, k := range keys {
		b = append(b, '"', byte(len(k))) //nolint:gosec // test keys are short
		b = append(b, k...)
		b = append(b, 'I', 2)
	}
	return append(b, '{', byte(len(keys))) //nolint:gosec // a few keys
}

func TestObjectKeyOrder(t *testing.T) {
	tests := []struct {
		name string
		keys []string
		want []string
	}{
		{"named only keep insertion order", []string{"b", "a"}, []string{"b", "a"}},
		{"integer keys already first and ascending", []string{"0", "1", "a"}, []string{"0", "1", "a"}},
		{"integer after named moves first", []string{"b", "0"}, []string{"0", "b"}},
		{"integer keys descending are sorted", []string{"2", "1"}, []string{"1", "2"}},
		{"mixed", []string{"z", "10", "a", "2"}, []string{"2", "10", "z", "a"}},
		{"leading zero is a named key", []string{"01", "1"}, []string{"1", "01"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := Deserialize(obj(tt.keys...))
			if err != nil {
				t.Fatal(err)
			}
			o := v.(*Object)
			if strings.Join(o.Keys, ",") != strings.Join(tt.want, ",") || len(o.Values) != len(o.Keys) {
				t.Fatalf("keys = %v, want %v", o.Keys, tt.want)
			}
			for i, val := range o.Values {
				if val != int64(1) {
					t.Fatalf("value %d = %v: values did not move with their keys", i, val)
				}
			}
		})
	}
}

func TestArrayDuplicateNamedPropertyKeepsFirstPosition(t *testing.T) {
	in := []byte{0xff, 15, 'A', 0,
		'"', 1, 'x', 'I', 2,
		'"', 1, 'y', 'I', 2,
		'"', 1, 'x', 'I', 4,
		'$', 3, 0}
	v, err := Deserialize(in)
	if err != nil {
		t.Fatal(err)
	}
	a := v.(*ArrayWithProps)
	if strings.Join(a.Props.Keys, ",") != "x,y" || a.Props.Values[0] != int64(2) || a.Props.Values[1] != int64(1) {
		t.Fatalf("props = %+v", a.Props)
	}
}

// Version 13 data written by the Chromium builds that put view flags in anyway decodes on the
// second attempt, with the flags read.
func TestVersion13BrokenViewFlagsRetry(t *testing.T) {
	in := []byte{0xff, 13, 'B', 2, 1, 2, 'V', 'B', 0, 2, 1}
	v, err := Deserialize(in)
	if err != nil {
		t.Fatal(err)
	}
	if b, ok := v.(Bytes); !ok || string(b) != "\x01\x02" {
		t.Fatalf("got %#v", v)
	}
}

func TestMalformedMore(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
	}{
		{"utf8 string length past end", []byte{0xff, 15, 'S', 9, 'a'}},
		{"two-byte string length past end", []byte{0xff, 15, 'c', 8, 'a', 0}},
		{"one-byte string without length", []byte{0xff, 15, '"'}},
		{"two-byte string without length", []byte{0xff, 15, 'c'}},
		{"utf8 string without length", []byte{0xff, 15, 'S'}},
		{"reference to an object still being built", []byte{0xff, 15, 's', '^', 0}},
		{"reference without an id", []byte{0xff, 15, '^'}},
		{"resizable buffer without a maximum", []byte{0xff, 16, '~', 0}},
		{"view sub-tag above one byte", []byte{0xff, 15, 'B', 2, 1, 2, 'V', 0x80, 0x02, 0, 0, 0}},
		{"view flags cut off", []byte{0xff, 15, 'B', 2, 1, 2, 'V', 'B', 0, 2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if v, err := Deserialize(tt.in); err == nil {
				t.Fatalf("expected an error, got %#v", v)
			}
		})
	}
}

func TestStringDecodingEdges(t *testing.T) {
	// Invalid UTF-8 in an 'S' string becomes one U+FFFD per bad byte.
	v, err := Deserialize([]byte{0xff, 15, 'S', 3, 'a', 0xff, 'b'})
	if err != nil || v != "a�b" {
		t.Fatalf("got %q, %v", v, err)
	}
	// Two-byte strings: a lone low surrogate at the end, and a high surrogate followed by a
	// non-surrogate, both come out as WTF-8 so the code units survive.
	v, err = Deserialize([]byte{0xff, 15, 'c', 2, 0x00, 0xDC})
	if err != nil || v != "\xed\xb0\x80" {
		t.Fatalf("low surrogate: %q, %v", v, err)
	}
	v, err = Deserialize([]byte{0xff, 15, 'c', 4, 0x00, 0xD8, 'a', 0})
	if err != nil || v != "\xed\xa0\x80a" {
		t.Fatalf("high surrogate then letter: %q, %v", v, err)
	}
}

func TestErrorTypesDescribeThemselves(t *testing.T) {
	_, err := Deserialize([]byte{0xff, 99})
	var ve *VersionError
	if !errors.As(err, &ve) || !strings.Contains(ve.Error(), "99") {
		t.Fatalf("version err = %v", err)
	}
	_, err = Deserialize([]byte{0xff, 15, '\\'})
	var ue *UnsupportedError
	if !errors.As(err, &ue) || !strings.Contains(ue.Error(), "v8_host_object") || !strings.Contains(ue.Error(), "0x5c") {
		t.Fatalf("unsupported err = %v", err)
	}
}

func TestCanonicalNestedErrors(t *testing.T) {
	bad := struct{}{}
	badObj := &Object{Keys: []string{"a"}} // keys and values disagree
	tests := map[string]any{
		"error cause":            &Error{Name: "E", HasCause: true, Cause: bad},
		"array items":            &ArrayWithProps{Items: []any{bad}},
		"array props":            &ArrayWithProps{Items: []any{}, Props: badObj},
		"wrapper value":          &Wrapper{Kind: "Number", Value: bad},
		"object value":           &Object{Keys: []string{"a"}, Values: []any{bad}},
		"map key":                &Map{Entries: [][2]any{{bad, nil}}},
		"map value":              &Map{Entries: [][2]any{{nil, bad}}},
		"second map entry value": &Map{Entries: [][2]any{{nil, nil}, {nil, bad}}},
		"set item":               &Set{Items: []any{bad}},
		"second array item":      []any{nil, bad},
	}
	for name, v := range tests {
		if got, err := Canonical(v); err == nil {
			t.Errorf("%s: Canonical = %s, want an error", name, got)
		}
	}
}

func TestCanonicalStringInvalidUTF8(t *testing.T) {
	if got := canon(t, "a\xffb"); got != "\"a�b\"" {
		t.Fatalf("got %s", got)
	}
}

func TestCanonicalArrayWithoutNamedProps(t *testing.T) {
	got := canon(t, &ArrayWithProps{Items: []any{int64(1)}})
	if got != `{"$array":[1],"$props":{}}` {
		t.Fatalf("got %s", got)
	}
}
