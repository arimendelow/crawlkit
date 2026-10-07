package v8

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const vectorDir = "testdata/vectors"

// readFile reads a test fixture; every path is built from the testdata directory or a t.TempDir.
func readFile(path string) ([]byte, error) {
	return os.ReadFile(path) //nolint:gosec // test fixture path
}

func readVectors(t testing.TB) []string {
	t.Helper()
	bins, err := filepath.Glob(filepath.Join(vectorDir, "*.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if len(bins) < 200 {
		t.Fatalf("expected a vector corpus of at least 200 files, found %d (run node testdata/gen/gen.mjs)", len(bins))
	}
	return bins
}

func TestVectors(t *testing.T) {
	for _, bin := range readVectors(t) {
		name := strings.TrimSuffix(filepath.Base(bin), ".bin")
		t.Run(name, func(t *testing.T) {
			in, err := readFile(bin)
			if err != nil {
				t.Fatal(err)
			}
			want, err := readFile(strings.TrimSuffix(bin, ".bin") + ".json")
			if err != nil {
				t.Fatal(err)
			}
			v, err := Deserialize(in)
			if err != nil {
				t.Fatalf("Deserialize: %v", err)
			}
			got, err := Canonical(v)
			if err != nil {
				t.Fatalf("Canonical: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("canonical mismatch\n got: %.400s\nwant: %.400s", got, want)
			}
		})
	}
}

func TestGeneratorIsCurrent(t *testing.T) {
	node, lookErr := exec.LookPath("node")
	major := ""
	if lookErr == nil {
		ver, err := exec.Command(node, "-p", "process.versions.node.split('.')[0]").Output() //nolint:gosec // node path comes from LookPath
		if err != nil {
			lookErr = err
		}
		major = strings.TrimSpace(string(ver))
	}
	skip, fail := nodeGate(os.Getenv("CRAWLKIT_REQUIRE_NODE") != "", lookErr, major)
	if fail != "" {
		t.Fatal(fail)
	}
	if skip != "" {
		t.Skip(skip)
	}
	out := t.TempDir()
	cmd := exec.Command(node, "testdata/gen/gen.mjs", "--out", out) //nolint:gosec // node path comes from LookPath
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("gen.mjs: %v\n%s", err, msg)
	}
	compareTrees(t, out, vectorDir)
	compareTrees(t, filepath.Join(out, "errors"), filepath.Join(vectorDir, "errors"))
}

func compareTrees(t *testing.T, generated, committed string) {
	t.Helper()
	list := func(dir string) map[string]bool {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]bool{}
		for _, e := range entries {
			if !e.IsDir() {
				m[e.Name()] = true
			}
		}
		return m
	}
	gen, com := list(generated), list(committed)
	for name := range gen {
		if !com[name] {
			t.Errorf("%s is generated but not committed; run node testdata/gen/gen.mjs", name)
			continue
		}
		a, _ := readFile(filepath.Join(generated, name))
		b, _ := readFile(filepath.Join(committed, name))
		if !bytes.Equal(a, b) {
			t.Errorf("%s differs from gen.mjs output; run node testdata/gen/gen.mjs", name)
		}
	}
	for name := range com {
		if !gen[name] {
			t.Errorf("%s is committed but no longer generated; run node testdata/gen/gen.mjs", name)
		}
	}
}

func TestUnknownTag(t *testing.T) {
	// 'o' at offset 2, key "a" at 3..5, then the bad tag 0x01 as the value at offset 6.
	in := []byte{0xff, 15, 'o', '"', 1, 'a', 0x01}
	_, err := Deserialize(in)
	var ue *UnsupportedError
	if !errors.As(err, &ue) {
		t.Fatalf("want UnsupportedError, got %v", err)
	}
	if ue.Code != "v8_unknown_tag" || ue.Tag != 0x01 || ue.Offset != 6 {
		t.Fatalf("got %+v", ue)
	}
}

func TestUnsupportedTags(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		code string
		tag  byte
	}{
		{"host object", []byte{0xff, 15, '\\', 1, 2, 3}, "v8_host_object", '\\'},
		{"array buffer transfer", []byte{0xff, 15, 't', 0}, "v8_host_object", 't'},
		{"wasm module", []byte{0xff, 15, 'w', 0}, "v8_host_object", 'w'},
		{"wasm memory", []byte{0xff, 15, 'm', 0}, "v8_host_object", 'm'},
		{"shared object", []byte{0xff, 15, 'p', 0}, "v8_shared", 'p'},
		{"shared array buffer", []byte{0xff, 15, 'u', 0}, "v8_shared", 'u'},
		{"shared immutable array buffer", []byte{0xff, 16, 'E', 0}, "v8_shared", 'E'},
		{"the hole outside an array", []byte{0xff, 15, '-'}, "v8_unknown_tag", '-'},
		{"shared object before version 15", []byte{0xff, 14, 'p', 0}, "v8_unknown_tag", 'p'},
		{"host object nested", []byte{0xff, 15, 'A', 1, '\\', 0}, "v8_host_object", '\\'},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Deserialize(tt.in)
			var ue *UnsupportedError
			if !errors.As(err, &ue) {
				t.Fatalf("want UnsupportedError, got %v", err)
			}
			if ue.Code != tt.code || ue.Tag != tt.tag {
				t.Fatalf("got %+v, want code %s tag %q", ue, tt.code, tt.tag)
			}
			if ue.Offset != bytes.IndexByte(tt.in[2:], tt.tag)+2 {
				t.Fatalf("offset %d does not point at the tag", ue.Offset)
			}
		})
	}
}

func TestHostObject(t *testing.T) {
	for _, name := range []string{"host_buffer.bin", "host_uint8array.bin"} {
		in, err := readFile(filepath.Join(vectorDir, "errors", name))
		if err != nil {
			t.Fatal(err)
		}
		_, err = Deserialize(in)
		var ue *UnsupportedError
		if !errors.As(err, &ue) || ue.Code != "v8_host_object" || ue.Tag != '\\' {
			t.Fatalf("%s: got %v", name, err)
		}
	}
}

func TestTruncated(t *testing.T) {
	for _, bin := range readVectors(t) {
		in, err := readFile(bin)
		if err != nil {
			t.Fatal(err)
		}
		end := len(in)
		for end > 2 && in[end-1] == 0 {
			end-- // trailing padding is legal, so cutting into it is not a truncation
		}
		for n := 0; n < end; n++ {
			v, err := Deserialize(in[:n])
			if _, bare := v.(Bytes); err == nil && bare && strings.HasPrefix(filepath.Base(bin), "view_") {
				continue // a view vector cut just before the view tag is a valid bare ArrayBuffer
			}
			if err == nil {
				t.Fatalf("%s truncated to %d of %d bytes decoded without error", filepath.Base(bin), n, len(in))
			}
		}
	}
}

func TestVersionHandling(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		ok   bool
	}{
		{"version 15", []byte{0xff, 15, '0'}, true},
		{"version 16", []byte{0xff, 16, '0'}, true},
		{"version 13", []byte{0xff, 13, '0'}, true},
		{"version 12 is older than supported", []byte{0xff, 12, '0'}, false},
		{"version 1 is older than supported", []byte{0xff, 1, '0'}, false},
		{"future version 17", []byte{0xff, 17, '0'}, false},
		{"version 0 legacy", []byte{0xff, 0, '0'}, false},
		{"no header", []byte{'0'}, false},
		{"empty", nil, false},
		{"header only", []byte{0xff, 15}, false},
		{"header version varint cut", []byte{0xff, 0x8f}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Deserialize(tt.in)
			if (err == nil) != tt.ok {
				t.Fatalf("err = %v, want ok=%v", err, tt.ok)
			}
		})
	}
}

func TestVersionErrorIsNamed(t *testing.T) {
	for _, v := range []byte{0, 1, 12, 17, 99} {
		_, err := Deserialize([]byte{0xff, v, '0'})
		var ve *VersionError
		if !errors.As(err, &ve) || ve.Version != uint64(v) {
			t.Fatalf("version %d: got %v, want *VersionError", v, err)
		}
	}
}

func TestInvalidDate(t *testing.T) {
	v, err := Deserialize(mustRead(t, "date_invalid_nan.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := v.(InvalidDate); !ok {
		t.Fatalf("got %#v, want InvalidDate", v)
	}
	// A record with an invalid Date still decodes around it.
	v, err = Deserialize([]byte{0xff, 15, 'A', 2, 'D', 0, 0, 0, 0, 0, 0, 0xf8, 0x7f, 'I', 2, '$', 0, 2})
	if err != nil {
		t.Fatal(err)
	}
	a := v.([]any)
	if _, ok := a[0].(InvalidDate); !ok || a[1].(int64) != 1 {
		t.Fatalf("got %#v", a)
	}
}

func mustRead(t *testing.T, name string) []byte {
	t.Helper()
	b, err := readFile(filepath.Join(vectorDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestNodeGate(t *testing.T) {
	tests := []struct {
		name       string
		ci         bool
		lookErr    error
		major      string
		skip, fail bool
	}{
		{"local without node skips", false, errors.New("no node"), "", true, false},
		{"local with wrong node skips", false, nil, "24", true, false},
		{"local with node 22 runs", false, nil, "22", false, false},
		{"CI without node fails", true, errors.New("no node"), "", false, true},
		{"CI with wrong node fails", true, nil, "24", false, true},
		{"CI with node 22 runs", true, nil, "22", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			skip, fail := nodeGate(tt.ci, tt.lookErr, tt.major)
			if (skip != "") != tt.skip || (fail != "") != tt.fail {
				t.Fatalf("skip=%q fail=%q", skip, fail)
			}
		})
	}
}

func TestMalformed(t *testing.T) {
	deep := append([]byte{0xff, 15}, bytes.Repeat([]byte{'A', 1}, 5000)...)
	tests := []struct {
		name string
		in   []byte
	}{
		{"two-byte string odd length", []byte{0xff, 15, 'c', 3, 'a', 0, 'b'}},
		{"string length past end", []byte{0xff, 15, '"', 9, 'a'}},
		{"object property count mismatch", []byte{0xff, 15, 'o', '"', 1, 'a', 'I', 2, '{', 2}},
		{"dense array count mismatch", []byte{0xff, 15, 'A', 0, '$', 1, 0}},
		{"dense array length mismatch", []byte{0xff, 15, 'A', 0, '$', 0, 1}},
		{"sparse array length mismatch", []byte{0xff, 15, 'a', 2, '@', 0, 3}},
		{"dense array longer than input", []byte{0xff, 15, 'A', 0x80, 0x80, 0x80, 0x08, '$', 0, 0}},
		{"sparse array absurd length", []byte{0xff, 15, 'a', 0xff, 0xff, 0xff, 0xff, 0x0f, '@', 0, 0xff, 0xff, 0xff, 0xff, 0x0f}},
		{"sparse array index past length", []byte{0xff, 15, 'a', 1, 'I', 10, 'I', 2, '@', 1, 1}},
		{"map odd entries", []byte{0xff, 15, ';', 'I', 2, ':', 1}},
		{"map count mismatch", []byte{0xff, 15, ';', 'I', 2, 'I', 2, ':', 4}},
		{"set count mismatch", []byte{0xff, 15, '\'', 'I', 2, ',', 2}},
		{"reference to nothing", []byte{0xff, 15, '^', 0}},
		{"reference out of range", []byte{0xff, 15, 'A', 1, '^', 7, '$', 0, 1}},
		{"reference to a primitive id", []byte{0xff, 15, 'A', 2, 'I', 2, '^', 1, '$', 0, 2}},
		{"nesting too deep", deep},
		{"trailing data", []byte{0xff, 15, '0', '0'}},
		{"view without buffer", []byte{0xff, 15, '0', 'V', 'B', 0, 0, 0}},
		{"view past buffer", []byte{0xff, 15, 'B', 2, 1, 2, 'V', 'B', 1, 2, 0}},
		{"view with unknown subtag", []byte{0xff, 15, 'B', 2, 1, 2, 'V', 'Z', 0, 2, 0}},
		{"typed array misaligned", []byte{0xff, 15, 'B', 4, 1, 2, 3, 4, 'V', 'w', 1, 2, 0}},
		{"buffer length past end", []byte{0xff, 15, 'B', 9, 1}},
		{"resizable length above max", []byte{0xff, 16, '~', 4, 2, 1, 2, 3, 4}},
		{"date out of range", append([]byte{0xff, 15, 'D'}, 0, 0, 0, 0, 0, 0, 0xf0, 0x7f)},
		{"regexp unknown flag bit", []byte{0xff, 15, 'R', '"', 1, 'a', 0x80, 0x40}},
		{"regexp pattern not a string", []byte{0xff, 15, 'R', 'I', 2, 0}},
		{"object key is an object", []byte{0xff, 15, 'o', 'o', '{', 0, 'I', 2, '{', 1}},
		{"object key is null", []byte{0xff, 15, 'o', '0', 'I', 2, '{', 1}},
		{"string wrapper holds a number", []byte{0xff, 15, 's', 'I', 2}},
		{"error missing end", []byte{0xff, 15, 'r', 'm', '"', 1, 'x'}},
		{"error unknown subtag", []byte{0xff, 15, 'r', '?'}},
		{"bigint length past end", []byte{0xff, 15, 'Z', 0x10, 1, 2}},
		{"varint too long", []byte{0xff, 15, 'U', 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x01}},
		{"double cut short", []byte{0xff, 15, 'N', 1, 2, 3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := Deserialize(tt.in)
			if err == nil {
				t.Fatalf("expected an error, got %#v", v)
			}
		})
	}
}

func TestDecodedTypes(t *testing.T) {
	type check func(t *testing.T, v any)
	tests := []struct {
		vector string
		check  check
	}{
		{"oddball_null", func(t *testing.T, v any) { mustEq(t, v == nil, true) }},
		{"oddball_undefined", func(t *testing.T, v any) { _ = v.(Undefined) }},
		{"oddball_true", func(t *testing.T, v any) { mustEq(t, v.(bool), true) }},
		{"number_smi_max", func(t *testing.T, v any) { mustEq(t, v.(int64), int64(1<<30-1)) }},
		{"number_smi_min", func(t *testing.T, v any) { mustEq(t, v.(int64), int64(-(1 << 30))) }},
		{"number_int32_min", func(t *testing.T, v any) { mustEq(t, v.(int64), int64(math.MinInt32)) }},
		{"number_utag_max", func(t *testing.T, v any) { mustEq(t, v.(int64), int64(math.MaxUint32)) }},
		{"number_half", func(t *testing.T, v any) { mustEq(t, v.(float64), 0.5) }},
		{"number_nan", func(t *testing.T, v any) { mustEq(t, math.IsNaN(v.(float64)), true) }},
		{"number_neg_zero", func(t *testing.T, v any) { mustEq(t, math.Signbit(v.(float64)), true) }},
		{"bigint_two_64", func(t *testing.T, v any) {
			want, _ := new(big.Int).SetString("18446744073709551616", 10)
			mustEq(t, v.(*big.Int).Cmp(want), 0)
		}},
		{"bigint_neg_two_100", func(t *testing.T, v any) { mustEq(t, v.(*big.Int).Sign(), -1) }},
		{"string_latin1", func(t *testing.T, v any) { mustEq(t, v.(string), "café ÿ\u0080") }},
		{"string_emoji", func(t *testing.T, v any) { mustEq(t, v.(string), "a\U0001F600\U0001F44D\U0001F3FDb") }},
		{"string_lone_high", func(t *testing.T, v any) { mustEq(t, v.(string), "\xed\xa0\x80") }},
		{"string_lone_low", func(t *testing.T, v any) { mustEq(t, v.(string), "a\xed\xb0\x80b") }},
		{"string_swapped_pair", func(t *testing.T, v any) { mustEq(t, v.(string), "\xed\xb8\x80\xed\xa0\xbd") }},
		{"date_recent", func(t *testing.T, v any) {
			mustEq(t, v.(time.Time).Equal(time.Date(2024, 2, 29, 12, 34, 56, 789_000_000, time.UTC)), true)
			mustEq(t, v.(time.Time).Location(), time.UTC)
		}},
		{"regexp_all_flags", func(t *testing.T, v any) { mustEq(t, *v.(*RegExp), RegExp{Source: "x(?<n>y)", Flags: "dgimsuy"}) }},
		{"regexp_v_flag", func(t *testing.T, v any) { mustEq(t, v.(*RegExp).Flags, "v") }},
		{"error_type", func(t *testing.T, v any) {
			e := v.(*Error)
			mustEq(t, e.Name+"|"+e.Message, "TypeError|bad type")
			mustEq(t, strings.HasPrefix(e.Stack, "TypeError: bad type"), true)
		}},
		{"wrapper_number", func(t *testing.T, v any) { mustEq(t, *v.(*Wrapper), Wrapper{Kind: "Number", Value: 1.5}) }},
		{"wrapper_true", func(t *testing.T, v any) { mustEq(t, *v.(*Wrapper), Wrapper{Kind: "Boolean", Value: true}) }},
		{"wrapper_string", func(t *testing.T, v any) { mustEq(t, *v.(*Wrapper), Wrapper{Kind: "String", Value: "héllo"}) }},
		{"arraybuffer_bytes", func(t *testing.T, v any) { mustEq(t, string(v.(Bytes)), "\x00\x01\x02\xfd\xfe\xff") }},
		{"array_named_property", func(t *testing.T, v any) {
			a := v.(*ArrayWithProps)
			mustEq(t, len(a.Items), 2)
			mustEq(t, strings.Join(a.Props.Keys, ","), "extra")
			mustEq(t, a.Props.Values[0].(string), "kept")
		}},
		{"array_empty_cycle_through_property", func(t *testing.T, v any) {
			a := v.(*ArrayWithProps)
			// The property points back at the array, which is the final wrapper.
			mustEq(t, a.Props.Values[0], any(a))
		}},
		{"date_invalid_nan", func(t *testing.T, v any) { _ = v.(InvalidDate) }},
		{"date_max_js_date", func(t *testing.T, v any) { mustEq(t, v.(time.Time).UnixMilli(), int64(8.64e15)) }},
		{"error_with_cause", func(t *testing.T, v any) {
			e := v.(*Error)
			mustEq(t, e.HasCause, true)
			mustEq(t, e.HasStack, true)
			mustEq(t, e.Cause.(*Object).Keys[0], "code")
		}},
		{"error_with_null_cause", func(t *testing.T, v any) {
			e := v.(*Error)
			mustEq(t, e.HasCause, true)
			mustEq(t, e.Cause == nil, true)
		}},
		{"error_no_stack", func(t *testing.T, v any) { mustEq(t, v.(*Error).HasStack, false) }},
		{"error_cause_cycle", func(t *testing.T, v any) {
			e := v.(*Error)
			mustEq(t, e.Cause == any(e), true)
		}},
		{"array_holes", func(t *testing.T, v any) {
			a := v.([]any)
			mustEq(t, len(a), 5)
			_ = a[1].(Hole)
			mustEq(t, a[2].(int64), int64(3))
		}},
		{"map_mixed_keys", func(t *testing.T, v any) {
			m := v.(*Map)
			mustEq(t, len(m.Entries), 5)
			mustEq(t, m.Entries[0][0].(string), "a")
			mustEq(t, m.Entries[1][0].(int64), int64(2))
		}},
		{"set_order", func(t *testing.T, v any) { mustEq(t, len(v.(*Set).Items), 5) }},
		{"object_integer_keys", func(t *testing.T, v any) {
			mustEq(t, strings.Join(v.(*Object).Keys, ","), "1,2,4294967294,b,-1,4294967295,01,1.5")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.vector, func(t *testing.T) {
			in, err := readFile(filepath.Join(vectorDir, tt.vector+".bin"))
			if err != nil {
				t.Fatal(err)
			}
			v, err := Deserialize(in)
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, v)
		})
	}
}

func mustEq[T comparable](t *testing.T, got, want T) {
	t.Helper()
	if got != want {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestBackReferencesShareIdentity(t *testing.T) {
	load := func(name string) any {
		in, err := readFile(filepath.Join(vectorDir, name+".bin"))
		if err != nil {
			t.Fatal(err)
		}
		v, err := Deserialize(in)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	o := load("ref_cycle_object").(*Object)
	if o.Values[1] != any(o) {
		t.Fatal("self reference is not the same *Object")
	}
	arr := load("ref_cycle_array").([]any)
	if &arr[0] != &arr[1].([]any)[0] {
		t.Fatal("array cycle does not share storage")
	}
	m := load("ref_cycle_map").(*Map)
	if m.Entries[0][1] != any(m) {
		t.Fatal("map cycle is not the same *Map")
	}
	s := load("ref_cycle_set").(*Set)
	if s.Items[0] != any(s) {
		t.Fatal("set cycle is not the same *Set")
	}
	same := load("ref_same_object_twice").([]any)
	if same[0] != same[1] {
		t.Fatal("repeated reference is not the same *Object")
	}
}

func TestDuplicateKeyKeepsFirstPosition(t *testing.T) {
	in, err := readFile(filepath.Join(vectorDir, "object_duplicate_key_last_wins.bin"))
	if err != nil {
		t.Fatal(err)
	}
	v, err := Deserialize(in)
	if err != nil {
		t.Fatal(err)
	}
	o := v.(*Object)
	mustEq(t, strings.Join(o.Keys, ","), "a,b")
	mustEq(t, o.Values[0].(int64), int64(3))
}

func TestDeserializeDoesNotAliasInput(t *testing.T) {
	in := []byte{0xff, 15, 'B', 2, 7, 8}
	v, err := Deserialize(in)
	if err != nil {
		t.Fatal(err)
	}
	in[4] = 0
	mustEq(t, string(v.(Bytes)), "\x07\x08")
}

func FuzzDeserialize(f *testing.F) {
	bins, err := filepath.Glob(filepath.Join(vectorDir, "*.bin"))
	if err != nil {
		f.Fatal(err)
	}
	for _, bin := range bins {
		if strings.Contains(bin, ".v16.") {
			continue
		}
		in, err := readFile(bin)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(in)
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		v, err := Deserialize(in)
		if err != nil {
			return
		}
		// Anything that decodes must also render (or fail cleanly) without panicking.
		_, _ = Canonical(v)
	})
}

// nodeGate decides whether TestGeneratorIsCurrent runs. By default a missing or
// wrong Node skips; with CRAWLKIT_REQUIRE_NODE set it fails, so the drift check cannot silently vanish.
func nodeGate(ci bool, lookErr error, major string) (skip, fail string) {
	var problem string
	switch {
	case lookErr != nil:
		problem = fmt.Sprintf("node unavailable: %v", lookErr)
	case major != "22":
		problem = fmt.Sprintf("vectors are generated with Node 22, found major %q", major)
	default:
		return "", ""
	}
	if ci {
		return "", problem + " (Node 22 is required because CRAWLKIT_REQUIRE_NODE is set)"
	}
	return problem, ""
}

// TestArrayWithPropsBackReferences checks that a back-reference to an array that
// carries named properties resolves to the final *ArrayWithProps, not the plain slice.
func TestArrayWithPropsBackReferences(t *testing.T) {
	prop := []byte{'"', 1, 'x', 'I', 2} // x = 1
	// a = []; a.x = 1; [a, a]
	inner := append(append([]byte{'A', 0}, prop...), '$', 1, 0)
	outer := append([]byte{0xff, 0x0f, 'A', 2}, inner...)
	outer = append(outer, '^', 1, '$', 0, 2)
	v, err := Deserialize(outer)
	if err != nil {
		t.Fatal(err)
	}
	pair := v.([]any)
	first, ok := pair[0].(*ArrayWithProps)
	if !ok {
		t.Fatalf("first element is %T, want *ArrayWithProps", pair[0])
	}
	if pair[1] != any(first) {
		t.Fatalf("back-reference is %T, want the same *ArrayWithProps", pair[1])
	}
	// a = [a]; a.x = 1 (dense, then sparse), the element is a reference to the array itself.
	for name, in := range map[string][]byte{
		"dense":  append(append([]byte{0xff, 0x0f, 'A', 1, '^', 0}, prop...), '$', 1, 1),
		"sparse": append(append([]byte{0xff, 0x0f, 'a', 1, 'I', 0, '^', 0}, prop...), '@', 2, 1),
	} {
		v, err := Deserialize(in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		a, ok := v.(*ArrayWithProps)
		if !ok {
			t.Fatalf("%s: got %T", name, v)
		}
		if a.Items[0] != any(a) {
			t.Fatalf("%s: self reference is %T, want the same *ArrayWithProps", name, a.Items[0])
		}
	}
}

// TestArrayWithPropsReachedThroughErrorCause covers a = []; e = new Error(”, {cause: a});
// a.x = e; serialize([a, e]): the error's cause must be the final wrapper.
func TestArrayWithPropsReachedThroughErrorCause(t *testing.T) {
	in := []byte{0xff, 0x0f, 'A', 2,
		'A', 0, '"', 1, 'x', 'r', 'c', '^', 1, '.', '$', 1, 0,
		'^', 2, '$', 0, 2}
	v, err := Deserialize(in)
	if err != nil {
		t.Fatal(err)
	}
	pair := v.([]any)
	a, ok := pair[0].(*ArrayWithProps)
	if !ok {
		t.Fatalf("pair[0] is %T, want *ArrayWithProps", pair[0])
	}
	e := pair[1].(*Error)
	if a.Props.Values[0] != any(e) {
		t.Fatal("property is not the shared error")
	}
	if e.Cause != any(a) {
		t.Fatalf("cause is %T, want the same *ArrayWithProps", e.Cause)
	}
}
