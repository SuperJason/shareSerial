package main

import (
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// codec converts between the device charset and UTF-8 for display, buffering
// a partial multi-byte sequence until the next chunk arrives (an embedded
// device happily splits one GBK character across two reads).
type codec struct {
	name string
	dec  *encoding.Decoder
	enc  *encoding.Encoder
	left []byte
}

func newCodec(name string) *codec {
	c := &codec{name: strings.ToLower(strings.TrimSpace(name))}
	var e encoding.Encoding
	switch c.name {
	case "", "utf-8", "utf8", "raw", "latin1", "iso-8859-1":
		// Passthrough: UTF-8 needs no transform, and "raw"/"latin1" means the
		// user wants the bytes exactly as sent.
		return c
	case "gbk", "gb2312":
		e = simplifiedchinese.GBK
	case "gb18030":
		e = simplifiedchinese.GB18030
	case "big5":
		e = traditionalchinese.Big5
	case "shift-jis", "sjis":
		e = japanese.ShiftJIS
	case "euc-jp":
		e = japanese.EUCJP
	case "euc-kr":
		e = korean.EUCKR
	case "utf-16le":
		e = unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM)
	case "utf-16be":
		e = unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM)
	default:
		return c // Validate() rejects unknown names; keep passthrough anyway
	}
	c.dec = e.NewDecoder()
	c.enc = e.NewEncoder()
	return c
}

// Decode converts a raw chunk to UTF-8. An incomplete trailing sequence stays
// buffered and is prepended to the next call.
func (c *codec) Decode(b []byte) []byte {
	if c.dec == nil || len(b) == 0 {
		return b
	}
	src := b
	if len(c.left) > 0 {
		src = append(c.left, b...)
		c.left = nil
	}
	out, n, err := transform.Bytes(c.dec, src)
	if err == transform.ErrShortSrc && n < len(src) {
		c.left = append(c.left[:0], src[n:]...)
	}
	return out
}

// Encode converts UTF-8 input to the device charset before writing.
func (c *codec) Encode(b []byte) []byte {
	if c.enc == nil || len(b) == 0 {
		return b
	}
	c.enc.Reset()
	out, _, err := transform.Bytes(c.enc, b)
	if err != nil {
		return b // unmappable input goes out unchanged rather than dropped
	}
	return out
}

func (c *codec) reset() { c.left = c.left[:0] }

// displayText renders raw bytes as a single safe display line: the trailing
// terminator is dropped, tabs are kept, other control bytes become visible
// escapes so a log row never contains raw ANSI or NUL that would corrupt the
// view. Returns the rendered text and whether the payload is binary (in which
// case the caller also wants the hex rendering).
func displayText(b []byte) (string, bool) {
	b = trimTerminator(b)
	if len(b) == 0 {
		return "", false
	}
	binary := !utf8.Valid(b) || mostlyNonPrintable(b)
	if binary {
		// Keep a short escaped preview so the row is still readable.
		var sb strings.Builder
		n := len(b)
		if n > 256 {
			n = 256
		}
		for _, c := range b[:n] {
			switch {
			case c == '\t':
				sb.WriteByte('\t')
			case c >= 0x20 && c < 0x7f:
				sb.WriteByte(c)
			default:
				sb.WriteString("\\x")
				const hexdig = "0123456789abcdef"
				sb.WriteByte(hexdig[c>>4])
				sb.WriteByte(hexdig[c&0x0f])
			}
		}
		if len(b) > n {
			sb.WriteString("...")
		}
		return sb.String(), true
	}
	var sb strings.Builder
	sb.Grow(len(b))
	for _, c := range b {
		switch {
		case c == '\t':
			sb.WriteByte('\t')
		case c == 0x1b:
			sb.WriteString("\\e")
		case c < 0x20 || c == 0x7f:
			sb.WriteString("\\x")
			const hexdig = "0123456789abcdef"
			sb.WriteByte(hexdig[c>>4])
			sb.WriteByte(hexdig[c&0x0f])
		default:
			sb.WriteByte(c)
		}
	}
	return sb.String(), false
}

func trimTerminator(b []byte) []byte {
	for len(b) > 0 {
		last := b[len(b)-1]
		if last == '\n' || last == '\r' {
			b = b[:len(b)-1]
			continue
		}
		break
	}
	return b
}

// mostlyNonPrintable reports whether the payload looks like binary rather than
// text. A lone escape byte in a short line is still text (it is rendered as
// \e), so binary needs a NUL or a real density of control bytes.
func mostlyNonPrintable(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	bad := 0
	for _, c := range b {
		if c == 0 {
			return true
		}
		if (c < 0x09) || (c > 0x0d && c < 0x20) || c == 0x7f {
			bad++
		}
	}
	return bad >= 2 && bad*10 > len(b)
}

const hexUpper = "0123456789ABCDEF"

func hexString(b []byte) string {
	out := make([]byte, 0, len(b)*3)
	for i, c := range b {
		if i > 0 && i%16 == 0 {
			out = append(out, '\n')
		} else if i > 0 {
			out = append(out, ' ')
		}
		out = append(out, hexUpper[c>>4], hexUpper[c&0x0f])
	}
	return string(out)
}
