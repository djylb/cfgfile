package jwcc

import (
	"bytes"
	"slices"
	"strings"
)

// Trailing selects how Format writes a comma after the last member or
// element of a multi-line object or array. Single-line arrays never get
// one.
type Trailing int

const (
	// TrailingMultiline adds a trailing comma to every multi-line
	// container (the default).
	TrailingMultiline Trailing = iota
	// TrailingKeep keeps a trailing comma where the input had one.
	TrailingKeep
	// TrailingRemove writes no trailing commas.
	TrailingRemove
)

// FormatOptions configures Format. A nil *FormatOptions uses the defaults.
type FormatOptions struct {
	// Indent is one level of indentation; default two spaces. Use "\t" for
	// tabs.
	Indent string
	// Trailing is the trailing comma policy.
	Trailing Trailing
	// Newline is the line ending to write; default the document's dominant
	// one (LF if it has none).
	Newline string
}

// Format returns v in canonical layout: one member per line, consistent
// indentation, `"name": value` with one space, at most one blank line
// between members, comments kept with the member they precede or follow
// and re-indented. Member order, literal text and the byte order mark are
// kept. Arrays of plain values that were on one line without comments or
// a trailing comma stay on one line. Format is idempotent and does not
// change the decoded value.
func Format(v *Value, o *FormatOptions) []byte {
	f := &formatter{ind: "  ", nl: v.Newline()}
	if o != nil {
		if o.Indent != "" {
			f.ind = o.Indent
		}
		if o.Newline != "" {
			f.nl = o.Newline
		}
		f.trailing = o.Trailing
	}
	if bytes.HasPrefix(v.Before, bom) {
		f.b.Write(bom)
	}
	lead, tb := f.comments(v.Before)
	for j, c := range lead {
		if j > 0 && c.breaks >= 2 {
			f.b.WriteString(f.nl)
		}
		f.b.Write(c.text)
		f.b.WriteString(f.nl)
	}
	if len(lead) > 0 && tb >= 2 {
		f.b.WriteString(f.nl)
	}
	f.value(v, 0)
	after, _ := f.comments(v.After)
	trail, rest := sameLine(after)
	f.trail(trail, 0)
	f.b.WriteString(f.nl)
	for _, c := range rest {
		if c.breaks >= 2 {
			f.b.WriteString(f.nl)
		}
		f.b.Write(c.text)
		f.b.WriteString(f.nl)
	}
	return f.b.Bytes()
}

type formatter struct {
	b        bytes.Buffer
	ind, nl  string
	trailing Trailing
}

type comment struct {
	text   []byte
	breaks int // line breaks between the previous token and this comment
}

// comments lists the comments in an extra and the line breaks after the
// last one (or in all of it, if it has none).
func (f *formatter) comments(e []byte) (cs []comment, tail int) {
	e = bytes.TrimPrefix(e, bom)
	breaks := 0
	for i := 0; i < len(e); {
		switch c := e[i]; {
		case c == '\r':
			if i+1 < len(e) && e[i+1] == '\n' {
				i++
			}
			fallthrough
		case c == '\n':
			breaks++
			i++
		case c == ' ' || c == '\t':
			i++
		case c == '/' && e[i+1] == '/':
			j := i
			for j < len(e) && e[j] != '\n' && e[j] != '\r' {
				j++
			}
			cs = append(cs, comment{bytes.TrimRight(e[i:j], " \t"), breaks})
			breaks, i = 0, j
		default: // block comment
			j := i + 2 + bytes.Index(e[i+2:], []byte("*/")) + 2
			cs = append(cs, comment{f.newlines(e[i:j]), breaks})
			breaks, i = 0, j
		}
	}
	return cs, breaks
}

// newlines rewrites the line breaks inside a block comment.
func (f *formatter) newlines(b []byte) []byte {
	if bytes.IndexAny(b, "\r\n") < 0 {
		return b
	}
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return []byte(strings.ReplaceAll(s, "\n", f.nl))
}

// sameLine splits off the comments on the line of the previous token.
func sameLine(cs []comment) (same, rest []comment) {
	i := 0
	for i < len(cs) && cs[i].breaks == 0 {
		i++
		if bytes.HasPrefix(cs[i-1].text, []byte("//")) {
			break
		}
	}
	return cs[:i:i], cs[i:]
}

// trail writes comments after a value on its line; comments that follow
// a line comment go on lines of their own at depth.
func (f *formatter) trail(cs []comment, depth int) {
	line := false
	for _, c := range cs {
		if line {
			f.b.WriteString(f.nl)
			f.indent(depth)
		} else {
			f.b.WriteByte(' ')
		}
		f.b.Write(c.text)
		line = line || bytes.HasPrefix(c.text, []byte("//"))
	}
}

func (f *formatter) indent(depth int) {
	for range depth {
		f.b.WriteString(f.ind)
	}
}

type entry struct {
	name  []byte
	val   *Value
	lead  []comment
	blank bool // blank line between lead comments (or the previous entry) and the entry
	trail []comment
}

func (f *formatter) value(v *Value, depth int) {
	switch {
	case v.Obj != nil:
		f.container(v, depth, '{', '}')
	case v.Arr != nil:
		if f.inline(v.Arr) {
			f.b.WriteByte('[')
			for i := range v.Arr.Elems {
				if i > 0 {
					f.b.WriteString(", ")
				}
				f.b.Write(v.Arr.Elems[i].Lit)
			}
			f.b.WriteByte(']')
			return
		}
		f.container(v, depth, '[', ']')
	default:
		f.b.Write(v.Lit)
	}
}

func blankish(e []byte) bool { return len(bytes.Trim(e, " \t")) == 0 }

// inline reports an array of plain values written on one line with no
// comments and no trailing comma.
func (f *formatter) inline(a *Array) bool {
	if a.Trailing || !blankish(a.Last) {
		return false
	}
	for i := range a.Elems {
		e := &a.Elems[i]
		if e.Lit == nil || !blankish(e.Before) || !blankish(e.After) {
			return false
		}
	}
	return true
}

func (f *formatter) container(v *Value, depth int, open, close byte) {
	var ents []entry
	var last []byte
	var trailing bool
	var pending []comment
	add := func(before, mid, after []byte, name []byte, val *Value) {
		cs, tb := f.comments(before)
		if len(ents) > 0 {
			var same []comment
			same, cs = sameLine(cs)
			ents[len(ents)-1].trail = append(slices.Clip(ents[len(ents)-1].trail), same...)
		}
		e := entry{name: name, val: val, lead: append(slices.Clip(pending), cs...)}
		pending = nil
		e.blank = tb >= 2 && (len(ents) > 0 || len(e.lead) > 0)
		m, _ := f.comments(mid)
		for _, c := range m {
			c.breaks = 1
			e.lead = append(e.lead, c)
		}
		ac, _ := f.comments(after)
		same, rest := sameLine(ac)
		e.trail = same
		pending = slices.Clip(rest)
		ents = append(ents, e)
	}
	if v.Obj != nil {
		for i := range v.Obj.Members {
			m := &v.Obj.Members[i]
			add(m.Name.Before, append(append([]byte(nil), m.Name.After...), m.Value.Before...), m.Value.After, m.Name.Lit, &m.Value)
		}
		last, trailing = v.Obj.Last, v.Obj.Trailing
	} else {
		for i := range v.Arr.Elems {
			e := &v.Arr.Elems[i]
			add(e.Before, nil, e.After, nil, e)
		}
		last, trailing = v.Arr.Last, v.Arr.Trailing
	}
	cs, _ := f.comments(last)
	if len(ents) > 0 {
		var same []comment
		same, cs = sameLine(cs)
		ents[len(ents)-1].trail = append(slices.Clip(ents[len(ents)-1].trail), same...)
	}
	tail := append(slices.Clip(pending), cs...)
	if len(ents) == 0 && len(tail) == 0 {
		f.b.WriteByte(open)
		f.b.WriteByte(close)
		return
	}
	f.b.WriteByte(open)
	f.b.WriteString(f.nl)
	first := true
	lines := func(cs []comment) {
		for _, c := range cs {
			if c.breaks >= 2 && !first {
				f.b.WriteString(f.nl)
			}
			first = false
			f.indent(depth + 1)
			f.b.Write(c.text)
			f.b.WriteString(f.nl)
		}
	}
	for i := range ents {
		e := &ents[i]
		lines(e.lead)
		if e.blank && !first {
			f.b.WriteString(f.nl)
		}
		first = false
		f.indent(depth + 1)
		if e.name != nil {
			f.b.Write(e.name)
			f.b.WriteString(": ")
		}
		f.value(e.val, depth+1)
		if i < len(ents)-1 || f.trailing == TrailingMultiline || f.trailing == TrailingKeep && trailing {
			f.b.WriteByte(',')
		}
		f.trail(e.trail, depth+1)
		f.b.WriteString(f.nl)
	}
	lines(tail)
	f.indent(depth)
	f.b.WriteByte(close)
}
