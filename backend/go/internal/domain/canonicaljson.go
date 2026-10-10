package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
)

// Canonical JSON feeds profile_id and chunk_id and must serialize exactly like
// Python `json.dumps(obj, sort_keys=True, separators=(",", ":"))`, otherwise
// hashes diverge from already indexed data.
type canonicalKind uint8

const (
	kindString canonicalKind = iota + 1
	kindInt
	kindBool
)

type canonicalValue struct {
	kind canonicalKind
	str  string
	num  int64
	flag bool
}

func canonicalString(v string) canonicalValue { return canonicalValue{kind: kindString, str: v} }

func canonicalInt(v int) canonicalValue { return canonicalValue{kind: kindInt, num: int64(v)} }

func canonicalBool(v bool) canonicalValue { return canonicalValue{kind: kindBool, flag: v} }

type canonicalObject struct {
	fields map[string]canonicalValue
}

func emptyCanonicalObject() canonicalObject {
	return canonicalObject{fields: make(map[string]canonicalValue)}
}

func (o canonicalObject) withString(name, value string) canonicalObject {
	return o.with(name, canonicalString(value))
}

func (o canonicalObject) withInt(name string, value int) canonicalObject {
	return o.with(name, canonicalInt(value))
}

func (o canonicalObject) withBool(name string, value bool) canonicalObject {
	return o.with(name, canonicalBool(value))
}

func (o canonicalObject) with(name string, value canonicalValue) canonicalObject {
	next := make(map[string]canonicalValue, len(o.fields)+1)
	for k, v := range o.fields {
		next[k] = v
	}
	next[name] = value
	return canonicalObject{fields: next}
}

// encode sorts keys, drops spaces, and escapes non-ASCII as \uXXXX to match
// Python ensure_ascii=True (the main divergence from encoding/json).
func (o canonicalObject) encode() []byte {
	names := make([]string, 0, len(o.fields))
	for name := range o.fields {
		names = append(names, name)
	}
	slices.Sort(names)

	const avgFieldBytes = 64
	out := make([]byte, 0, avgFieldBytes*len(names))
	out = append(out, '{')
	for i, name := range names {
		if i > 0 {
			out = append(out, ',')
		}
		out = appendEscapedJSONString(out, name)
		out = append(out, ':')
		out = appendJSONValue(out, o.fields[name])
	}
	return append(out, '}')
}

func appendJSONValue(dst []byte, v canonicalValue) []byte {
	switch v.kind {
	case kindString:
		return appendEscapedJSONString(dst, v.str)
	case kindInt:
		return strconv.AppendInt(dst, v.num, 10)
	case kindBool:
		if v.flag {
			return append(dst, "true"...)
		}
		return append(dst, "false"...)
	default:
		return append(dst, `""`...)
	}
}

// appendEscapedJSONString mirrors CPython encode_basestring_ascii.
func appendEscapedJSONString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	for _, r := range s {
		switch r {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\f':
			dst = append(dst, '\\', 'f')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		default:
			switch {
			case r < 0x20:
				dst = appendUnicodeEscape(dst, r)
			case r < 0x7f:
				dst = append(dst, byte(r))
			case r > 0xffff:
				r -= 0x10000
				dst = appendUnicodeEscape(dst, 0xd800+(r>>10))
				dst = appendUnicodeEscape(dst, 0xdc00+(r&0x3ff))
			default:
				dst = appendUnicodeEscape(dst, r)
			}
		}
	}
	return append(dst, '"')
}

const canonicalHexDigits = "0123456789abcdef"

func appendUnicodeEscape(dst []byte, r rune) []byte {
	return append(dst, '\\', 'u',
		canonicalHexDigits[(r>>12)&0xf],
		canonicalHexDigits[(r>>8)&0xf],
		canonicalHexDigits[(r>>4)&0xf],
		canonicalHexDigits[r&0xf],
	)
}

func sha256Prefixed(payload []byte) string {
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}
