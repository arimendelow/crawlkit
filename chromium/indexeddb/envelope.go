package indexeddb

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/golang/snappy"
	"github.com/openclaw/crawlkit/chromium/v8"
)

const (
	blinkTag           = 0xff
	blinkPseudoVersion = 0x11 // kRequiresProcessingSSVPseudoVersion
	wrapBlob           = 0x01
	wrapSnappy         = 0x02
	trailerTag         = 0xfe
	trailerSize        = 13
	trailerMinVersion  = 21
	maxEnvelopeDepth   = 4
	envelopeHexBytes   = 16
	// maxSnappyRatio bounds a Snappy block's declared decoded length by its
	// compressed length. The densest Snappy encoding is a 3-byte copy of 64
	// bytes (about 21:1), so no valid block exceeds 32:1; a larger claim is a
	// forged length header, which snappy.Decode would otherwise allocate in full.
	maxSnappyRatio   = 32
	v21PayloadOffset = 15 // ff 15 fe + 8-byte offset + 4-byte size; cross-checks the trailer
)

// Omission codes: the complete list callers can see. v8_unknown_tag,
// v8_host_object and v8_shared pass through from chromium/v8 unchanged.
const (
	CodeEmptyValue      = "empty_value"
	CodeUnknownEnvelope = "unknown_envelope"
	CodeBlobMissing     = "blob_missing"
	CodeBadKey          = "bad_key"
	CodeV8Version       = "v8_version"
	CodeV8Malformed     = "v8_malformed"
	CodeSnappyTooLarge  = "snappy_too_large"
)

// Omission describes a value that could not be decoded.
type Omission struct{ Code, Detail string }

// OmissionError is the typed error Decode returns; callers count it by Code.
type OmissionError struct{ Omission }

func (e *OmissionError) Error() string { return "indexeddb: " + e.Code + ": " + e.Detail }

// unknown builds an unknown_envelope error whose detail ends with the first
// 16 bytes of the offending value in lowercase hex.
func unknown(raw []byte, detail string) error {
	n := min(len(raw), envelopeHexBytes)
	return &OmissionError{Omission{Code: CodeUnknownEnvelope, Detail: detail + "; envelope " + hex.EncodeToString(raw[:n])}}
}

// EnvelopeKind classifies a raw record value (Record.Raw: the stored value with
// its leading version varint already removed) as plain, snappy, blob, v21 or
// unknown. Empty input is unknown.
func EnvelopeKind(raw []byte) string {
	if len(raw) < 2 || raw[0] != blinkTag {
		return "unknown"
	}
	ver, n, err := readVarint(raw[1:])
	if err != nil {
		return "unknown"
	}
	rest := raw[1+n:]
	switch {
	case ver == blinkPseudoVersion:
		if len(rest) == 0 {
			return "unknown"
		}
		switch rest[0] {
		case wrapBlob:
			return "blob"
		case wrapSnappy:
			return "snappy"
		}
		return "unknown"
	case ver >= trailerMinVersion:
		if len(rest) > 0 && rest[0] == trailerTag {
			return "v21"
		}
		return "unknown"
	case ver >= 1:
		return "plain"
	}
	return "unknown"
}

// Decode unwraps the Blink envelope of a record value (blob replacement,
// snappy, v21 trailer) and deserializes the V8 payload. raw must be the Raw
// of a Record from Records (which resolves blob indexes to blob numbers);
// hand-built values are read with the blob number in place of the index.
// Failures are *OmissionError.
func (o *Origin) Decode(dbID int64, raw []byte) (any, error) {
	payload, err := o.Payload(dbID, raw)
	if err != nil {
		return nil, err
	}
	return o.DecodePayload(payload)
}

// DecodePayload deserializes a V8 payload that Payload returned. Decode is Payload followed by
// DecodePayload, so a caller that needs the payload as well (to digest it) unwraps the value once.
// Failures are *OmissionError.
func (o *Origin) DecodePayload(payload []byte) (any, error) {
	v, err := v8.Deserialize(payload)
	if err != nil {
		var ue *v8.UnsupportedError
		var ve *v8.VersionError
		switch {
		case errors.As(err, &ue):
			return nil, &OmissionError{Omission{Code: ue.Code, Detail: err.Error()}}
		case errors.As(err, &ve):
			return nil, &OmissionError{Omission{Code: CodeV8Version, Detail: err.Error()}}
		}
		return nil, &OmissionError{Omission{Code: CodeV8Malformed, Detail: err.Error()}}
	}
	return v, nil
}

// Payload unwraps the Blink envelope of a record value as Decode does and returns the V8 payload
// (starting at its own ff <version> header) without deserializing it, so another decoder can be
// compared against this one. The result may alias raw. Failures are *OmissionError.
func (o *Origin) Payload(dbID int64, raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return nil, &OmissionError{Omission{Code: CodeEmptyValue, Detail: "record has no value"}}
	}
	return o.unwrap(dbID, raw, 0)
}

// unwrap returns the V8 payload (starting at its own ff <version> header).
func (o *Origin) unwrap(dbID int64, raw []byte, depth int) ([]byte, error) {
	if depth > maxEnvelopeDepth {
		return nil, unknown(raw, "envelope nested too deeply")
	}
	if len(raw) < 2 || raw[0] != blinkTag {
		return nil, unknown(raw, "missing Blink version tag")
	}
	ver, n, err := readVarint(raw[1:])
	if err != nil {
		return nil, unknown(raw, "truncated Blink version")
	}
	pos := 1 + n
	rest := raw[pos:]
	switch {
	case ver == blinkPseudoVersion:
		if len(rest) == 0 {
			return nil, unknown(raw, "missing wrapper kind")
		}
		switch rest[0] {
		case wrapBlob:
			_, number, _, ok := parseBlobRef(raw)
			if !ok {
				return nil, unknown(raw, "malformed blob reference")
			}
			b, err := o.readBlob(dbID, number)
			if err != nil {
				return nil, err
			}
			return o.unwrap(dbID, b, depth+1)
		case wrapSnappy:
			n, err := snappy.DecodedLen(rest[1:])
			if err != nil {
				return nil, unknown(raw, "snappy: "+err.Error())
			}
			if uint64(n) > maxSnappyRatio*uint64(len(rest)-1) {
				return nil, &OmissionError{Omission{Code: CodeSnappyTooLarge, Detail: fmt.Sprintf("snappy block declares %d decoded bytes for %d compressed; envelope %s", n, len(rest)-1, hex.EncodeToString(raw[:min(len(raw), envelopeHexBytes)]))}}
			}
			dec, err := snappy.Decode(nil, rest[1:])
			if err != nil {
				return nil, unknown(raw, "snappy: "+err.Error())
			}
			return o.unwrap(dbID, dec, depth+1)
		}
		return nil, unknown(raw, "unrecognized wrapper kind")
	case ver >= trailerMinVersion:
		if len(rest) < trailerSize || rest[0] != trailerTag {
			return nil, unknown(raw, "missing trailer offset tag")
		}
		offset := binary.BigEndian.Uint64(rest[1:9])
		size := uint64(binary.BigEndian.Uint32(rest[9:13]))
		start := uint64(pos + trailerSize) //nolint:gosec // pos and trailerSize are small non-negative ints
		total := uint64(len(raw))
		// Wire version 21: the V8 payload
		// starts at the fixed offset 15. An empty trailer is offset 0, size 0 (all
		// 4714 v21 values in a real cache). Otherwise the trailer runs from offset
		// to the end of the value; any other combination is not guessed at.
		if start != v21PayloadOffset {
			return nil, unknown(raw, "trailer does not match the value")
		}
		if offset == 0 && size == 0 {
			// Blink leaves an empty trailer zeroed; the payload runs to the end.
			return raw[start:], nil
		}
		if offset < start || offset > total || size != total-offset {
			return nil, unknown(raw, "trailer does not match the value")
		}
		return raw[start:offset], nil
	case ver >= 1:
		if len(rest) > 0 && rest[0] == blinkTag {
			return rest, nil
		}
		return raw, nil
	}
	return nil, unknown(raw, "unsupported Blink version")
}
