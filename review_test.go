package cfgfile_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/djylb/cfgfile"
	"github.com/djylb/cfgfile/jwcc"
)

type omit struct {
	Version int    `json:"version"`
	X       int    `json:"x,omitempty"`
	A       [2]int `json:"a"`
}

func (o *omit) Defaults() { o.X = 5 }

func TestSaveOmitemptyZero(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	mustWrite(t, p, []byte("{\"version\": 1, \"x\": 7}"), 0o600)
	o := &cfgfile.Options{Version: 1}
	f, err := cfgfile.Open[omit](p, o)
	if err != nil {
		t.Fatal(err)
	}
	v := f.Value()
	v.X = 0
	must(t, f.Save(&v))
	if r2, err := cfgfile.Load[omit](p, o); err != nil || r2.Value.X != 0 {
		b, _ := os.ReadFile(p)
		t.Fatalf("%v %q", err, b)
	}
}

func TestArrayOverflow(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	mustWrite(t, p, []byte("{\"version\": 1, \"a\": [1, 2, 3]}"), 0o600)
	_, err := cfgfile.Load[omit](p, &cfgfile.Options{Version: 1})
	if err == nil || !strings.Contains(err.Error(), "1:28: a[2]: more than 2 elements") {
		t.Fatal(err)
	}
}

func TestWriteFileSymlink(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "js" || runtime.GOOS == "wasip1" || runtime.GOOS == "plan9" {
		t.Skip("no symlinks")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "real.json")
	link := filepath.Join(dir, "link.json")
	mustWrite(t, target, []byte("{}"), 0o640)
	if err := os.Symlink("real.json", link); err != nil {
		t.Skip(err)
	}
	if err := cfgfile.WriteFile(link, []byte("{\"a\":1}"), 0o600); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Lstat(link)
	got, _ := os.ReadFile(target)
	if fi.Mode()&os.ModeSymlink == 0 || string(got) != "{\"a\":1}" {
		t.Fatalf("link replaced or target unchanged: %v %q", fi.Mode(), got)
	}
	if st, _ := os.Stat(target); st.Mode().Perm() != 0o640 {
		t.Fatal(st.Mode())
	}
}

func TestFileSizeLimit(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	mustWrite(t, p, []byte("{\"version\": 1, \"x\": \""+strings.Repeat("a", 3000)+"\"}"), 0o600)
	o := &cfgfile.Options{Version: 1, Limits: &jwcc.Limits{MaxSize: 1000}}
	if _, err := cfgfile.Load[omit](p, o); !errors.Is(err, jwcc.ErrTooLarge) {
		t.Fatal(err)
	}
}

type inner1 struct {
	Q int `json:"A"`
	B int
}
type inner2 struct {
	A int
	B int
}
type conflict struct {
	Version int `json:"version"`
	inner1
	inner2
}

func TestFieldConflicts(t *testing.T) {
	// "A": tagged in inner1 only, so it wins; "B": untagged twice, dropped.
	fs, err := cfgfile.Fields[conflict](&cfgfile.Options{Version: 1, EnvPrefix: "X_"})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range fs {
		names = append(names, f.Flag)
	}
	if strings.Join(names, ",") != "A" {
		t.Fatal(names)
	}
	p := filepath.Join(t.TempDir(), "c.json")
	mustWrite(t, p, []byte("{\"version\": 1, \"B\": 1}"), 0o600)
	if _, err := cfgfile.Load[conflict](p, &cfgfile.Options{Version: 1}); err == nil {
		t.Fatal("ambiguous member accepted")
	}
}

func TestWriteReloadError(t *testing.T) {
	o := opts()
	o.EnvPrefix = "APP_"
	var bad atomic.Bool
	o.LookupEnv = func(k string) (string, bool) {
		if k == "APP_LOG_LEVEL" && bad.Load() {
			return "loud", true
		}
		return "", false
	}
	f, err := cfgfile.Open[Config](write(t, v2), o)
	if err != nil {
		t.Fatal(err)
	}
	bad.Store(true)
	c := f.Value()
	c.DataDir = "/x"
	if err := f.Save(&c); err == nil || !strings.Contains(err.Error(), "does not load") {
		t.Fatal(err)
	}
	if f.Value().DataDir == "/x" {
		t.Fatal("unloadable value put in effect")
	}
	c.DataDir = "/y"
	if err := f.Save(&c); !errors.Is(err, cfgfile.ErrConflict) {
		t.Fatal("stale state not reported:", err)
	}
	bad.Store(false)
	must(t, f.Reload())
	if f.Value().DataDir != "/x" {
		t.Fatal(f.Value().DataDir)
	}
	c = f.Value()
	c.DataDir = "/y"
	if err := f.Save(&c); err != nil {
		t.Fatal(err)
	}
	if got := f.Value().DataDir; got != "/y" {
		t.Fatal(got)
	}
}
