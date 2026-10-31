package cfgfile_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/djylb/cfgfile"
	"github.com/djylb/cfgfile/jwcc"
)

func toUTF16(s string, le bool) []byte {
	var b []byte
	for _, u := range append([]uint16{0xFEFF}, utf16.Encode([]rune(s))...) {
		if le {
			b = append(b, byte(u), byte(u>>8))
		} else {
			b = append(b, byte(u>>8), byte(u))
		}
	}
	return b
}

func TestUTF16(t *testing.T) {
	src := strings.Replace(v2, "where data lives", "数据目录 😀", 1)
	for _, le := range []bool{true, false} {
		p := filepath.Join(t.TempDir(), "app.jsonc")
		os.WriteFile(p, toUTF16(src, le), 0o644)
		o := opts()
		r, err := cfgfile.Load[Config](p, o)
		if err != nil {
			t.Fatal(err)
		}
		if r.Value.DataDir != "/var/lib/app" || string(r.Text) != src {
			t.Fatalf("%+v", r.Value)
		}
		c := r.Value
		c.Log.Level = "warn"
		if _, err := cfgfile.Save(p, r.Snapshot, &c, o); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(p)
		if want := toUTF16(strings.Replace(src, `"debug"`, `"warn"`, 1), le); !bytes.Equal(got, want) {
			t.Fatalf("le=%v: saved %q", le, got)
		}
	}
	// Migration and write-back keep UTF-16 too.
	p := filepath.Join(t.TempDir(), "app.jsonc")
	os.WriteFile(p, toUTF16(v1, true), 0o644)
	o := opts()
	o.Persist = true
	if r, err := cfgfile.Load[Config](p, o); err != nil || !r.Persisted {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if !bytes.Equal(got, toUTF16(strings.NewReplacer(`"version": 1`, `"version": 2`, `"datadir"`, `"data_dir"`, `"addr"`, `"listen"`).Replace(v1), true)) {
		t.Fatalf("%q", got)
	}
	if bak, _ := os.ReadFile(cfgfile.BackupPath(p, 1)); !bytes.Equal(bak, toUTF16(v1, true)) {
		t.Fatal("backup not byte-identical")
	}
}

func TestBadEncodings(t *testing.T) {
	for _, c := range []struct {
		data []byte
		want string
	}{
		{[]byte("{\"version\": 2,\n  // \xd6\xd0\xce\xc4\n}"), ":2:6: invalid UTF-8 byte 0xd6: the file is not UTF-8; save it as UTF-8"},
		{[]byte("{\"version\": 2, \"data_dir\": \"caf\xe9\"}"), "1:32: data_dir: invalid UTF-8 byte 0xe9"},
		{toUTF16("{}", true)[2:], "UTF-16 without a byte order mark"},
		{append(toUTF16("{}", true), 0), "odd number"},
		{append(toUTF16("{", true), 0x00, 0xD8, '}', 0), "invalid UTF-16"},
	} {
		p := filepath.Join(t.TempDir(), "app.jsonc")
		os.WriteFile(p, c.data, 0o644)
		_, err := cfgfile.Load[Config](p, opts())
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: %v, want %q", c.data, err, c.want)
		}
	}
	p := filepath.Join(t.TempDir(), "app.jsonc")
	os.WriteFile(p, []byte("\xff\xff"), 0o644)
	if _, err := cfgfile.Load[Config](p, opts()); err == nil {
		t.Fatal("garbage accepted")
	}
	os.WriteFile(p, []byte("{\"version\": 2, // \xa1\n}"), 0o644)
	if _, err := cfgfile.Load[Config](p, opts()); !errors.Is(err, jwcc.ErrNotUTF8) {
		t.Fatal(err)
	}
}

func TestLineEndingsAndBOM(t *testing.T) {
	for _, nl := range []string{"\r\n", "\r"} {
		in := "\xEF\xBB\xBF" + strings.ReplaceAll(v1, "\n", nl)
		p := write(t, in)
		o := opts()
		o.Persist = true
		r, err := cfgfile.Load[Config](p, o)
		if err != nil || !r.Persisted {
			t.Fatal(err)
		}
		c := r.Value
		c.Log.Format = "json"
		if _, err := cfgfile.Save(p, r.Snapshot, &c, o); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(p)
		want := "\xEF\xBB\xBF" + strings.ReplaceAll(strings.NewReplacer(`"version": 1`, `"version": 2`, `"datadir"`, `"data_dir"`, `"addr"`, `"listen"`).Replace(v1), "\n", nl)
		want = strings.Replace(want, "},"+nl+"}", "},"+nl+"  \"log\": {"+nl+"    \"format\": \"json\""+nl+"  },"+nl+"}", 1)
		if string(got) != want {
			t.Fatalf("%q:\ngot  %q\nwant %q", nl, got, want)
		}
	}
	p := filepath.Join(t.TempDir(), "new.jsonc")
	o := opts()
	o.CRLF = true
	c := Config{DataDir: "/d"}
	c.Defaults()
	if _, err := cfgfile.Save(p, cfgfile.Snapshot{}, &c, o); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); string(got) != "{\r\n  \"version\": 2,\r\n  \"data_dir\": \"/d\"\r\n}\r\n" {
		t.Fatalf("%q", got)
	}
}
