package v8

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/big"
)

const (
	// Wire versions 13 to 16 are supported: Chromium has written 13 or later since
	// 2016, so older versions (with UTF-8 regexp strings, undefined-as-hole) cannot
	// appear in an app cache.
	minVersion    = 13
	latestVersion = 16
	// maxDepth bounds recursion on hostile input; V8 bounds it by stack size.
	maxDepth = 1000
	// maxSparseLength bounds the slice allocated for a sparse array, whose
	// length is not backed by bytes in the input.
	maxSparseLength = 1 << 20
)

// Wire tags. See V8's src/objects/value-serializer.cc.
const (
	tagVersion         = 0xff
	tagPadding         = 0
	tagVerifyCount     = '?'
	tagTheHole         = '-'
	tagUndefined       = '_'
	tagNull            = '0'
	tagTrue            = 'T'
	tagFalse           = 'F'
	tagInt32           = 'I'
	tagUint32          = 'U'
	tagDouble          = 'N'
	tagBigInt          = 'Z'
	tagUTF8String      = 'S'
	tagOneByteString   = '"'
	tagTwoByteString   = 'c'
	tagObjectRef       = '^'
	tagBeginObject     = 'o'
	tagEndObject       = '{'
	tagBeginSparse     = 'a'
	tagEndSparse       = '@'
	tagBeginDense      = 'A'
	tagEndDense        = '$'
	tagDate            = 'D'
	tagTrueObject      = 'y'
	tagFalseObject     = 'x'
	tagNumberObject    = 'n'
	tagBigIntObject    = 'z'
	tagStringObject    = 's'
	tagRegExp          = 'R'
	tagBeginMap        = ';'
	tagEndMap          = ':'
	tagBeginSet        = '\''
	tagEndSet          = ','
	tagArrayBuffer     = 'B'
	tagImmutableBuffer = 'C'
	tagSharedImmutable = 'E'
	tagResizableBuffer = '~'
	tagBufferTransfer  = 't'
	tagBufferView      = 'V'
	tagSharedBuffer    = 'u'
	tagSharedObject    = 'p'
	tagWasmModule      = 'w'
	tagWasmMemory      = 'm'
	tagHostObject      = '\\'
	tagError           = 'r'
)

const (
	codeUnknownTag = "v8_unknown_tag"
	codeHostObject = "v8_host_object"
	codeShared     = "v8_shared"
)

// unset marks an object id that has been allocated but not yet given a value.
type unset struct{}

type decoder struct {
	b       []byte
	pos     int
	version uint32
	depth   int

	refs   []any  // object id -> value
	refBuf []bool // object id -> is a whole ArrayBuffer (eligible to be followed by a view)

	// brokenV13 reads array buffer view flags in version 13 data, which some
	// Chromium builds wrote by mistake (crbug.com/1284506).
	brokenV13 bool
}

// Deserialize decodes one V8 structured-clone value from b, which must start
// with the 0xFF version header. The result uses the types in values.go plus
// string, float64, int64, *big.Int, bool, nil and time.Time. Input that needs
// out-of-band state (host objects, shared values) yields an *UnsupportedError.
func Deserialize(b []byte) (any, error) {
	d := &decoder{b: b}
	if err := d.readHeader(); err != nil {
		return nil, err
	}
	start := d.pos
	v, err := d.readTop()
	var ue *UnsupportedError
	if err != nil && d.version == 13 && !errors.As(err, &ue) {
		d.pos, d.refs, d.refBuf, d.depth, d.brokenV13 = start, nil, nil, 0, true
		if v2, err2 := d.readTop(); err2 == nil {
			return v2, nil
		}
	}
	return v, err
}

func (d *decoder) readTop() (any, error) {
	v, err := d.readObject()
	if err != nil {
		return nil, err
	}
	d.skipPadding()
	if d.pos != len(d.b) {
		return nil, d.errorf("trailing data after value")
	}
	return v, nil
}

func (d *decoder) errorf(format string, args ...any) error {
	return fmt.Errorf("v8: %s at offset %d", fmt.Sprintf(format, args...), d.pos)
}

func (d *decoder) truncated() error { return d.errorf("unexpected end of data") }

func (d *decoder) unsupported(code string, tag byte, offset int) error {
	return &UnsupportedError{Code: code, Tag: tag, Offset: offset}
}

func (d *decoder) readHeader() error {
	if len(d.b) == 0 || d.b[0] != tagVersion {
		return errors.New("v8: missing version header")
	}
	d.pos = 1
	v, err := d.readVarint()
	if err != nil {
		return err
	}
	if v < minVersion || v > latestVersion {
		return &VersionError{Version: v}
	}
	d.version = uint32(v)
	return nil
}

// --- primitives ---------------------------------------------------------------

func (d *decoder) readByte() (byte, error) {
	if d.pos >= len(d.b) {
		return 0, d.truncated()
	}
	c := d.b[d.pos]
	d.pos++
	return c, nil
}

// readVarint reads a base-128 little-endian varint of up to 64 bits.
func (d *decoder) readVarint() (uint64, error) {
	var v uint64
	for i := 0; i < 10; i++ {
		c, err := d.readByte()
		if err != nil {
			return 0, err
		}
		v |= uint64(c&0x7f) << (7 * uint(i))
		if c&0x80 == 0 {
			return v, nil
		}
	}
	return 0, d.errorf("varint longer than 10 bytes")
}

// readVarint32 reads a uint32 the way V8 does: extra high bits are dropped.
func (d *decoder) readVarint32() (uint32, error) {
	v, err := d.readVarint()
	return uint32(v), err //nolint:gosec // V8 keeps the low 32 bits of an over-long varint
}

// readSize reads a varint length that must fit in the bytes that remain.
func (d *decoder) readSize() (int, error) {
	v, err := d.readVarint()
	if err != nil {
		return 0, err
	}
	if v > uint64(len(d.b)-d.pos) { //nolint:gosec // the remaining length is never negative
		return 0, d.errorf("length %d exceeds remaining data", v)
	}
	return int(v), nil //nolint:gosec // bounded by the remaining input length above
}

// readLengthPrefixed reads a varint length and then that many bytes, which alias the input.
func (d *decoder) readLengthPrefixed() ([]byte, error) {
	n, err := d.readSize() // already checked against the remaining input
	if err != nil {
		return nil, err
	}
	out := d.b[d.pos : d.pos+n]
	d.pos += n
	return out, nil
}

func (d *decoder) readRaw(n int) ([]byte, error) {
	if n < 0 || n > len(d.b)-d.pos {
		return nil, d.truncated()
	}
	out := d.b[d.pos : d.pos+n]
	d.pos += n
	return out, nil
}

func (d *decoder) readDouble() (float64, error) {
	raw, err := d.readRaw(8)
	if err != nil {
		return 0, err
	}
	return math.Float64frombits(binary.LittleEndian.Uint64(raw)), nil
}

func (d *decoder) readBigInt() (*big.Int, error) {
	bitfield, err := d.readVarint32()
	if err != nil {
		return nil, err
	}
	n := int(bitfield >> 1)
	raw, err := d.readRaw(n)
	if err != nil {
		return nil, err
	}
	be := make([]byte, n)
	for i, c := range raw {
		be[n-1-i] = c
	}
	v := new(big.Int).SetBytes(be)
	if bitfield&1 != 0 {
		v.Neg(v)
	}
	return v, nil
}

func (d *decoder) skipPadding() {
	for d.pos < len(d.b) && d.b[d.pos] == tagPadding {
		d.pos++
	}
}

// readTag returns the next tag, skipping padding, and the offset of the tag byte.
func (d *decoder) readTag() (byte, int, error) {
	d.skipPadding()
	if d.pos >= len(d.b) {
		return 0, 0, d.truncated()
	}
	off := d.pos
	d.pos++
	return d.b[off], off, nil
}

// peekTag returns the next non-padding tag without consuming it.
func (d *decoder) peekTag() (byte, bool) {
	d.skipPadding()
	if d.pos >= len(d.b) {
		return 0, false
	}
	return d.b[d.pos], true
}

// --- object ids -----------------------------------------------------------------

func (d *decoder) newID() int {
	d.refs = append(d.refs, unset{})
	d.refBuf = append(d.refBuf, false)
	return len(d.refs) - 1
}

func (d *decoder) setID(id int, v any, isBuffer bool) {
	d.refs[id] = v
	d.refBuf[id] = isBuffer
}

// register allocates the next id for v and returns v, for values that are
// complete when created.
func (d *decoder) register(v any, isBuffer bool) any {
	d.setID(d.newID(), v, isBuffer)
	return v
}

// --- values ------------------------------------------------------------------------

// readObject reads one value, then folds in an array buffer view if one follows a buffer.
func (d *decoder) readObject() (any, error) {
	d.depth++
	defer func() { d.depth-- }()
	if d.depth > maxDepth {
		return nil, d.errorf("nesting deeper than %d", maxDepth)
	}
	v, isBuffer, err := d.readInternal()
	if err != nil {
		return nil, err
	}
	if isBuffer {
		if t, ok := d.peekTag(); ok && t == tagBufferView {
			d.pos++
			return d.readView(v.(Bytes))
		}
	}
	return v, nil
}

func (d *decoder) readInternal() (v any, isBuffer bool, err error) {
	tag, off, err := d.readTag()
	if err != nil {
		return nil, false, err
	}
	switch tag {
	case tagVerifyCount:
		if _, err := d.readVarint32(); err != nil {
			return nil, false, err
		}
		v, err := d.readObject()
		return v, false, err
	case tagUndefined:
		return Undefined{}, false, nil
	case tagNull:
		return nil, false, nil
	case tagTrue:
		return true, false, nil
	case tagFalse:
		return false, false, nil
	case tagInt32:
		u, err := d.readVarint32()
		return int64(int32(u>>1) ^ -int32(u&1)), false, err
	case tagUint32:
		u, err := d.readVarint32()
		return int64(u), false, err
	case tagDouble:
		f, err := d.readDouble()
		return f, false, err
	case tagBigInt:
		n, err := d.readBigInt()
		return n, false, err
	case tagUTF8String:
		s, err := d.readUTF8String()
		return s, false, err
	case tagOneByteString:
		s, err := d.readOneByteString()
		return s, false, err
	case tagTwoByteString:
		s, err := d.readTwoByteString()
		return s, false, err
	case tagObjectRef:
		return d.readReference()
	case tagBeginObject:
		v, err := d.readPlainObject()
		return v, false, err
	case tagBeginSparse:
		v, err := d.readSparseArray()
		return v, false, err
	case tagBeginDense:
		v, err := d.readDenseArray()
		return v, false, err
	case tagDate:
		v, err := d.readDate()
		return v, false, err
	case tagTrueObject, tagFalseObject, tagNumberObject, tagBigIntObject, tagStringObject:
		v, err := d.readWrapper(tag)
		return v, false, err
	case tagRegExp:
		v, err := d.readRegExp()
		return v, false, err
	case tagBeginMap:
		v, err := d.readMap()
		return v, false, err
	case tagBeginSet:
		v, err := d.readSet()
		return v, false, err
	case tagArrayBuffer, tagImmutableBuffer:
		return d.readArrayBuffer(false)
	case tagResizableBuffer:
		return d.readArrayBuffer(true)
	case tagError:
		v, err := d.readError()
		return v, false, err
	case tagSharedBuffer, tagSharedImmutable:
		return nil, false, d.unsupported(codeShared, tag, off)
	case tagSharedObject:
		if d.version >= 15 {
			return nil, false, d.unsupported(codeShared, tag, off)
		}
	case tagBufferTransfer, tagWasmModule, tagWasmMemory, tagHostObject:
		return nil, false, d.unsupported(codeHostObject, tag, off)
	}
	return nil, false, d.unsupported(codeUnknownTag, tag, off)
}

func (d *decoder) readReference() (any, bool, error) {
	id, err := d.readVarint32()
	if err != nil {
		return nil, false, err
	}
	if int64(id) >= int64(len(d.refs)) {
		return nil, false, d.errorf("reference to unknown object %d", id)
	}
	v := d.refs[id]
	if _, pending := v.(unset); pending {
		return nil, false, d.errorf("reference to object %d before it is complete", id)
	}
	return v, d.refBuf[id], nil
}
