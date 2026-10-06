// Package v8 decodes V8's structured-clone wire format (the bytes produced by
// v8.serialize in Node and by Blink for IndexedDB values) into plain Go values,
// and renders those values as canonical JSON for golden files.
//
// This code originates in https://github.com/ourostack/teamscrawl (internal/v8, MIT licensed) and is
// provider neutral: it knows nothing about any one application's databases.
package v8

import "fmt"

// Object is a plain JS object. Keys are in JS property order: integer-like
// keys ascending first, then the remaining keys in insertion order. Numeric
// keys are stored as their decimal string form.
type Object struct {
	Keys   []string
	Values []any
}

// Hole marks an absent element in an array.
type Hole struct{}

// Undefined is the JS undefined value. JS null is a Go nil.
type Undefined struct{}

// Map is a JS Map with entries in insertion order.
type Map struct {
	Entries [][2]any
}

// Set is a JS Set with items in insertion order.
type Set struct {
	Items []any
}

// Bytes holds an ArrayBuffer's contents, or the window of an ArrayBuffer that
// a typed array or DataView covers.
type Bytes []byte

// RegExp is a JS regular expression. Flags are in the order of RegExp.prototype.flags.
type RegExp struct {
	Source string
	Flags  string
}

// Error is a JS error object. HasStack and HasCause say whether the source
// carried a stack string or a cause; Cause may itself be nil (JS null).
type Error struct {
	Name     string
	Message  string
	Stack    string
	HasStack bool
	Cause    any
	HasCause bool
}

// InvalidDate is a JS Date whose time value is NaN.
type InvalidDate struct{}

// ArrayWithProps is an array that also carries named (non-index) properties.
// Arrays without them decode to a plain []any.
type ArrayWithProps struct {
	Items []any
	Props *Object
}

// VersionError reports a wire format version outside the supported range.
type VersionError struct{ Version uint64 }

func (e *VersionError) Error() string {
	return fmt.Sprintf("v8: unsupported wire format version %d (supported: 13 to 16)", e.Version)
}

// Wrapper is a primitive wrapper object. Kind is "Boolean", "Number",
// "BigInt" or "String"; Value is a bool, float64, *big.Int or string.
type Wrapper struct {
	Kind  string
	Value any
}

// UnsupportedError reports a construct that needs state outside the byte
// stream (host objects, shared values, transfers) or an unrecognized tag.
type UnsupportedError struct {
	Code   string // v8_unknown_tag, v8_host_object or v8_shared
	Tag    byte
	Offset int // offset of the tag byte in the input
}

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf("v8: %s: tag 0x%02x at offset %d", e.Code, e.Tag, e.Offset)
}
