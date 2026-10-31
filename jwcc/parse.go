package jwcc

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Default limits used when Limits or one of its fields is zero.
const (
	DefaultMaxDepth = 64
	DefaultMaxSize  = 1 << 20
)

// Limits bounds parsing. A nil *Limits uses the defaults.
type Limits struct {
	// MaxDepth is the deepest nesting of objects and arrays allowed.
	MaxDepth int
	// MaxSize is the largest input accepted, in bytes.
	MaxSize int
}

func (o *Limits) limits() (depth, size int) {
	depth, size = DefaultMaxDepth, DefaultMaxSize
	if o != nil {
		if o.MaxDepth > 0 {
			depth = o.MaxDepth
		}
		if o.MaxSize > 0 {
			size = o.MaxSize
		}
	}
	return
}

// SyntaxError reports malformed input. Line and Column are 1-based; Column
// counts bytes. Path is the value being parsed when the error occurred.
type SyntaxError struct {
	Offset       int
	Line, Column int
	Path         Path
	Msg          string
	err          error
}

// Unwrap returns ErrNotUTF8 for invalid UTF-8 and nil otherwise.
func (e *SyntaxError) Unwrap() error { return e.err }

func (e *SyntaxError) Error() string {
	if len(e.Path) > 0 {
		return fmt.Sprintf("%d:%d: %s: %s", e.Line, e.Column, e.Path, e.Msg)
	}
	return fmt.Sprintf("%d:%d: %s", e.Line, e.Column, e.Msg)
}

// ErrTooLarge is wrapped by the error for input above Limits.MaxSize.
var ErrTooLarge = errors.New("jwcc: input too large")

type parser struct {
	data     []byte
	pos      int
	depth    int
	maxDepth int
	path     Path
}

// Parse parses a complete JWCC document. Duplicate names in an object are
// an error.
func Parse(data []byte, opts *Limits) (*Value, error) {
	maxDepth, maxSize := opts.limits()
	if len(data) > maxSize {
		return nil, fmt.Errorf("%w: %d bytes, limit %d", ErrTooLarge, len(data), maxSize)
	}
	p := &parser{data: data, maxDepth: maxDepth}
	v := new(Value)
	if err := p.value(v); err != nil {
		return nil, err
	}
	if p.pos < len(data) {
		return nil, p.errf("unexpected %s after top-level value", p.desc())
	}
	v.nl = Newline(data)
	return v, nil
}

// ParseValue parses a single value, such as the argument of Set.
func ParseValue(data []byte) (*Value, error) { return Parse(data, &Limits{MaxSize: len(data) + 1}) }

func (p *parser) errf(format string, args ...any) *SyntaxError {
	l, c := Position(p.data, p.pos)
	msg := fmt.Sprintf(format, args...)
	if strings.HasPrefix(format, "unexpected") && p.pos < len(p.data) {
		r, _ := utf8.DecodeRune(p.data[p.pos:])
		if l, ok := lookalikes[r]; ok {
			msg = fmt.Sprintf("%s %q; use '%s'", l.name, r, l.use)
		}
	}
	return &SyntaxError{Offset: p.pos, Line: l, Column: c, Path: append(Path(nil), p.path...), Msg: msg}
}

// lookalikes are characters that input methods produce in place of JSON
// punctuation. They are reported by name, never accepted.
var lookalikes = map[rune]struct{ name, use string }{
	'：':      {"full-width colon", ":"},
	'，':      {"full-width comma", ","},
	'、':      {"ideographic comma", ","},
	'；':      {"full-width semicolon", ","},
	'“':      {"curly quote", `"`},
	'”':      {"curly quote", `"`},
	'‘':      {"curly quote", `"`},
	'’':      {"curly quote", `"`},
	'＂':      {"full-width quote", `"`},
	'｛':      {"full-width brace", "{"},
	'｝':      {"full-width brace", "}"},
	'［':      {"full-width bracket", "["},
	'］':      {"full-width bracket", "]"},
	'【':      {"CJK bracket", "["},
	'】':      {"CJK bracket", "]"},
	'－':      {"full-width minus", "-"},
	'／':      {"full-width slash", "/"},
	'\u00a0': {"non-breaking space", " "},
	'\u3000': {"full-width space", " "},
	'\ufeff': {"byte order mark", "nothing"},
}

func (p *parser) desc() string {
	if p.pos >= len(p.data) {
		return "end of input"
	}
	r, _ := utf8.DecodeRune(p.data[p.pos:])
	return strconv.QuoteRune(r)
}

// extra consumes whitespace and comments.
func (p *parser) extra() ([]byte, error) {
	start := p.pos
	for p.pos < len(p.data) {
		switch c := p.data[p.pos]; {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			p.pos++
		case c == '/' && p.pos+1 < len(p.data) && p.data[p.pos+1] == '/':
			for p.pos < len(p.data) && p.data[p.pos] != '\n' && p.data[p.pos] != '\r' {
				if err := p.utf8(); err != nil {
					return nil, err
				}
			}
		case c == '/' && p.pos+1 < len(p.data) && p.data[p.pos+1] == '*':
			open := p.pos
			p.pos += 2
			for p.pos+1 < len(p.data) && (p.data[p.pos] != '*' || p.data[p.pos+1] != '/') {
				if err := p.utf8(); err != nil {
					return nil, err
				}
			}
			if p.pos+1 >= len(p.data) {
				p.pos = open
				return nil, p.errf("unterminated block comment")
			}
			p.pos += 2
		case p.pos == 0 && bytes.HasPrefix(p.data, bom):
			p.pos = len(bom)
		default:
			return p.slice(start), nil
		}
	}
	return p.slice(start), nil
}

func (p *parser) slice(start int) []byte {
	if start == p.pos {
		return nil
	}
	return p.data[start:p.pos:p.pos]
}

// value parses Before, the value and After into v.
func (p *parser) value(v *Value) (err error) {
	if v.Before, err = p.extra(); err != nil {
		return err
	}
	if err = p.bare(v); err != nil {
		return err
	}
	v.After, err = p.extra()
	return err
}

func (p *parser) bare(v *Value) error {
	if p.pos >= len(p.data) {
		return p.errf("unexpected end of input, want a value")
	}
	v.Start = p.pos
	defer func() { v.End = p.pos }()
	switch c := p.data[p.pos]; {
	case c == '{':
		return p.object(v)
	case c == '[':
		return p.array(v)
	case c == '"':
		return p.str(v)
	case c == '-' || c >= '0' && c <= '9':
		return p.number(v)
	}
	for _, w := range []string{"true", "false", "null"} {
		if len(p.data)-p.pos >= len(w) && string(p.data[p.pos:p.pos+len(w)]) == w {
			p.pos += len(w)
			v.Lit = p.slice(v.Start)
			return nil
		}
	}
	if r, n := utf8.DecodeRune(p.data[p.pos:]); r == utf8.RuneError && n == 1 {
		return p.notUTF8()
	}
	return p.errf("unexpected %s, want a value", p.desc())
}

func (p *parser) enter() error {
	if p.depth++; p.depth > p.maxDepth {
		return p.errf("nesting deeper than %d", p.maxDepth)
	}
	return nil
}

func (p *parser) object(v *Value) error {
	if err := p.enter(); err != nil {
		return err
	}
	p.pos++
	o := &Object{}
	v.Obj = o
	o.idx = map[string]int{}
	for {
		ex, err := p.extra()
		if err != nil {
			return err
		}
		if p.pos < len(p.data) && p.data[p.pos] == '}' {
			o.Last = ex
			break
		}
		var m Member
		m.Name.Before = ex
		if p.pos >= len(p.data) || p.data[p.pos] != '"' {
			return p.errf("unexpected %s, want a member name", p.desc())
		}
		m.Name.Start = p.pos
		if err := p.str(&m.Name); err != nil {
			return err
		}
		m.Name.End = p.pos
		key := m.Key()
		m.key, m.keyed = key, true
		if _, dup := o.idx[key]; dup {
			p.pos = m.Name.Start
			return p.errf("duplicate name %q", key)
		}
		if m.Name.After, err = p.extra(); err != nil {
			return err
		}
		if p.pos >= len(p.data) || p.data[p.pos] != ':' {
			return p.errf("unexpected %s, want ':'", p.desc())
		}
		p.pos++
		p.path = append(p.path, key)
		if err := p.value(&m.Value); err != nil {
			return err
		}
		p.path = p.path[:len(p.path)-1]
		o.idx[key] = len(o.Members)
		o.Members = append(o.Members, m)
		if p.pos < len(p.data) && p.data[p.pos] == ',' {
			p.pos++
			o.Trailing = true
			continue
		}
		o.Trailing = false
		if p.pos >= len(p.data) || p.data[p.pos] != '}' {
			return p.errf("unexpected %s, want ',' or '}'", p.desc())
		}
		break
	}
	p.pos++
	p.depth--
	return nil
}

func (p *parser) array(v *Value) error {
	if err := p.enter(); err != nil {
		return err
	}
	p.pos++
	a := &Array{}
	v.Arr = a
	for {
		ex, err := p.extra()
		if err != nil {
			return err
		}
		if p.pos < len(p.data) && p.data[p.pos] == ']' {
			a.Last = ex
			break
		}
		e := Value{Before: ex}
		p.path = append(p.path, itoa(len(a.Elems)))
		if err := p.bare(&e); err != nil {
			return err
		}
		if e.After, err = p.extra(); err != nil {
			return err
		}
		p.path = p.path[:len(p.path)-1]
		a.Elems = append(a.Elems, e)
		if p.pos < len(p.data) && p.data[p.pos] == ',' {
			p.pos++
			a.Trailing = true
			continue
		}
		a.Trailing = false
		if p.pos >= len(p.data) || p.data[p.pos] != ']' {
			return p.errf("unexpected %s, want ',' or ']'", p.desc())
		}
		break
	}
	p.pos++
	p.depth--
	return nil
}

func (p *parser) str(v *Value) error {
	start := p.pos
	p.pos++
	for p.pos < len(p.data) {
		c := p.data[p.pos]
		switch {
		case c == '"':
			p.pos++
			v.Lit = p.slice(start)
			return nil
		case c < 0x20:
			return p.errf("control character in string")
		case c == '\\':
			if p.pos+1 >= len(p.data) {
				return p.errf("unterminated string")
			}
			switch p.data[p.pos+1] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				p.pos += 2
			case 'u':
				if p.pos+6 > len(p.data) {
					return p.errf("unterminated string")
				}
				for _, h := range p.data[p.pos+2 : p.pos+6] {
					if !(h >= '0' && h <= '9' || h >= 'a' && h <= 'f' || h >= 'A' && h <= 'F') {
						return p.errf("invalid \\u escape")
					}
				}
				p.pos += 6
			default:
				return p.errf("invalid escape")
			}
		default:
			if err := p.utf8(); err != nil {
				return err
			}
		}
	}
	return p.errf("unterminated string")
}

// bom is the UTF-8 byte order mark, accepted at the start of the input and
// kept in the root's Before.
var bom = []byte{0xEF, 0xBB, 0xBF}

// ErrNotUTF8 is wrapped by the error for input that is not valid UTF-8.
var ErrNotUTF8 = errors.New("the file is not UTF-8; save it as UTF-8")

// utf8 consumes one UTF-8 character, failing on an invalid byte.
func (p *parser) utf8() error {
	if p.data[p.pos] < utf8.RuneSelf {
		p.pos++
		return nil
	}
	r, n := utf8.DecodeRune(p.data[p.pos:])
	if r == utf8.RuneError && n == 1 {
		return p.notUTF8()
	}
	p.pos += n
	return nil
}

func (p *parser) notUTF8() error {
	err := p.errf("invalid UTF-8 byte 0x%02x: %v", p.data[p.pos], ErrNotUTF8)
	err.err = ErrNotUTF8
	return err
}

func (p *parser) number(v *Value) error {
	start := p.pos
	d := p.data
	digits := func() int {
		s := p.pos
		for p.pos < len(d) && d[p.pos] >= '0' && d[p.pos] <= '9' {
			p.pos++
		}
		return p.pos - s
	}
	if d[p.pos] == '-' {
		p.pos++
	}
	if p.pos < len(d) && d[p.pos] == '0' {
		p.pos++
	} else if digits() == 0 {
		return p.errf("invalid number")
	}
	if p.pos < len(d) && d[p.pos] == '.' {
		p.pos++
		if digits() == 0 {
			return p.errf("invalid number")
		}
	}
	if p.pos < len(d) && (d[p.pos] == 'e' || d[p.pos] == 'E') {
		p.pos++
		if p.pos < len(d) && (d[p.pos] == '+' || d[p.pos] == '-') {
			p.pos++
		}
		if digits() == 0 {
			return p.errf("invalid number")
		}
	}
	v.Lit = p.slice(start)
	return nil
}
