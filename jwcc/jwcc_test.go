package jwcc

import (
	"bytes"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"strings"
	"testing"
)

const sample = `{
  "$schema": "https://example.com/app-1.json",
  "version": 1,

  // data directory
  "data_dir": "/var/lib/app",

  /* admin
     endpoint */
  "admin": {
    "listen": "127.0.0.1:8081",
    "bootstrap": {
      "admin_user": "admin",
    },
  },

  "log": {
    "level": "info",          // debug | info
    "format": "text"          // text | json
  },
  "list": [1, 2.5e3, -0, true, null, "\u00e9\n", {}, [],],
}
`

func TestRoundTrip(t *testing.T) {
	for _, in := range []string{sample, `1`, ` [ ] `, `{}`, "\"x\" // c", "/*a*/{/*b*/}/*c*/", `{"a":{"b":[[[]]]}}`} {
		v, err := Parse([]byte(in), nil)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got := string(v.Pack()); got != in {
			t.Fatalf("Pack = %q, want %q", got, in)
		}
	}
}

func TestStandardize(t *testing.T) {
	std, err := Standardize([]byte(sample), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(std) != len(sample) || bytes.Count(std, []byte{'\n'}) != strings.Count(sample, "\n") {
		t.Fatal("length or lines changed")
	}
	if !json.Valid(std) {
		t.Fatalf("not valid JSON:\n%s", std)
	}
	var m map[string]any
	if err := json.Unmarshal(std, &m); err != nil || m["data_dir"] != "/var/lib/app" {
		t.Fatal(err, m)
	}
}

func TestErrors(t *testing.T) {
	for _, c := range []struct {
		in   string
		line int
		col  int
		path string
	}{
		{"{\n  \"a\": {\n    \"b\": tru\n  }\n}", 3, 10, "a.b"},
		{"{\"a\": [1, 2, x]}", 1, 14, "a[2]"},
		{"{\"a\":1,\"a\":2}", 1, 8, ""},
		{"{\"a\":1 \"b\"}", 1, 8, ""},
		{"/* x", 1, 1, ""},
		{"[1,,]", 1, 4, "[1]"},
		{"01", 1, 2, ""},
		{"\"\x01\"", 1, 2, ""},
		{"\"\xff\"", 1, 2, ""},
		{"{,}", 1, 2, ""},
		{"[1 2]", 1, 4, ""},
		{"", 1, 1, ""},
		{"1 2", 1, 3, ""},
	} {
		_, err := Parse([]byte(c.in), nil)
		var se *SyntaxError
		if !errors.As(err, &se) {
			t.Fatalf("%q: err = %v", c.in, err)
		}
		if se.Line != c.line || se.Column != c.col || se.Path.String() != c.path {
			t.Errorf("%q: got %d:%d %q (%v), want %d:%d %q", c.in, se.Line, se.Column, se.Path, se, c.line, c.col, c.path)
		}
	}
}

func TestLimits(t *testing.T) {
	deep := strings.Repeat("[", 10) + strings.Repeat("]", 10)
	if _, err := Parse([]byte(deep), &Options{MaxDepth: 9}); err == nil {
		t.Fatal("depth not bounded")
	}
	if _, err := Parse([]byte(deep), &Options{MaxDepth: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse([]byte(deep), &Options{MaxSize: 19}); !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
	if _, err := Parse([]byte(strings.Repeat("[", 100000)), nil); err == nil {
		t.Fatal("deep input accepted")
	}
}

func TestPath(t *testing.T) {
	for _, s := range []string{"", "a", "a.b", "a[0].b", `a["x.y"][3]`, `["0"]`, `[""]`} {
		p, err := ParsePath(s)
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		want := s
		if s == `["0"]` {
			want = "[0]"
		}
		if p.String() != want {
			t.Errorf("%q -> %q", s, p.String())
		}
	}
	for _, s := range []string{"a.", "a..b", "a[", "a[x]", "a[01]", `a["x]`, ".a"} {
		if _, err := ParsePath(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}

func TestLocate(t *testing.T) {
	v, _ := Parse([]byte(sample), nil)
	off := strings.Index(sample, `"text"`)
	if got := v.Locate(off).String(); got != "log.format" {
		t.Fatal(got)
	}
}

func mustPath(s string) Path {
	p, err := ParsePath(s)
	if err != nil {
		panic(err)
	}
	return p
}

func edit(t *testing.T, in string, f func(v *Value) error) string {
	t.Helper()
	v, err := Parse([]byte(in), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f(v); err != nil {
		t.Fatal(err)
	}
	out := v.Pack()
	if _, err := Parse(out, nil); err != nil {
		t.Fatalf("edit produced invalid JWCC: %v\n%s", err, out)
	}
	return string(out)
}

func TestPatch(t *testing.T) {
	cases := []struct {
		name, in, want string
		f              func(v *Value) error
	}{
		{"replace keeps comments", "{\n  \"a\": 1, // one\n  \"b\": 2\n}", "{\n  \"a\": \"x\", // one\n  \"b\": 2\n}",
			func(v *Value) error { return v.SetValue(mustPath("a"), "x") }},
		{"append after line comment", "{\n  \"a\": 1 // one\n}", "{\n  \"a\": 1, // one\n  \"b\": 2\n}",
			func(v *Value) error { return v.Set(mustPath("b"), []byte("2")) }},
		{"append with trailing comma", "{\n    \"a\": 1,\n}", "{\n    \"a\": 1,\n    \"b\": 2,\n}",
			func(v *Value) error { return v.Set(mustPath("b"), []byte("2")) }},
		{"append compact", `{"a":1}`, `{"a":1,"b": 2}`,
			func(v *Value) error { return v.Set(mustPath("b"), []byte("2")) }},
		{"create intermediate", "{\n  \"a\": 1\n}", "{\n  \"a\": 1,\n  \"x\": {\n    \"y\": true\n  }\n}",
			func(v *Value) error { return v.Set(mustPath("x.y"), []byte("true")) }},
		{"empty object", "{\n}", "{\n  \"a\": 1\n}",
			func(v *Value) error { return v.Set(mustPath("a"), []byte("1")) }},
		{"delete with comments", "{\n  \"a\": 1,\n  // about b\n  \"b\": 2, // two\n  \"c\": 3\n}", "{\n  \"a\": 1,\n  \"c\": 3\n}",
			func(v *Value) error { return v.Delete(mustPath("b")) }},
		{"delete last", "{\n  \"a\": 1,\n  \"b\": 2 // two\n}", "{\n  \"a\": 1\n}",
			func(v *Value) error { return v.Delete(mustPath("b")) }},
		{"delete last trailing", "{\n  \"a\": 1,\n  \"b\": 2, // two\n}", "{\n  \"a\": 1,\n}",
			func(v *Value) error { return v.Delete(mustPath("b")) }},
		{"delete only", "{\"a\": 1,}", "{}",
			func(v *Value) error { return v.Delete(mustPath("a")) }},
		{"array", "[1, 2, 3]", "[1, 3, 4]",
			func(v *Value) error {
				if err := v.Delete(mustPath("[1]")); err != nil {
					return err
				}
				return v.Set(mustPath("[2]"), []byte("4"))
			}},
		{"rename", "{\n  // c\n  \"old\": 1\n}", "{\n  // c\n  \"new\": 1\n}",
			func(v *Value) error { return v.Rename(mustPath("old"), "new") }},
		{"move", "{\n  \"a\": {\"b\": 1},\n  \"c\": 2\n}", "{\n  \"c\": 2,\n  \"d\": {\n    \"e\": {\"b\": 1}\n  }\n}",
			func(v *Value) error { return v.Move(mustPath("a"), mustPath("d.e")) }},
		{"root", " 1 ", " [] ", func(v *Value) error { return v.Set(nil, []byte("[]")) }},
	}
	for _, c := range cases {
		if got := edit(t, c.in, c.f); got != c.want {
			t.Errorf("%s:\ngot  %q\nwant %q", c.name, got, c.want)
		}
	}
}

func TestPatchErrors(t *testing.T) {
	v, _ := Parse([]byte(`{"a": 1, "b": [1]}`), nil)
	for _, err := range []error{
		v.Delete(mustPath("x")),
		v.Set(mustPath("a.b"), []byte("1")),
		v.Set(mustPath("b[5]"), []byte("1")),
		v.Rename(mustPath("x"), "y"),
	} {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v", err)
		}
	}
	if v.Rename(mustPath("a"), "b") == nil || v.Move(mustPath("b"), mustPath("b.c")) == nil || v.Set(mustPath("a"), []byte("{")) == nil {
		t.Error("bad edit accepted")
	}
}

// TestRoundTripProperty prints random documents with random whitespace,
// comments and trailing commas and checks Parse then Pack is the identity
// and Standardize keeps the meaning.
func TestRoundTripProperty(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	extras := []string{"", " ", "\n  ", "\t", "// c\n", "/* b */", " /* x\n y */ ", "\r\n"}
	ex := func() string { return extras[r.IntN(len(extras))] + extras[r.IntN(len(extras))] }
	lits := []string{`0`, `-1.5e+3`, `"s"`, `"é\"\\"`, `true`, `false`, `null`, `""`}
	var gen func(depth int, b *strings.Builder)
	gen = func(depth int, b *strings.Builder) {
		b.WriteString(ex())
		switch k := r.IntN(4); {
		case depth > 4 || k < 2:
			b.WriteString(lits[r.IntN(len(lits))])
		case k == 2:
			b.WriteByte('{')
			n := r.IntN(4)
			for i := range n {
				b.WriteString(ex() + `"k` + itoa(i) + `"` + ex() + ":")
				gen(depth+1, b)
				if i < n-1 || r.IntN(2) == 0 {
					b.WriteByte(',')
				}
			}
			b.WriteString(ex() + "}")
		default:
			b.WriteByte('[')
			n := r.IntN(4)
			for i := range n {
				gen(depth+1, b)
				if i < n-1 || r.IntN(2) == 0 {
					b.WriteByte(',')
				}
			}
			b.WriteString(ex() + "]")
		}
		b.WriteString(ex())
	}
	for range 2000 {
		var b strings.Builder
		gen(0, &b)
		in := b.String()
		v, err := Parse([]byte(in), nil)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got := string(v.Pack()); got != in {
			t.Fatalf("Pack = %q, want %q", got, in)
		}
		if std := v.Standardize(); len(std) != len(in) || !json.Valid(std) {
			t.Fatalf("Standardize(%q) = %q", in, std)
		}
	}
}

func TestEncodings(t *testing.T) {
	bomDoc := "\xEF\xBB\xBF{\r\n  \"a\": 1, // c\r\n}\r\n"
	crDoc := "{\r  // old Mac\r  \"a\": 1\r}\r"
	for _, in := range []string{bomDoc, crDoc, "\xEF\xBB\xBF1", "{\"a\":1}\r\n// x\r", "[1,\r\n2]"} {
		v, err := Parse([]byte(in), nil)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if string(v.Pack()) != in || !json.Valid(v.Standardize()) {
			t.Fatalf("%q round trip", in)
		}
	}
	// Positions: BOM ignored, CRLF and CR each one line.
	for _, c := range []struct {
		in        string
		line, col int
	}{
		{"\xEF\xBB\xBF{x}", 1, 2},
		{"{\r\n\r\n  x}", 3, 3},
		{"{\r\r  x}", 3, 3},
		{"{\r\n  // \xff\r\n}", 2, 6},
		{"{\"a\": \"\xc3\"}", 1, 8},
		{"{\xa1\xa1}", 1, 2},
	} {
		_, err := Parse([]byte(c.in), nil)
		var se *SyntaxError
		if !errors.As(err, &se) || se.Line != c.line || se.Column != c.col {
			t.Errorf("%q: %v, want %d:%d", c.in, err, c.line, c.col)
		}
	}
	if _, err := Parse([]byte("// \xd6\xd0\xce\xc4\n{}"), nil); !errors.Is(err, ErrNotUTF8) || !strings.Contains(err.Error(), "1:4: invalid UTF-8 byte 0xd6: the file is not UTF-8; save it as UTF-8") {
		t.Fatal(err)
	}
	if _, err := Parse([]byte("{} \xEF\xBB\xBF"), nil); err == nil {
		t.Fatal("BOM accepted after the start")
	}
	// Edits keep the document's line endings.
	for _, nl := range []string{"\r\n", "\r", "\n"} {
		in := strings.ReplaceAll("\xEF\xBB\xBF{\n  \"a\": 1 // c\n}\n", "\n", nl)
		got := edit(t, in, func(v *Value) error {
			if err := v.Set(mustPath("x.y"), []byte("{\n\"z\": 1}")); err != nil {
				return err
			}
			return v.Set(mustPath("b"), []byte("2"))
		})
		want := strings.ReplaceAll("\xEF\xBB\xBF{\n  \"a\": 1, // c\n  \"x\": {\n    \"y\": {\n\"z\": 1}\n  },\n  \"b\": 2\n}\n", "\n", nl)
		if got != want {
			t.Errorf("%q:\ngot  %q\nwant %q", nl, got, want)
		}
		if Newline([]byte(got)) != nl {
			t.Errorf("%q: mixed line endings", nl)
		}
	}
}

func TestLookalikes(t *testing.T) {
	for in, want := range map[string]string{
		"{\n  \"a\"：1}":        "2:6: full-width colon '：'; use ':'",
		"{\"a\": 1， \"b\": 2}": "1:8: full-width comma '，'; use ','",
		"{“a”: 1}":             "1:2: curly quote '“'; use '\"'",
		"｛}":                   "1:1: full-width brace '｛'; use '{'",
		"{\"a\":\u00a01}":      `1:6: a: non-breaking space '\u00a0'; use ' '`,
		"[1,\u30002]":          `1:4: [1]: full-width space '\u3000'; use ' '`,
		"[1】":                  "1:3: CJK bracket '】'; use ']'",
	} {
		_, err := Parse([]byte(in), nil)
		if err == nil || err.Error() != want {
			t.Errorf("%q: %v, want %s", in, err, want)
		}
	}
	if _, err := Parse([]byte(`{"a：b": "，"} // ：`), nil); err != nil {
		t.Fatal(err)
	}
}

func TestNeighbourComments(t *testing.T) {
	for _, c := range []struct {
		in, want string
		f        func(v *Value) error
	}{
		{"{\n  \"x\": 1, // about x\n  \"y\": 2,\n}\n", "{\n  \"x\": 1, // about x\n}\n", func(v *Value) error { return v.Delete(Path{"y"}) }},
		{"{\n  \"x\": 1, // about x\n  \"y\": 2\n}\n", "{\n  \"x\": 1 // about x\n}\n", func(v *Value) error { return v.Delete(Path{"y"}) }},
		{"{\n  \"x\": 1, // about x\n  \"y\": 2, // about y\n  \"z\": 3\n}", "{\n  \"x\": 1, // about x\n  \"z\": 3\n}", func(v *Value) error { return v.Delete(Path{"y"}) }},
		{"{ // head\n  \"x\": 1\n}", "{ // head\n}", func(v *Value) error { return v.Delete(Path{"x"}) }},
		{"{\n  \"x\": 1, // about x\n}\n", "{\n  \"x\": 1, // about x\n  \"y\": 2,\n}\n", func(v *Value) error { return v.Set(Path{"y"}, []byte("2")) }},
	} {
		if got := edit(t, c.in, c.f); got != c.want {
			t.Errorf("%q:\ngot  %q\nwant %q", c.in, got, c.want)
		}
	}
}

func TestNonASCIIError(t *testing.T) {
	if _, err := Parse([]byte("{中}"), nil); err == nil || !strings.Contains(err.Error(), "'中'") {
		t.Fatal(err)
	}
}

func TestLinearEdits(t *testing.T) {
	var b strings.Builder
	b.WriteString("{\n")
	for i := range 50000 {
		b.WriteString("  \"k" + itoa(i) + "\": 1,\n")
	}
	b.WriteString("}\n")
	v, err := Parse([]byte(b.String()), &Options{MaxSize: 1 << 22})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 50000 {
		if err := v.Set(Path{"k" + itoa(i)}, []byte("2")); err != nil {
			t.Fatal(err)
		}
		if err := v.Set(Path{"n" + itoa(i)}, []byte("3")); err != nil {
			t.Fatal(err)
		}
	}
	if v.Find(Path{"n49999"}) == nil {
		t.Fatal("missing")
	}
}
