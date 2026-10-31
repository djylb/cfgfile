package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const messy = "{\"a\":1, // one\n\"b\":[1,2]}\n"
const tidy = "{\n  \"a\": 1, // one\n  \"b\": [1, 2],\n}\n"

func cli(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func TestCLI(t *testing.T) {
	if code, out, _ := cli(t, messy); code != 0 || out != tidy {
		t.Fatalf("%d %q", code, out)
	}
	if code, out, _ := cli(t, messy, "-strict"); code != 0 || out != "{\n  \"a\": 1,\n  \"b\": [\n    1,\n    2\n  ]\n}\n" {
		t.Fatalf("%d %q", code, out)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "a.jsonc")
	q := filepath.Join(dir, "b.jsonc")
	mustWrite(t, p, []byte(messy), 0o644)
	mustWrite(t, q, []byte(tidy), 0o644)
	if code, out, _ := cli(t, "", "-check", p, q); code != 1 || out != p+"\n" {
		t.Fatalf("%d %q", code, out)
	}
	if code, out, _ := cli(t, "", "-d", p); code != 0 || !strings.Contains(out, "-{\"a\":1, // one\n") || !strings.Contains(out, "+  \"a\": 1, // one\n") {
		t.Fatalf("%d %q", code, out)
	}
	if code, _, _ := cli(t, "", "-w", p); code != 0 {
		t.Fatal(code)
	}
	if got, _ := os.ReadFile(p); string(got) != tidy {
		t.Fatalf("%q", got)
	}
	if code, out, _ := cli(t, "", "-check", p, q); code != 0 || out != "" {
		t.Fatalf("%d %q", code, out)
	}
	if code, _, errs := cli(t, "{\"a\"：1}"); code != 1 || !strings.Contains(errs, "full-width colon") {
		t.Fatalf("%d %q", code, errs)
	}
	if code, out, _ := cli(t, "{\r\n\"a\":[1,]}", "-tabs", "-trailing", "remove"); code != 0 || out != "{\r\n\t\"a\": [\r\n\t\t1\r\n\t]\r\n}\r\n" {
		t.Fatalf("%d %q", code, out)
	}
	if code, _, _ := cli(t, "", "-trailing", "x"); code != 2 {
		t.Fatal(code)
	}
}

func mustWrite(t *testing.T, path string, data []byte, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, perm); err != nil {
		t.Fatal(err)
	}
}
