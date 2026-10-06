package v8

import (
	"math"
	"math/big"
	"strings"
	"testing"
	"time"
)

func TestFormatES(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{0, "0"}, {1, "1"}, {-1, "-1"}, {0.1, "0.1"}, {1.5, "1.5"}, {-1.5, "-1.5"}, {100, "100"},
		{1e21, "1e+21"}, {1e20, "100000000000000000000"}, {1e-7, "1e-7"}, {1e-6, "0.000001"},
		{5e-324, "5e-324"}, {123456789012345680000, "123456789012345680000"},
		{1.7976931348623157e308, "1.7976931348623157e+308"}, {2147483648, "2147483648"},
		{9007199254740993, "9007199254740992"}, {0.000001234, "0.000001234"}, {1.234e-7, "1.234e-7"},
		{1.2345e25, "1.2345e+25"}, {-1e-7, "-1e-7"}, {4.35, "4.35"}, {math.Pi, "3.141592653589793"},
		{math.NaN(), "NaN"}, {math.Inf(1), "Infinity"}, {math.Inf(-1), "-Infinity"},
	}
	for _, tt := range tests {
		if got := FormatES(tt.in); got != tt.want {
			t.Errorf("FormatES(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func canon(t *testing.T, v any) string {
	t.Helper()
	b, err := Canonical(v)
	if err != nil {
		t.Fatalf("Canonical(%#v): %v", v, err)
	}
	return string(b)
}

func TestCanonicalScalars(t *testing.T) {
	bigv, _ := new(big.Int).SetString("-123456789012345678901234567890", 10)
	negZero := math.Copysign(0, -1)
	tests := []struct {
		name string
		in   any
		want string
	}{
		{"null", nil, "null"},
		{"true", true, "true"},
		{"false", false, "false"},
		{"undefined", Undefined{}, `{"$undefined":true}`},
		{"hole", Hole{}, `{"$hole":true}`},
		{"int64", int64(-42), "-42"},
		{"float integral", float64(7), "7"},
		{"float", 0.1, "0.1"},
		{"NaN", math.NaN(), `{"$number":"NaN"}`},
		{"+Inf", math.Inf(1), `{"$number":"Infinity"}`},
		{"-Inf", math.Inf(-1), `{"$number":"-Infinity"}`},
		{"-0", negZero, `{"$number":"-0"}`},
		{"bigint", bigv, `{"$bigint":"-123456789012345678901234567890"}`},
		{"date", time.Date(2020, 1, 2, 3, 4, 5, 100_000_000, time.UTC), `{"$date":"2020-01-02T03:04:05.1Z"}`},
		{"date whole second", time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC), `{"$date":"2020-01-02T03:04:05Z"}`},
		{"date non-UTC zone is converted", time.Date(2020, 1, 2, 3, 4, 5, 0, time.FixedZone("x", 3600)), `{"$date":"2020-01-02T02:04:05Z"}`},
		{"bytes", Bytes{0, 1, 2, 255}, `{"$bytes":"AAEC/w=="}`},
		{"empty bytes", Bytes{}, `{"$bytes":""}`},
		{"regexp", &RegExp{Source: `a"b`, Flags: "gi"}, `{"$regexp":["a\"b","gi"]}`},
		{"error", &Error{Name: "TypeError", Message: "m"}, `{"$error":{"name":"TypeError","message":"m"}}`},
		{"error with stack", &Error{Name: "E", Message: "m", Stack: "s\n", HasStack: true}, `{"$error":{"name":"E","message":"m","stack":"s\n"}}`},
		{"error with empty stack", &Error{Name: "E", HasStack: true}, `{"$error":{"name":"E","message":"","stack":""}}`},
		{"error with null cause", &Error{Name: "E", HasCause: true}, `{"$error":{"name":"E","message":"","cause":null}}`},
		{"error with stack and cause", &Error{Name: "E", Stack: "s", HasStack: true, Cause: int64(1), HasCause: true}, `{"$error":{"name":"E","message":"","stack":"s","cause":1}}`},
		{"invalid date", InvalidDate{}, `{"$date":null}`},
		{"date year 10000", time.Date(10000, 1, 2, 3, 4, 5, 6_000_000, time.UTC), `{"$date":"+010000-01-02T03:04:05.006Z"}`},
		{"date max JS", time.UnixMilli(8_640_000_000_000_000).UTC(), `{"$date":"+275760-09-13T00:00:00Z"}`},
		{"date year 0", time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC), `{"$date":"0000-01-01T00:00:00Z"}`},
		{"date year -1", time.Date(-1, 12, 31, 23, 59, 59, 999_000_000, time.UTC), `{"$date":"-000001-12-31T23:59:59.999Z"}`},
		{"date min JS", time.UnixMilli(-8_640_000_000_000_000).UTC(), `{"$date":"-271821-04-20T00:00:00Z"}`},
		{"array with props", &ArrayWithProps{Items: []any{int64(1)}, Props: &Object{Keys: []string{"k"}, Values: []any{"v"}}}, `{"$array":[1],"$props":{"k":"v"}}`},
		{"array with props and no items", &ArrayWithProps{Items: []any{}, Props: &Object{Keys: []string{"k"}, Values: []any{nil}}}, `{"$array":[],"$props":{"k":null}}`},
		{"wrapper", &Wrapper{Kind: "Number", Value: 1.5}, `{"$wrapper":["Number",1.5]}`},
		{"wrapper string", &Wrapper{Kind: "String", Value: "x"}, `{"$wrapper":["String","x"]}`},
		{"map", &Map{Entries: [][2]any{{"k", int64(1)}, {int64(2), nil}}}, `{"$map":[["k",1],[2,null]]}`},
		{"set", &Set{Items: []any{int64(1), "a"}}, `{"$set":[1,"a"]}`},
		{"empty map", &Map{}, `{"$map":[]}`},
		{"empty set", &Set{}, `{"$set":[]}`},
		{"array", []any{int64(1), Hole{}, "x"}, `[1,{"$hole":true},"x"]`},
		{"empty array", []any{}, `[]`},
		{"object", &Object{Keys: []string{"b", "a"}, Values: []any{int64(1), nil}}, `{"b":1,"a":null}`},
		{"empty object", &Object{}, `{}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := canon(t, tt.in); got != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestCanonicalStringEscapes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "abc", `"abc"`},
		{"quote and backslash", "\"\\", `"\"\\"`},
		{"short escapes", "\b\f\n\r\t", `"\b\f\n\r\t"`},
		{"other controls lowercase hex", "\x00\x01\x1f", `"\u0000\u0001\u001f"`},
		{"DEL is literal", "\x7f", "\"\x7f\""},
		{"slash is literal", "a/b", `"a/b"`},
		{"U+2028 is literal", " ", "\" \""},
		{"non-ASCII literal", "café \U0001F600", "\"café \U0001F600\""},
		{"lone high surrogate", "\xed\xa0\x80", `"\ud800"`},
		{"lone low surrogate", "a\xed\xbf\xbfb", `"a\udfffb"`},
		{"swapped pair stays two escapes", "\xed\xb8\x80\xed\xa0\xbd", `"\ude00\ud83d"`},
		{"valid pair stays literal", "\U0001F600", "\"\U0001F600\""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := canon(t, tt.in); got != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
	// Object keys escape the same way.
	if got := canon(t, &Object{Keys: []string{"\"\xed\xa0\x80"}, Values: []any{nil}}); got != `{"\"\ud800":null}` {
		t.Fatalf("key escaping: %s", got)
	}
}

func TestCanonicalCycles(t *testing.T) {
	a := &Object{Keys: []string{"name", "self"}, Values: []any{"a", nil}}
	a.Values[1] = a
	if got := canon(t, a); got != `{"name":"a","self":{"$cycle":0}}` {
		t.Fatalf("got %s", got)
	}
	arr := make([]any, 2)
	arr[0] = int64(1)
	arr[1] = arr
	if got := canon(t, arr); got != `[1,{"$cycle":0}]` {
		t.Fatalf("got %s", got)
	}
	m := &Map{}
	s := &Set{}
	m.Entries = [][2]any{{"s", s}}
	s.Items = []any{m}
	if got := canon(t, m); got != `{"$map":[["s",{"$set":[{"$cycle":0}]}]]}` {
		t.Fatalf("got %s", got)
	}
	// Shared but acyclic references are written in full each time.
	shared := &Object{Keys: []string{"x"}, Values: []any{int64(1)}}
	if got := canon(t, []any{shared, shared}); got != `[{"x":1},{"x":1}]` {
		t.Fatalf("got %s", got)
	}
}

func TestCanonicalErrorCycle(t *testing.T) {
	e := &Error{Name: "E", HasCause: true}
	e.Cause = e
	if got := canon(t, e); got != `{"$error":{"name":"E","message":"","cause":{"$cycle":0}}}` {
		t.Fatalf("got %s", got)
	}
	// An array cycling through its named property, whether or not it has items.
	for _, items := range [][]any{make([]any, 0, 1), {int64(1)}} {
		a := &ArrayWithProps{Items: items, Props: &Object{Keys: []string{"self"}, Values: []any{nil}}}
		a.Props.Values[0] = a.Items
		got := canon(t, a)
		if !strings.HasSuffix(got, `"$props":{"self":{"$cycle":0}}}`) {
			t.Fatalf("got %s", got)
		}
	}
}

func TestCanonicalErrors(t *testing.T) {
	bad := []any{
		struct{}{},
		int(3),
		&Wrapper{Kind: "Number", Value: struct{}{}},
		[]any{struct{}{}},
		&Object{Keys: []string{"a"}},
	}
	for _, v := range bad {
		if _, err := Canonical(v); err == nil {
			t.Errorf("Canonical(%#v) succeeded, want an error", v)
		}
	}
}
