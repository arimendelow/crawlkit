package v8

import "unicode/utf8"

// readString reads a string-valued field (regexp source, error message, String
// wrapper): an ordinary value that must be a string.
func (d *decoder) readString() (string, error) {
	v, err := d.readObject()
	if err != nil {
		return "", err
	}
	s, ok := v.(string)
	if !ok {
		return "", d.errorf("expected a string, found %T", v)
	}
	return s, nil
}

func (d *decoder) readUTF8String() (string, error) {
	raw, err := d.readLengthPrefixed()
	if err != nil {
		return "", err
	}
	if utf8.Valid(raw) {
		return string(raw), nil
	}
	out := make([]byte, 0, len(raw)+8)
	for len(raw) > 0 {
		r, size := utf8.DecodeRune(raw)
		out = utf8.AppendRune(out, r) // invalid bytes become U+FFFD, one per byte
		raw = raw[size:]
	}
	return string(out), nil
}

// readOneByteString reads a Latin-1 string and returns it as UTF-8.
func (d *decoder) readOneByteString() (string, error) {
	raw, err := d.readLengthPrefixed()
	if err != nil {
		return "", err
	}
	out := make([]byte, 0, len(raw)+len(raw)/4)
	for _, c := range raw {
		out = utf8.AppendRune(out, rune(c))
	}
	return string(out), nil
}

// readTwoByteString reads a UTF-16LE string and returns it as UTF-8. Unpaired
// surrogates are encoded as three-byte WTF-8 sequences (ED A0 80 to ED BF BF)
// so the original code units can be recovered.
func (d *decoder) readTwoByteString() (string, error) {
	raw, err := d.readLengthPrefixed()
	if err != nil {
		return "", err
	}
	n := len(raw)
	if n%2 != 0 {
		return "", d.errorf("two-byte string has odd byte length %d", n)
	}
	out := make([]byte, 0, n*3/2)
	for i := 0; i < n; i += 2 {
		u := rune(raw[i]) | rune(raw[i+1])<<8
		switch {
		case u >= 0xD800 && u <= 0xDBFF && i+3 < n:
			lo := rune(raw[i+2]) | rune(raw[i+3])<<8
			if lo >= 0xDC00 && lo <= 0xDFFF {
				out = utf8.AppendRune(out, 0x10000+(u-0xD800)<<10+(lo-0xDC00))
				i += 2
				continue
			}
			out = appendSurrogate(out, u)
		case u >= 0xD800 && u <= 0xDFFF:
			out = appendSurrogate(out, u)
		default:
			out = utf8.AppendRune(out, u)
		}
	}
	return string(out), nil
}

func appendSurrogate(out []byte, u rune) []byte {
	return append(out, 0xE0|byte(u>>12), 0x80|byte(u>>6)&0x3f, 0x80|byte(u)&0x3f) //nolint:gosec // u is a surrogate (0xD800-0xDFFF), so each part fits a byte
}
