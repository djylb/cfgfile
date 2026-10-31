package jwcc

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestFormat(t *testing.T) {
	in := "\xEF\xBB\xBF// head\r\n{\"version\":1,   // v\r\n\r\n\r\n  // about a\r\n      \"a\" :{\"b\":[1,2 ,3],\"c\":[ {\"d\":null} ],},\r\n\"e\":[],\"f\":{ /* empty */ }, \"g\": [1, // one\r\n 2]}\r\n"
	want := "\xEF\xBB\xBF// head\r\n{\r\n  \"version\": 1, // v\r\n\r\n  // about a\r\n  \"a\": {\r\n    \"b\": [1, 2, 3],\r\n    \"c\": [\r\n      {\r\n        \"d\": null,\r\n      },\r\n    ],\r\n  },\r\n  \"e\": [],\r\n  \"f\": {\r\n    /* empty */\r\n  },\r\n  \"g\": [\r\n    1, // one\r\n    2,\r\n  ],\r\n}\r\n"
	v, err := Parse([]byte(in), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := string(Format(v, nil))
	if got != want {
		t.Fatalf("got\n%q\nwant\n%q", got, want)
	}
	tabs := string(Format(v, &FormatOptions{Indent: "\t", Trailing: TrailingRemove, Newline: "\n"}))
	if !strings.Contains(tabs, "\n\t\"a\": {\n\t\t\"b\": [1, 2, 3],\n\t\t\"c\": [\n\t\t\t{\n\t\t\t\t\"d\": null\n\t\t\t}\n\t\t]\n\t},") || strings.Contains(tabs, "\r") {
		t.Fatalf("%s", tabs)
	}
	keep := string(Format(v, &FormatOptions{Trailing: TrailingKeep, Newline: "\n"}))
	if !strings.Contains(keep, "\"c\": [\n      {\n        \"d\": null\n      }\n    ],\n  },\n") {
		t.Fatalf("%s", keep)
	}
	checkFormat(t, []byte(in))
}

func decodeAny(t *testing.T, std []byte) any {
	var x any
	dec := json.NewDecoder(bytes.NewReader(std))
	dec.UseNumber()
	if err := dec.Decode(&x); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return x
}

// checkFormat checks that Format is idempotent and keeps the value, for
// every policy.
func checkFormat(t *testing.T, data []byte) {
	t.Helper()
	v, err := Parse(data, nil)
	if err != nil {
		return
	}
	want := decodeAny(t, v.Standardize())
	for _, o := range []*FormatOptions{nil, {Trailing: TrailingKeep}, {Trailing: TrailingRemove, Indent: "\t", Newline: "\r"}} {
		once := Format(v, o)
		w, err := Parse(once, &Limits{MaxDepth: 1 << 20, MaxSize: 1 << 30})
		if err != nil {
			t.Fatalf("Format produced invalid JWCC: %v\n%q\nfrom %q", err, once, data)
		}
		if got := decodeAny(t, w.Standardize()); !reflect.DeepEqual(got, want) {
			t.Fatalf("Format changed the value:\n%q\nfrom %q", once, data)
		}
		if a, b := commentSet(v), commentSet(w); !slices.Equal(a, b) {
			t.Fatalf("Format changed the comments:\n%q\n%q\nfrom %q", a, b, data)
		}
		if twice := Format(w, o); !bytes.Equal(once, twice) {
			t.Fatalf("Format not idempotent:\n%q\n%q\nfrom %q", once, twice, data)
		}
	}
}

func TestFormatProperty(t *testing.T) {
	for _, s := range []string{sample, "1", "// c\n1 // d\n/* e */", "[[],[[]],{}]", "{\"a\" /*x*/ : /*y*/ 1 /*z*/ , /*w*/ }", "[1 // a\n,2]"} {
		checkFormat(t, []byte(s))
	}
}

func FuzzFormat(f *testing.F) {
	for _, s := range []string{sample, "\xEF\xBB\xBF{\r\n  \"a\": 1, // c\r\n}\r\n", "{\r  // cr\r  \"a\": [1,\r]\r}\r", "[1 // a\n,2]", "{\"a\" /*x*/ : /*y*/ 1 /*z*/ , /*w*/ }", "// a\n\n\n// b\n[]\n\n// c"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) { checkFormat(t, data) })
}

// commentSet returns the comments of a document, normalised as Format
// writes them, sorted.
func commentSet(v *Value) []string {
	f := &formatter{nl: "\n"}
	var out []string
	add := func(e []byte) {
		cs, _ := f.comments(e)
		for _, c := range cs {
			out = append(out, string(c.text))
		}
	}
	var walk func(v *Value)
	walk = func(v *Value) {
		add(v.Before)
		add(v.After)
		switch {
		case v.Obj != nil:
			add(v.Obj.Last)
			for i := range v.Obj.Members {
				add(v.Obj.Members[i].Name.Before)
				add(v.Obj.Members[i].Name.After)
				walk(&v.Obj.Members[i].Value)
			}
		case v.Arr != nil:
			add(v.Arr.Last)
			for i := range v.Arr.Elems {
				walk(&v.Arr.Elems[i])
			}
		}
	}
	walk(v)
	slices.Sort(out)
	return out
}

// subset reports whether sorted multiset a is contained in sorted b.
func subset(a, b []string) bool {
	j := 0
	for _, x := range a {
		for j < len(b) && b[j] < x {
			j++
		}
		if j == len(b) || b[j] != x {
			return false
		}
		j++
	}
	return true
}

func TestFormatCommentAlias(t *testing.T) {
	for _, in := range []string{
		"[1 /*a*/\n/*p*/, /*b*/ 2]",
		"{\"x\": 1 /*a*/\n/*p*/, /*b*/ \"y\": 2}",
	} {
		checkFormat(t, []byte(in))
		v, _ := Parse([]byte(in), nil)
		out := string(Format(v, nil))
		for _, c := range []string{"/*a*/", "/*p*/", "/*b*/"} {
			if strings.Count(out, c) != 1 {
				t.Errorf("%q -> %q: %s count %d", in, out, c, strings.Count(out, c))
			}
		}
	}
}
