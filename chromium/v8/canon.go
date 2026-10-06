package v8

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// FormatES formats f as ECMAScript Number::toString does, for example "1e+21",
// "1e-7" and "0.000001". It prints "0" for both zeros.
func FormatES(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == 0:
		return "0"
	}
	sign := ""
	if f < 0 {
		sign, f = "-", -f
	}
	// Shortest round-trip digits: d.ddddde±XX
	e := strconv.FormatFloat(f, 'e', -1, 64)
	mant, expStr, _ := strings.Cut(e, "e")
	digits := strings.Replace(mant, ".", "", 1)
	exp, _ := strconv.Atoi(expStr)
	k, n := len(digits), exp+1
	switch {
	case k <= n && n <= 21:
		return sign + digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		return sign + digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		return sign + "0." + strings.Repeat("0", -n) + digits
	}
	exponent := n - 1
	esign := "+"
	if exponent < 0 {
		esign, exponent = "-", -exponent
	}
	if k == 1 {
		return sign + digits + "e" + esign + strconv.Itoa(exponent)
	}
	return sign + digits[:1] + "." + digits[1:] + "e" + esign + strconv.Itoa(exponent)
}

// Canonical renders a value produced by Deserialize as canonical JSON: no
// whitespace, keys in stored order, numbers as ECMAScript prints them, strings
// escaped exactly as JSON.stringify does, and values JSON cannot express
// written as single-key tagged objects. scripts/v8vectors/canon.mjs produces
// identical bytes for the same JS values.
//
// A reference back to an ancestor that is still being written (a cycle) is
// {"$cycle":N} where N is the ancestor's depth, the root being 0. Shared
// acyclic references are written in full each time.
func Canonical(v any) ([]byte, error) {
	c := &canonWriter{}
	if err := c.value(v); err != nil {
		return nil, err
	}
	return c.buf.Bytes(), nil
}

type canonWriter struct {
	buf   bytes.Buffer
	stack []any // identities of the containers being written
}

func (c *canonWriter) value(v any) error {
	switch x := v.(type) {
	case nil:
		c.buf.WriteString("null")
	case bool:
		c.buf.WriteString(strconv.FormatBool(x))
	case int64:
		c.buf.WriteString(strconv.FormatInt(x, 10))
	case float64:
		c.number(x)
	case string:
		c.str(x)
	case Undefined:
		c.buf.WriteString(`{"$undefined":true}`)
	case Hole:
		c.buf.WriteString(`{"$hole":true}`)
	case *big.Int:
		c.buf.WriteString(`{"$bigint":"` + x.String() + `"}`)
	case time.Time:
		return c.date(x)
	case Bytes:
		c.buf.WriteString(`{"$bytes":"` + base64.StdEncoding.EncodeToString(x) + `"}`)
	case *RegExp:
		c.buf.WriteString(`{"$regexp":[`)
		c.str(x.Source)
		c.buf.WriteByte(',')
		c.str(x.Flags)
		c.buf.WriteString(`]}`)
	case InvalidDate:
		c.buf.WriteString(`{"$date":null}`)
	case *Error:
		return c.container(v, func() error { return c.errorValue(x) })
	case *ArrayWithProps:
		return c.container(v, func() error { return c.arrayWithProps(x) })
	case *Wrapper:
		return c.wrapper(x)
	case *Object:
		return c.container(v, func() error { return c.object(x) })
	case []any:
		return c.container(v, func() error { return c.array(x) })
	case *Map:
		return c.container(v, func() error { return c.mapValue(x) })
	case *Set:
		return c.container(v, func() error { return c.set(x) })
	default:
		return fmt.Errorf("v8: cannot canonicalize %T", v)
	}
	return nil
}

func (c *canonWriter) number(f float64) {
	switch {
	case math.IsNaN(f), math.IsInf(f, 0):
		c.buf.WriteString(`{"$number":"` + FormatES(f) + `"}`)
	case f == 0 && math.Signbit(f):
		c.buf.WriteString(`{"$number":"-0"}`)
	default:
		c.buf.WriteString(FormatES(f))
	}
}

func (c *canonWriter) date(t time.Time) error {
	t = t.UTC()
	c.buf.WriteString(`{"$date":"`)
	if y := t.Year(); y >= 0 && y <= 9999 {
		c.buf.WriteString(t.Format(time.RFC3339Nano))
	} else {
		// JS toISOString writes years outside 0000-9999 as a sign and six digits.
		sign := byte('+')
		if y < 0 {
			sign, y = '-', -y
		}
		c.buf.WriteByte(sign)
		fmt.Fprintf(&c.buf, "%06d", y)
		c.buf.WriteString(t.Format("-01-02T15:04:05.999999999Z"))
	}
	c.buf.WriteString(`"}`)
	return nil
}

func (c *canonWriter) errorValue(e *Error) error {
	c.buf.WriteString(`{"$error":{"name":`)
	c.str(e.Name)
	c.buf.WriteString(`,"message":`)
	c.str(e.Message)
	if e.HasStack {
		c.buf.WriteString(`,"stack":`)
		c.str(e.Stack)
	}
	if e.HasCause {
		c.buf.WriteString(`,"cause":`)
		if err := c.value(e.Cause); err != nil {
			return err
		}
	}
	c.buf.WriteString(`}}`)
	return nil
}

func (c *canonWriter) arrayWithProps(a *ArrayWithProps) error {
	c.buf.WriteString(`{"$array":`)
	if err := c.array(a.Items); err != nil {
		return err
	}
	c.buf.WriteString(`,"$props":`)
	if a.Props == nil {
		c.buf.WriteString(`{}`)
	} else if err := c.object(a.Props); err != nil {
		return err
	}
	c.buf.WriteByte('}')
	return nil
}

func (c *canonWriter) wrapper(w *Wrapper) error {
	c.buf.WriteString(`{"$wrapper":[`)
	c.str(w.Kind)
	c.buf.WriteByte(',')
	switch w.Value.(type) {
	case bool, float64, *big.Int, string:
	default:
		return fmt.Errorf("v8: cannot canonicalize wrapper value %T", w.Value)
	}
	_ = c.value(w.Value) // cannot fail: the switch above admitted only scalar kinds
	c.buf.WriteString(`]}`)
	return nil
}

// identity returns a comparable identity for containers, which may be cyclic.
// Arrays are identified by their storage, so a reference to the []any inside an
// *ArrayWithProps matches the wrapper.
func identity(v any) any {
	switch a := v.(type) {
	case []any:
		return storage(a)
	case *ArrayWithProps:
		return storage(a.Items)
	}
	return v
}

// storage identifies a slice by its first backing element; a slice without
// capacity has none, so it cannot take part in a cycle.
func storage(a []any) any {
	if cap(a) == 0 {
		return nil
	}
	return &a[:1][0]
}

func (c *canonWriter) container(v any, write func() error) error {
	id := identity(v)
	if id != nil {
		for depth, anc := range c.stack {
			if anc == id {
				c.buf.WriteString(`{"$cycle":` + strconv.Itoa(depth) + `}`)
				return nil
			}
		}
	}
	c.stack = append(c.stack, id)
	err := write()
	c.stack = c.stack[:len(c.stack)-1]
	return err
}

func (c *canonWriter) object(o *Object) error {
	if len(o.Keys) != len(o.Values) {
		return fmt.Errorf("v8: object has %d keys and %d values", len(o.Keys), len(o.Values))
	}
	c.buf.WriteByte('{')
	for i, k := range o.Keys {
		if i > 0 {
			c.buf.WriteByte(',')
		}
		c.str(k)
		c.buf.WriteByte(':')
		if err := c.value(o.Values[i]); err != nil {
			return err
		}
	}
	c.buf.WriteByte('}')
	return nil
}

func (c *canonWriter) array(a []any) error {
	c.buf.WriteByte('[')
	for i, e := range a {
		if i > 0 {
			c.buf.WriteByte(',')
		}
		if err := c.value(e); err != nil {
			return err
		}
	}
	c.buf.WriteByte(']')
	return nil
}

func (c *canonWriter) mapValue(m *Map) error {
	c.buf.WriteString(`{"$map":[`)
	for i, e := range m.Entries {
		if i > 0 {
			c.buf.WriteByte(',')
		}
		c.buf.WriteByte('[')
		if err := c.value(e[0]); err != nil {
			return err
		}
		c.buf.WriteByte(',')
		if err := c.value(e[1]); err != nil {
			return err
		}
		c.buf.WriteByte(']')
	}
	c.buf.WriteString(`]}`)
	return nil
}

func (c *canonWriter) set(s *Set) error {
	c.buf.WriteString(`{"$set":`)
	if err := c.array(s.Items); err != nil {
		return err
	}
	c.buf.WriteByte('}')
	return nil
}

// str writes s as a JSON string the way JSON.stringify does. Strings are UTF-8
// with unpaired surrogates as WTF-8; those come out as lowercase \udxxx.
func (c *canonWriter) str(s string) {
	const hex = "0123456789abcdef"
	c.buf.WriteByte('"')
	for i := 0; i < len(s); {
		b := s[i]
		switch {
		case b == '"':
			c.buf.WriteString(`\"`)
		case b == '\\':
			c.buf.WriteString(`\\`)
		case b == '\b':
			c.buf.WriteString(`\b`)
		case b == '\f':
			c.buf.WriteString(`\f`)
		case b == '\n':
			c.buf.WriteString(`\n`)
		case b == '\r':
			c.buf.WriteString(`\r`)
		case b == '\t':
			c.buf.WriteString(`\t`)
		case b < 0x20:
			c.buf.WriteString(`\u00`)
			c.buf.WriteByte(hex[b>>4])
			c.buf.WriteByte(hex[b&0xf])
		case b < 0x80:
			c.buf.WriteByte(b)
		case b == 0xED && i+2 < len(s) && s[i+1] >= 0xA0 && s[i+1] <= 0xBF && s[i+2]&0xC0 == 0x80:
			u := 0xD000 | int(s[i+1]&0x3f)<<6 | int(s[i+2]&0x3f)
			c.buf.WriteString(`\u`)
			c.buf.WriteString(strconv.FormatInt(int64(u), 16))
			i += 3
			continue
		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError && size == 1 {
				c.buf.WriteString("�")
			} else {
				c.buf.WriteString(s[i : i+size])
			}
			i += size
			continue
		}
		i++
	}
	c.buf.WriteByte('"')
}
