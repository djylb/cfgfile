package cfgfile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/djylb/cfgfile/jwcc"
)

// textEncoding is the encoding of a file on disk. UTF-8 files, with or
// without a byte order mark, are handled by jwcc as they are; UTF-16 files
// with a byte order mark are transcoded to UTF-8 for parsing and back to
// UTF-16 (with the same byte order mark) when written.
type textEncoding int

const (
	encUTF8 textEncoding = iota
	encUTF16LE
	encUTF16BE
)

// decodeText returns the UTF-8 text of a file and its encoding. Text that
// is not valid UTF-8 is left for the parser, which reports the position.
func decodeText(raw []byte) ([]byte, textEncoding, error) {
	var enc textEncoding
	switch {
	case bytes.HasPrefix(raw, []byte{0xFF, 0xFE}):
		enc = encUTF16LE
	case bytes.HasPrefix(raw, []byte{0xFE, 0xFF}):
		enc = encUTF16BE
	default:
		if len(raw) >= 2 && (raw[0] == 0 || raw[1] == 0) {
			return nil, 0, fmt.Errorf("%w: the file looks like UTF-16 without a byte order mark; save it as UTF-8", ErrEncoding)
		}
		return raw, encUTF8, nil
	}
	body := raw[2:]
	if len(body)%2 != 0 {
		return nil, 0, fmt.Errorf("%w: UTF-16 file has an odd number of bytes", ErrEncoding)
	}
	out := make([]byte, 0, len(body))
	for i := 0; i < len(body); i += 2 {
		u := unit(body[i:], enc)
		r := rune(u)
		if utf16.IsSurrogate(r) {
			if u >= 0xDC00 || i+3 >= len(body) {
				return nil, 0, fmt.Errorf("%w: invalid UTF-16 at byte %d", ErrEncoding, i+2)
			}
			r = utf16.DecodeRune(r, rune(unit(body[i+2:], enc)))
			if r == utf8.RuneError {
				return nil, 0, fmt.Errorf("%w: invalid UTF-16 at byte %d", ErrEncoding, i+2)
			}
			i += 2
		}
		out = utf8.AppendRune(out, r)
	}
	return out, enc, nil
}

func unit(b []byte, enc textEncoding) uint16 {
	if enc == encUTF16LE {
		return uint16(b[0]) | uint16(b[1])<<8
	}
	return uint16(b[0])<<8 | uint16(b[1])
}

// encodeText converts UTF-8 text back to the file's encoding.
func encodeText(text []byte, enc textEncoding) []byte {
	if enc == encUTF8 {
		return text
	}
	out := make([]byte, 0, 2+2*len(text))
	put := func(u uint16) {
		if enc == encUTF16LE {
			out = append(out, byte(u), byte(u>>8))
		} else {
			out = append(out, byte(u>>8), byte(u))
		}
	}
	put(0xFEFF)
	for _, r := range string(text) {
		if r1, r2 := utf16.EncodeRune(r); r1 != utf8.RuneError {
			put(uint16(r1))
			put(uint16(r2))
		} else {
			put(uint16(r))
		}
	}
	return out
}

// Format formats the content of a configuration file with jwcc.Format,
// keeping its encoding (UTF-8 with or without a byte order mark, or UTF-16
// with one) and line endings.
func Format(raw []byte, o *jwcc.FormatOptions) ([]byte, error) {
	text, enc, err := decodeText(raw)
	if err != nil {
		return nil, err
	}
	v, err := jwcc.Parse(text, nil)
	if err != nil {
		return nil, err
	}
	return encodeText(jwcc.Format(v, o), enc), nil
}

// ToJSON returns the content of a configuration file as indented standard
// JSON in UTF-8, without comments, trailing commas or byte order mark.
func ToJSON(raw []byte, indent string) ([]byte, error) {
	text, _, err := decodeText(raw)
	if err != nil {
		return nil, err
	}
	v, err := jwcc.Parse(text, nil)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := json.Indent(&b, bytes.TrimSpace(v.Standardize()), "", indent); err != nil {
		return nil, err
	}
	b.WriteByte('\n')
	return b.Bytes(), nil
}

// readFile reads path, refusing more than the parse limit allows.
func readFile(path string, o *Options) (_ []byte, err error) {
	limit := int64(jwcc.DefaultMaxSize)
	if o != nil && o.Limits != nil && o.Limits.MaxSize > 0 {
		limit = int64(o.Limits.MaxSize)
	}
	limit = 2*limit + 2 // UTF-16 with its byte order mark
	f, err := openShared(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := f.Close(); err == nil && cerr != nil {
			err = cerr
		}
	}()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, &Error{File: path, Err: fmt.Errorf("%w: more than %d bytes", jwcc.ErrTooLarge, limit)}
	}
	return data, nil
}
