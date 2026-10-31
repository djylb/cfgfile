// Package jwcc parses and edits JWCC documents: JSON with "//" and "/* */"
// comments and trailing commas, and nothing else.
//
// Parse builds a syntax tree that keeps every byte of the input, so Pack
// reproduces the input exactly. Standardize turns a document into strict
// JSON of the same length, so byte offsets, lines and columns reported by
// encoding/json point into the original text. Set, Delete, Rename and Move
// edit the tree by path and leave comments and formatting they do not touch
// as they were.
//
// The package has no global state and starts no goroutines. A tree may be
// read concurrently; editing it needs exclusive access.
package jwcc

import (
	"bytes"
	"encoding/json"
)

// Value is one JSON value with the whitespace and comments around it.
// Exactly one of Lit, Obj and Arr is set.
type Value struct {
	// Before holds the whitespace and comments before the value.
	Before []byte
	// Lit is the raw text of a string, number, true, false or null.
	Lit []byte
	Obj *Object
	Arr *Array
	// After holds the whitespace and comments after the value, up to the
	// next comma, closing bracket or end of input.
	After []byte
	// Start and End are the byte offsets of the value (without Before and
	// After) in the parsed input, or -1 once an edit has made them stale.
	Start, End int
	nl         string // cached Newline of a parsed document
	unit       string // cached first indentation of the document
}

// Object is a JSON object. Edit Members through the methods of Value, or
// call Reindex after changing them directly.
type Object struct {
	Members []Member
	idx     map[string]int
	// Last holds the whitespace and comments before the closing brace that
	// follow a trailing comma, or all of the inside of an empty object.
	Last []byte
	// Trailing reports a comma after the last member.
	Trailing bool
}

// Member is one name-value pair; Name.Lit is the quoted name.
type Member struct {
	Name, Value Value
	key         string // decoded Name.Lit, if keyed
	keyed       bool
}

// Reindex drops the name index after Members was changed directly.
func (o *Object) Reindex() {
	o.idx = nil
	for i := range o.Members {
		o.Members[i].keyed = false
	}
}

// Array is a JSON array.
type Array struct {
	Elems    []Value
	Last     []byte
	Trailing bool
}

// Kind returns '{', '[', '"', '0' (number), 't', 'f' or 'n'.
func (v *Value) Kind() byte {
	switch {
	case v.Obj != nil:
		return '{'
	case v.Arr != nil:
		return '['
	case len(v.Lit) == 0:
		return 0
	case v.Lit[0] == '-' || v.Lit[0] >= '0' && v.Lit[0] <= '9':
		return '0'
	}
	return v.Lit[0]
}

// Key returns the unquoted member name.
func (m *Member) Key() string {
	if m.keyed {
		return m.key
	}
	var s string
	_ = json.Unmarshal(m.Name.Lit, &s)
	return s
}

// Pack returns the document text.
func (v *Value) Pack() []byte {
	var b bytes.Buffer
	v.write(&b, false)
	return b.Bytes()
}

// Standardize returns the document as strict JSON of the same length: every
// byte of a comment other than a newline, and every trailing comma, becomes
// a space.
func (v *Value) Standardize() []byte {
	var b bytes.Buffer
	v.write(&b, true)
	return b.Bytes()
}

// Standardize parses data and returns its standardized form.
func Standardize(data []byte, opts *Limits) ([]byte, error) {
	v, err := Parse(data, opts)
	if err != nil {
		return nil, err
	}
	return v.Standardize(), nil
}

func writeExtra(b *bytes.Buffer, e []byte, std bool) {
	if !std {
		b.Write(e)
		return
	}
	for _, c := range e {
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			b.WriteByte(c)
		} else {
			b.WriteByte(' ')
		}
	}
}

func (v *Value) write(b *bytes.Buffer, std bool) {
	writeExtra(b, v.Before, std)
	switch {
	case v.Obj != nil:
		b.WriteByte('{')
		for i := range v.Obj.Members {
			m := &v.Obj.Members[i]
			if i > 0 {
				b.WriteByte(',')
			}
			m.Name.write(b, std)
			b.WriteByte(':')
			m.Value.write(b, std)
		}
		writeClose(b, v.Obj.Trailing, v.Obj.Last, '}', std)
	case v.Arr != nil:
		b.WriteByte('[')
		for i := range v.Arr.Elems {
			if i > 0 {
				b.WriteByte(',')
			}
			v.Arr.Elems[i].write(b, std)
		}
		writeClose(b, v.Arr.Trailing, v.Arr.Last, ']', std)
	default:
		b.Write(v.Lit)
	}
	writeExtra(b, v.After, std)
}

func writeClose(b *bytes.Buffer, trailing bool, last []byte, c byte, std bool) {
	if trailing {
		if std {
			b.WriteByte(' ')
		} else {
			b.WriteByte(',')
		}
	}
	writeExtra(b, last, std)
	b.WriteByte(c)
}

// Find returns the value at p, or nil.
func (v *Value) Find(p Path) *Value {
	cur := v
	for _, s := range p {
		if cur = cur.child(s); cur == nil {
			return nil
		}
	}
	return cur
}

func (v *Value) child(s string) *Value {
	switch {
	case v.Obj != nil:
		if i := v.Obj.index(s); i >= 0 {
			return &v.Obj.Members[i].Value
		}
	case v.Arr != nil:
		if i, ok := arrayIndex(s); ok && i < len(v.Arr.Elems) {
			return &v.Arr.Elems[i]
		}
	}
	return nil
}

func (o *Object) index(name string) int {
	if o.idx == nil || len(o.idx) != len(o.Members) {
		o.idx = make(map[string]int, len(o.Members))
		for i := range o.Members {
			o.idx[o.Members[i].Key()] = i
		}
	}
	if i, ok := o.idx[name]; ok && i < len(o.Members) && o.Members[i].Key() == name {
		return i
	}
	return -1
}

// Locate returns the path of the innermost value whose text contains the
// byte offset off, as parsed (offsets are stale after edits).
func (v *Value) Locate(off int) Path {
	var p Path
	cur := v
	for {
		var next *Value
		var seg string
		switch {
		case cur.Obj != nil:
			for i := range cur.Obj.Members {
				m := &cur.Obj.Members[i]
				if off >= m.Name.Start && off < m.Value.End {
					next, seg = &m.Value, m.Key()
					break
				}
			}
		case cur.Arr != nil:
			for i := range cur.Arr.Elems {
				e := &cur.Arr.Elems[i]
				if off >= e.Start && off < e.End {
					next, seg = e, itoa(i)
					break
				}
			}
		}
		if next == nil {
			return p
		}
		p = append(p, seg)
		cur = next
	}
}

// Position converts a byte offset in data into a 1-based line and byte
// column. LF, CRLF and a lone CR each end a line; a leading UTF-8 byte
// order mark does not count.
func Position(data []byte, off int) (line, col int) {
	off = min(max(off, 0), len(data))
	line, start := 1, 0
	if bytes.HasPrefix(data, bom) && off >= len(bom) {
		start = len(bom)
	}
	for i := 0; i < off; i++ {
		switch data[i] {
		case '\r':
			if i+1 < len(data) && data[i+1] == '\n' {
				continue
			}
			fallthrough
		case '\n':
			line, start = line+1, i+1
		}
	}
	return line, off - start + 1
}

// Newline returns the dominant line ending of the document: "\n", "\r\n"
// or "\r"; "\n" if it has none. Edits write new line breaks in this style.
func (v *Value) Newline() string {
	if v.nl == "" {
		v.nl = Newline(v.Pack())
	}
	return v.nl
}

// Newline returns the dominant line ending of data, as Value.Newline.
func Newline(data []byte) string {
	var lf, crlf, cr int
	for i := 0; i < len(data); i++ {
		switch data[i] {
		case '\n':
			lf++
		case '\r':
			if i+1 < len(data) && data[i+1] == '\n' {
				crlf++
				i++
			} else {
				cr++
			}
		}
	}
	switch {
	case crlf > lf && crlf >= cr:
		return "\r\n"
	case cr > lf && cr > crlf:
		return "\r"
	}
	return "\n"
}
