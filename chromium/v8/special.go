package v8

import (
	"math"
	"time"
)

// Values with their own wire layouts: dates, primitive wrappers, regular
// expressions, errors, array buffers and the views over them.

func (d *decoder) readDate() (any, error) {
	ms, err := d.readDouble()
	if err != nil {
		return nil, err
	}
	if math.IsNaN(ms) {
		return d.register(InvalidDate{}, false), nil
	}
	if math.Abs(ms) > 8.64e15 {
		return nil, d.errorf("date value %v out of range", ms)
	}
	return d.register(time.UnixMilli(int64(ms)).UTC(), false), nil
}

func (d *decoder) readWrapper(tag byte) (any, error) {
	id := d.newID()
	w := &Wrapper{}
	switch tag {
	case tagTrueObject:
		w.Kind, w.Value = "Boolean", true
	case tagFalseObject:
		w.Kind, w.Value = "Boolean", false
	case tagNumberObject:
		f, err := d.readDouble()
		if err != nil {
			return nil, err
		}
		w.Kind, w.Value = "Number", f
	case tagBigIntObject:
		n, err := d.readBigInt()
		if err != nil {
			return nil, err
		}
		w.Kind, w.Value = "BigInt", n
	default:
		s, err := d.readString()
		if err != nil {
			return nil, err
		}
		w.Kind, w.Value = "String", s
	}
	d.setID(id, w, false)
	return w, nil
}

// regexpFlags maps V8's flag bits to letters in RegExp.prototype.flags order.
var regexpFlags = []struct {
	bit    uint32
	letter byte
}{
	{1 << 7, 'd'}, {1 << 0, 'g'}, {1 << 1, 'i'}, {1 << 2, 'm'}, {1 << 5, 's'}, {1 << 4, 'u'}, {1 << 8, 'v'}, {1 << 3, 'y'},
}

func (d *decoder) readRegExp() (any, error) {
	id := d.newID()
	source, err := d.readString()
	if err != nil {
		return nil, err
	}
	bits, err := d.readVarint32()
	if err != nil {
		return nil, err
	}
	var flags []byte
	for _, f := range regexpFlags {
		if bits&f.bit != 0 {
			flags = append(flags, f.letter)
			bits &^= f.bit
		}
	}
	if bits != 0 {
		return nil, d.errorf("unknown regexp flag bits 0x%x", bits)
	}
	re := &RegExp{Source: source, Flags: string(flags)}
	d.setID(id, re, false)
	return re, nil
}

var errorNames = map[byte]string{
	'E': "EvalError", 'R': "RangeError", 'F': "ReferenceError", 'S': "SyntaxError", 'T': "TypeError", 'U': "URIError",
}

func (d *decoder) readError() (any, error) {
	e := &Error{Name: "Error"}
	d.register(e, false)
	t, err := d.readByte()
	if err != nil {
		return nil, err
	}
	if name, ok := errorNames[t]; ok {
		e.Name = name
		if t, err = d.readByte(); err != nil {
			return nil, err
		}
	}
	for {
		switch t {
		case 'm':
			if e.Message, err = d.readString(); err != nil {
				return nil, err
			}
		case 's':
			if e.Stack, err = d.readString(); err != nil {
				return nil, err
			}
			e.HasStack = true
		case 'c':
			if e.Cause, err = d.readObject(); err != nil {
				return nil, err
			}
			e.HasCause = true
		case '.':
			return e, nil
		default:
			return nil, d.errorf("unexpected error sub-tag 0x%02x", t)
		}
		if t, err = d.readByte(); err != nil {
			return nil, err
		}
	}
}

func (d *decoder) readArrayBuffer(resizable bool) (any, bool, error) {
	n, err := d.readSize()
	if err != nil {
		return nil, false, err
	}
	if resizable {
		max, err := d.readVarint()
		if err != nil {
			return nil, false, err
		}
		if uint64(n) > max { //nolint:gosec // n is a non-negative length read from the input
			return nil, false, d.errorf("array buffer length %d above maximum %d", n, max)
		}
	}
	raw, err := d.readRaw(n)
	if err != nil {
		return nil, false, err
	}
	buf := Bytes(append([]byte{}, raw...))
	d.register(buf, true)
	return buf, true, nil
}

// viewElementSize is the element size for each ArrayBufferViewTag.
var viewElementSize = map[byte]uint64{
	'b': 1, 'B': 1, 'C': 1, 'w': 2, 'W': 2, 'h': 2, 'd': 4, 'D': 4, 'f': 4, 'F': 8, 'q': 8, 'Q': 8, '?': 1,
}

func (d *decoder) readView(buf Bytes) (any, error) {
	sub, err := d.readVarint()
	if err != nil {
		return nil, err
	}
	offset, err := d.readVarint()
	if err != nil {
		return nil, err
	}
	length, err := d.readVarint()
	if err != nil {
		return nil, err
	}
	if offset > uint64(len(buf)) || length > uint64(len(buf))-offset {
		return nil, d.errorf("view [%d, +%d) outside its %d byte buffer", offset, length, len(buf))
	}
	if d.version >= 14 || d.brokenV13 {
		if _, err := d.readVarint32(); err != nil { // flags: length-tracking and resizable-buffer bits
			return nil, err
		}
	}
	if sub > 0xff {
		return nil, d.errorf("unknown array buffer view sub-tag %d", sub)
	}
	size, ok := viewElementSize[byte(sub)] //nolint:gosec // range checked above
	if !ok {
		return nil, d.errorf("unknown array buffer view sub-tag %d", sub)
	}
	if offset%size != 0 || length%size != 0 {
		return nil, d.errorf("view offset %d or length %d not a multiple of element size %d", offset, length, size)
	}
	view := buf[offset : offset+length : offset+length]
	d.register(view, false)
	return view, nil
}
