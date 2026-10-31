package jwcc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"unicode/utf8"
)

// ErrNotFound is wrapped by edits whose path does not exist.
var ErrNotFound = errors.New("jwcc: path not found")

// Set replaces the value at p with the JWCC value raw, keeping the comments
// around the old value. A missing member is appended to its object with the
// indentation of its siblings; missing intermediate members are created as
// objects. On an array, an index equal to the length appends. Set does not
// reformat raw; offsets of the edited nodes become -1.
func (v *Value) Set(p Path, raw []byte) error {
	nv, err := ParseValue(raw)
	if err != nil {
		return err
	}
	nl := v.Newline()
	nv.setNewline([]byte(nl))
	nv.nl = nl
	return v.setNode(p, nv)
}

// SetValue is Set with x encoded by encoding/json (without HTML escaping).
func (v *Value) SetValue(p Path, x any) error {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(x); err != nil {
		return err
	}
	return v.Set(p, bytes.TrimSpace(b.Bytes()))
}

func (v *Value) setNode(p Path, nv *Value) error {
	nl := []byte(v.Newline())
	cur := v
	for i, seg := range p {
		last := i == len(p)-1
		switch {
		case cur.Obj != nil:
			if j := cur.Obj.index(seg); j >= 0 {
				cur = &cur.Obj.Members[j].Value
				continue
			}
			if !utf8.ValidString(seg) {
				return fmt.Errorf("jwcc: invalid UTF-8 in name %q", seg)
			}
			child := Value{Start: -1, End: -1}
			if last {
				child.Lit, child.Obj, child.Arr = nv.Lit, nv.Obj, nv.Arr
			} else {
				child.Obj = &Object{}
			}
			m := cur.Obj.insert(seg, child, nl)
			if !last {
				if ind := lineIndent(m.Name.Before); lastBreak(ind) >= 0 {
					m.Value.Obj.Last = ind
				}
			}
			cur = &m.Value
			if last {
				return nil
			}
		case cur.Arr != nil:
			n, ok := arrayIndex(seg)
			if !ok || n > len(cur.Arr.Elems) || n == len(cur.Arr.Elems) && !last {
				return fmt.Errorf("%w: %s", ErrNotFound, p[:i+1])
			}
			if n == len(cur.Arr.Elems) {
				cur.Arr.appendElem(Value{Lit: nv.Lit, Obj: nv.Obj, Arr: nv.Arr, Start: -1, End: -1}, nl)
				return nil
			}
			cur = &cur.Arr.Elems[n]
		default:
			return fmt.Errorf("%w: %s is not an object or array", ErrNotFound, p[:i])
		}
	}
	cur.Lit, cur.Obj, cur.Arr = nv.Lit, nv.Obj, nv.Arr
	cur.Start, cur.End = -1, -1
	return nil
}

// wsTail splits b into its content and the run of whitespace that ends it.
func wsTail(b []byte) (head, tail []byte) {
	i := len(b)
	for i > 0 && (b[i-1] == ' ' || b[i-1] == '\t' || b[i-1] == '\n' || b[i-1] == '\r') {
		i--
	}
	return b[:i:i], b[i:]
}

// lineIndent returns the line break and indentation that b ends with, or
// its trailing whitespace if it has no line break.
func lineIndent(b []byte) []byte {
	_, tail := wsTail(b)
	if k := lastBreak(tail); k >= 0 {
		tail = tail[k:]
	}
	return slices.Clone(tail)
}

// lastBreak returns the index of the last line break in b (the CR of a
// CRLF), or -1.
func lastBreak(b []byte) int {
	k := bytes.LastIndexAny(b, "\r\n")
	if k > 0 && b[k] == '\n' && b[k-1] == '\r' {
		k--
	}
	return k
}

// breakLen returns the length of the line break at b[k].
func breakLen(b []byte, k int) int {
	if b[k] == '\r' && k+1 < len(b) && b[k+1] == '\n' {
		return 2
	}
	return 1
}

// setNewline rewrites every line break in the whitespace and comments of v
// to nl, so that inserted text matches the document.
func (v *Value) setNewline(nl []byte) {
	fix := func(b []byte) []byte {
		if bytes.IndexAny(b, "\r\n") < 0 {
			return b
		}
		var out []byte
		for i := 0; i < len(b); i++ {
			switch b[i] {
			case '\r':
				if i+1 < len(b) && b[i+1] == '\n' {
					i++
				}
				out = append(out, nl...)
			case '\n':
				out = append(out, nl...)
			default:
				out = append(out, b[i])
			}
		}
		return out
	}
	v.Before, v.After = fix(v.Before), fix(v.After)
	switch {
	case v.Obj != nil:
		v.Obj.Last = fix(v.Obj.Last)
		for i := range v.Obj.Members {
			m := &v.Obj.Members[i]
			m.Name.Before, m.Name.After = fix(m.Name.Before), fix(m.Name.After)
			m.Value.setNewline(nl)
		}
	case v.Arr != nil:
		v.Arr.Last = fix(v.Arr.Last)
		for i := range v.Arr.Elems {
			v.Arr.Elems[i].setNewline(nl)
		}
	}
}

// placeNew computes Before for a new last item and moves the closing
// whitespace onto it. prevBefore is the previous item's Before (nil if
// none), prevAfter the previous item's After, last the container's Last.
func placeNew(prevBefore []byte, prevAfter, last *[]byte, trailing bool, nl []byte) (before, after []byte) {
	if prevAfter == nil { // empty container
		head, tail := wsTail(*last)
		old := *last
		*last = nil
		if k := lastBreak(tail); k >= 0 {
			before = append(slices.Clone(head), nl...)
			before = append(before, string(tail[k+breakLen(tail, k):])+"  "...)
			return before, slices.Clone(tail)
		}
		return slices.Clone(old), nil
	}
	before = lineIndent(prevBefore)
	if trailing {
		// A comment after the last comma belongs to the previous item.
		own, rest := splitSameLine(*last)
		*last = rest
		if len(own) > 0 {
			if bytes.Contains(own, []byte("//")) && lastBreak(before) < 0 {
				before = append(slices.Clone(nl), before...)
			}
			before = append(slices.Clone(own), before...)
		}
		return before, nil
	}
	head, tail := wsTail(*prevAfter)
	*prevAfter = nil
	if len(head) > 0 {
		if bytes.Contains(head, []byte("//")) && lastBreak(before) < 0 {
			before = append(slices.Clone(nl), before...)
		}
		before = append(slices.Clone(head), before...)
	}
	return before, slices.Clone(tail)
}

func (o *Object) insert(key string, val Value, nl []byte) *Member {
	m := Member{Name: Value{Lit: quote(key), Start: -1, End: -1}, Value: val, key: key, keyed: true}
	m.Value.Before = []byte(" ")
	if n := len(o.Members); n > 0 {
		m.Name.Before, m.Value.After = placeNew(o.Members[n-1].Name.Before, &o.Members[n-1].Value.After, &o.Last, o.Trailing, nl)
	} else {
		m.Name.Before, m.Value.After = placeNew(nil, nil, &o.Last, false, nl)
	}
	o.Members = append(o.Members, m)
	if o.idx != nil && len(o.idx) == len(o.Members)-1 {
		o.idx[key] = len(o.Members) - 1
	}
	return &o.Members[len(o.Members)-1]
}

func (a *Array) appendElem(val Value, nl []byte) {
	if n := len(a.Elems); n > 0 {
		before, after := placeNew(a.Elems[n-1].Before, &a.Elems[n-1].After, &a.Last, a.Trailing, nl)
		val.Before, val.After = before, after
	} else {
		val.Before, val.After = placeNew(nil, nil, &a.Last, false, nl)
	}
	a.Elems = append(a.Elems, val)
}

// startsLine reports whether b begins, after blanks, with a line break.
func startsLine(b []byte) bool {
	b = bytes.TrimLeft(b, " \t")
	return len(b) > 0 && (b[0] == '\n' || b[0] == '\r')
}

// splitSameLine splits off the comments that b starts with on the line of
// the previous token; it returns nil and b if there are none.
func splitSameLine(b []byte) (own, rest []byte) {
	i, comment := 0, false
scan:
	for i < len(b) {
		switch c := b[i]; {
		case c == '\n' || c == '\r':
			break scan
		case c == ' ' || c == '\t':
			i++
		case c == '/' && i+1 < len(b) && b[i+1] == '/':
			comment = true
			for i < len(b) && b[i] != '\n' && b[i] != '\r' {
				i++
			}
		case c == '/' && i+1 < len(b) && b[i+1] == '*':
			comment = true
			end := bytes.Index(b[i+2:], []byte("*/"))
			if end < 0 {
				i = len(b)
			} else {
				i += end + 4
			}
		default:
			i++
		}
	}
	if !comment {
		return nil, b
	}
	return slices.Clone(b[:i]), b[i:]
}

// dropSameLine removes a comment that b starts with on the current line;
// after a deletion it belongs to the deleted item.
func dropSameLine(b []byte) []byte {
	_, rest := splitSameLine(b)
	return rest
}

// Delete removes the member or element at p together with its leading
// comments and a comment on the rest of its line.
func (v *Value) Delete(p Path) error {
	if len(p) == 0 {
		return errors.New("jwcc: cannot delete the root")
	}
	parent := v.Find(p[:len(p)-1])
	seg := p[len(p)-1]
	switch {
	case parent == nil:
	case parent.Obj != nil:
		o := parent.Obj
		if i := o.index(seg); i >= 0 {
			n := len(o.Members)
			var prevAfter *[]byte
			if i > 0 {
				prevAfter = &o.Members[i-1].Value.After
			}
			var nextBefore *[]byte
			if i+1 < n {
				nextBefore = &o.Members[i+1].Name.Before
			}
			unlink(o.Members[i].Name.Before, o.Members[i].Value.After, prevAfter, nextBefore, &o.Last, &o.Trailing, n, v.Newline())
			o.Members = slices.Delete(o.Members, i, i+1)
			o.idx = nil
			return nil
		}
	case parent.Arr != nil:
		a := parent.Arr
		if i, ok := arrayIndex(seg); ok && i < len(a.Elems) {
			n := len(a.Elems)
			var prevAfter *[]byte
			if i > 0 {
				prevAfter = &a.Elems[i-1].After
			}
			var nextBefore *[]byte
			if i+1 < n {
				nextBefore = &a.Elems[i+1].Before
			}
			unlink(a.Elems[i].Before, a.Elems[i].After, prevAfter, nextBefore, &a.Last, &a.Trailing, n, v.Newline())
			a.Elems = slices.Delete(a.Elems, i, i+1)
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrNotFound, p)
}

// unlink fixes up the neighbours of a removed item. A comment on the line
// before the item (after the previous comma or the opening bracket) stays;
// a comment on the item's own line goes with it.
func unlink(before, after []byte, prevAfter, nextBefore, last *[]byte, trailing *bool, n int, nl string) {
	own, _ := splitSameLine(before)
	keep := func(rest []byte) []byte {
		if bytes.Contains(own, []byte("//")) && !startsLine(rest) {
			own = append(own, nl...)
		}
		return append(own, rest...)
	}
	switch {
	case nextBefore != nil:
		*nextBefore = keep(dropSameLine(*nextBefore))
	case *trailing:
		*last = keep(dropSameLine(*last))
		if n == 1 {
			*trailing = false
		}
	default:
		_, tail := wsTail(after)
		add := keep(slices.Clone(tail))
		if prevAfter != nil {
			*prevAfter = append(slices.Clip(*prevAfter), add...)
		} else {
			*last = add
		}
	}
}

// Rename changes the name of the member at p, keeping its value and
// comments.
func (v *Value) Rename(p Path, name string) error {
	if len(p) == 0 {
		return errors.New("jwcc: cannot rename the root")
	}
	parent := v.Find(p[:len(p)-1])
	if parent == nil || parent.Obj == nil {
		return fmt.Errorf("%w: %s", ErrNotFound, p)
	}
	i := parent.Obj.index(p[len(p)-1])
	if i < 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, p)
	}
	if !utf8.ValidString(name) {
		return fmt.Errorf("jwcc: invalid UTF-8 in name %q", name)
	}
	if j := parent.Obj.index(name); j >= 0 && j != i {
		return fmt.Errorf("jwcc: rename %s: name %q exists", p, name)
	}
	parent.Obj.Members[i].Name.Lit = quote(name)
	parent.Obj.Members[i].key, parent.Obj.Members[i].keyed = name, true
	parent.Obj.idx = nil
	parent.Obj.Members[i].Name.Start, parent.Obj.Members[i].Name.End = -1, -1
	return nil
}

// Move deletes the value at from and sets it at to. Comments inside the
// value move with it; comments around it stay behind or are dropped as by
// Delete.
func (v *Value) Move(from, to Path) error {
	if len(to) >= len(from) && slices.Equal(to[:len(from)], from) {
		return fmt.Errorf("jwcc: move %s into itself", from)
	}
	src := v.Find(from)
	if src == nil {
		return fmt.Errorf("%w: %s", ErrNotFound, from)
	}
	node := &Value{Lit: src.Lit, Obj: src.Obj, Arr: src.Arr}
	if err := v.Delete(from); err != nil {
		return err
	}
	return v.setNode(to, node)
}
