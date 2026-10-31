package jwcc

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"
)

func FuzzParse(f *testing.F) {
	for _, s := range []string{sample, `{"a":[1,{"b":null}],}`, "// c\n1", "[/**/]", `"\ud800"`, `{"a":1,"a":2}`, "\xEF\xBB\xBF{\r\n  \"a\": 1, // c\r\n}\r\n", "{\r  // cr\r  \"a\": [1,\r]\r}\r", "// \xd6\xd0\n{}"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		v, err := Parse(data, nil)
		if err != nil {
			return
		}
		if !bytes.Equal(v.Pack(), data) {
			t.Fatalf("round trip changed input")
		}
		std := v.Standardize()
		if len(std) != len(data) {
			t.Fatal("Standardize changed length")
		}
		if !json.Valid(std) {
			t.Fatalf("Standardize produced invalid JSON: %q", std)
		}
	})
}

// FuzzPatch applies a sequence of edits chosen by the input and checks the
// document stays valid and untouched members keep their text.
func FuzzPatch(f *testing.F) {
	f.Add([]byte(sample), "log.level", "admin", uint8(0))
	f.Add([]byte(`[1,2,]`), "[1]", "[0]", uint8(7))
	f.Add([]byte("{\n  \"a\": 1 // x\n}"), "b.c", "a", uint8(3))
	f.Add([]byte("\xEF\xBB\xBF{\r\n  \"a\": 1 // x\r\n}\r\n"), "b.c", "a", uint8(0))
	f.Add([]byte("{\r  \"a\": [1,\r  ],\r}"), "a[1]", "a[0]", uint8(0x31))
	f.Fuzz(func(t *testing.T, data []byte, p1, p2 string, op uint8) {
		v, err := Parse(data, nil)
		if err != nil {
			return
		}
		a, err1 := ParsePath(p1)
		b, err2 := ParsePath(p2)
		if err1 != nil || err2 != nil {
			return
		}
		before := commentSet(v)
		onlyRename := true
		for i := range 3 {
			if (op>>(2*i))&3 != 2 {
				onlyRename = false
			}
			switch (op >> (2 * i)) & 3 {
			case 0:
				_ = v.Set(a, []byte(`{"k": [1, "s"]}`))
			case 1:
				_ = v.Delete(b)
			case 2:
				if len(a) > 0 {
					_ = v.Rename(a, p2)
				}
			case 3:
				_ = v.Move(b, a)
			}
			a, b = b, a
		}
		out := v.Pack()
		if in := styles(data); len(in) == 1 {
			if got := styles(out); len(got) > 1 || len(got) == 1 && !got[Newline(data)] {
				t.Fatalf("line endings mixed: %v -> %v\n%q", in, got, out)
			}
		}
		w, err := Parse(out, &Limits{MaxDepth: 1 << 20, MaxSize: 1 << 30})
		if err != nil {
			t.Fatalf("edits produced invalid JWCC: %v\n%q", err, out)
		}
		after := commentSet(w)
		if !subset(after, before) || onlyRename && !slices.Equal(after, before) {
			t.Fatalf("comments changed: %q -> %q\n%q", before, after, out)
		}
		if !bytes.Equal(w.Pack(), out) {
			t.Fatal("round trip after edit")
		}
		if !json.Valid(w.Standardize()) {
			t.Fatal("standardized edit invalid")
		}
	})
}

// styles returns the line endings used in b outside strings (strings
// cannot hold raw line breaks).
func styles(b []byte) map[string]bool {
	m := map[string]bool{}
	for i := 0; i < len(b); i++ {
		switch b[i] {
		case '\n':
			m["\n"] = true
		case '\r':
			if i+1 < len(b) && b[i+1] == '\n' {
				m["\r\n"] = true
				i++
			} else {
				m["\r"] = true
			}
		}
	}
	return m
}
