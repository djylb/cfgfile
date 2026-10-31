package cfgfile_test

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/djylb/cfgfile"
	"github.com/djylb/cfgfile/jwcc"
)

type App struct {
	Name     string           `json:"name" doc:"service name"`
	DataDir  cfgfile.FilePath `json:"data_dir" doc:"data directory, relative to this file"`
	Dirs     []cfgfile.FilePath
	Password string           `json:"password" secret:"true"`
	Timeout  cfgfile.Duration `json:"timeout" env:"-"`
	Internal int              `json:"internal" flag:"-"`
	DB       struct {
		Host string `json:"host" doc:"database host"`
		Port int    `json:"port"`
	} `json:"db"`
}

func (a *App) Defaults() {
	a.Name = "app"
	a.Password = "hunter2"
	a.Timeout = cfgfile.Duration(5 * time.Second)
	a.DB.Port = 5432
}

func envOf(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestUnversioned(t *testing.T) {
	p := write(t, "{\"name\": \"x\"}")
	f, err := cfgfile.Open[App](p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.Value().Name != "x" {
		t.Fatal(f.Value())
	}
	c := f.Value()
	c.DB.Port = 1
	must(t, f.Save(&c))
	if got, _ := os.ReadFile(p); string(got) != "{\"name\": \"x\",\"db\": {\"port\": 1}}" {
		t.Fatalf("%q", got)
	}
	np := filepath.Join(t.TempDir(), "n.jsonc")
	nf, err := cfgfile.Open[App](np, nil)
	if err != nil {
		t.Fatal(err)
	}
	c = nf.Value()
	c.Name = "y"
	must(t, nf.Save(&c))
	if got, _ := os.ReadFile(np); string(got) != "{\n  \"name\": \"y\"\n}\n" {
		t.Fatalf("%q", got)
	}
}

func TestUnknownWarn(t *testing.T) {
	p := write(t, "{\"name\": \"x\",\n \"extra\": 1}")
	if _, err := cfgfile.Load[App](p, nil); !errors.Is(err, cfgfile.ErrUnknown) {
		t.Fatal(err)
	}
	r, err := cfgfile.Load[App](p, &cfgfile.Options{Unknown: cfgfile.UnknownWarn})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Warnings) != 1 || !errors.Is(r.Warnings[0], cfgfile.ErrUnknown) || !strings.Contains(r.Warnings[0].Error(), ":2:2: extra") {
		t.Fatal(r.Warnings)
	}
}

func TestFilePath(t *testing.T) {
	p := write(t, "{\"data_dir\": \"data\", \"Dirs\": [\"/abs\", \"rel\"]}")
	dir := filepath.Dir(p)
	f, err := cfgfile.Open[App](p, &cfgfile.Options{EnvPrefix: "A_", LookupEnv: envOf(nil)})
	if err != nil {
		t.Fatal(err)
	}
	c := f.Value()
	abs := cfgfile.FilePath("/abs")
	if runtime.GOOS == "windows" { // rooted: the root of the file's volume
		abs = cfgfile.FilePath(filepath.VolumeName(dir) + "/abs")
	}
	if c.DataDir != cfgfile.FilePath(filepath.Join(dir, "data")) || c.Dirs[0] != abs || c.Dirs[1] != cfgfile.FilePath(filepath.Join(dir, "rel")) {
		t.Fatalf("%+v", c)
	}
	// Saving an unchanged resolved path does not rewrite it.
	c.Name = "n"
	must(t, f.Save(&c))
	if got, _ := os.ReadFile(p); !strings.Contains(string(got), "\"data_dir\": \"data\"") {
		t.Fatalf("%s", got)
	}
	// Overrides stay relative to the working directory.
	r, err := cfgfile.Load[App](p, &cfgfile.Options{EnvPrefix: "A_", LookupEnv: envOf(map[string]string{"A_DATA_DIR": "x"})})
	if err != nil {
		t.Fatal(err)
	}
	if r.Value.DataDir != "x" {
		t.Fatal(r.Value.DataDir)
	}
}

func TestSecretsAndTags(t *testing.T) {
	dir := t.TempDir()
	pw := filepath.Join(dir, "pw")
	mustWrite(t, pw, []byte("s3cret\n"), 0o600)
	o := &cfgfile.Options{EnvPrefix: "A_", LookupEnv: envOf(map[string]string{"A_PASSWORD_FILE": pw, "A_TIMEOUT": "1s", "A_INTERNAL": "7"})}
	r, err := cfgfile.Load[App]("", o)
	if err != nil {
		t.Fatal(err)
	}
	// env:"-" keeps A_TIMEOUT out; flag:"-" does not affect the environment.
	if r.Value.Password != "s3cret" || r.Value.Timeout != cfgfile.Duration(5*time.Second) || r.Value.Internal != 7 {
		t.Fatalf("%+v", r.Value)
	}
	b, _ := json.Marshal(cfgfile.Redacted(&r.Value))
	if strings.Contains(string(b), "s3cret") || !strings.Contains(string(b), `"password":"[redacted]"`) {
		t.Fatalf("%s", b)
	}
	o.LookupEnv = envOf(map[string]string{"A_PASSWORD_FILE": pw, "A_PASSWORD": "x"})
	if _, err := cfgfile.Load[App]("", o); err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatal(err)
	}
	type sec struct {
		Key cfgfile.Duration `json:"key" secret:"true"`
	}
	_, err = cfgfile.Load[sec](write(t, `{"key": "topsecretvalue"}`), nil)
	if err == nil || strings.Contains(err.Error(), "topsecretvalue") || !strings.Contains(err.Error(), "secret value not shown") {
		t.Fatal(err)
	}
	_, err = cfgfile.Load[sec]("", &cfgfile.Options{EnvPrefix: "S_", LookupEnv: envOf(map[string]string{"S_KEY": "topsecretvalue"})})
	if err == nil || strings.Contains(err.Error(), "topsecretvalue") {
		t.Fatal(err)
	}
	flags := flag.NewFlagSet("x", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	must(t, cfgfile.DefineFlags[App](flags, nil))
	if flags.Lookup("internal") != nil || flags.Lookup("password").DefValue != "" || flags.Lookup("db.port").DefValue != "5432" || flags.Lookup("db.host").Usage != "database host" {
		t.Fatal("flag definitions")
	}
	if _, err := cfgfile.Load[App]("", &cfgfile.Options{Flags: flags}); err == nil {
		t.Fatal("unparsed flags accepted")
	}
	must(t, flags.Parse([]string{"-db.port=1", "-name", "n"}))
	r, err = cfgfile.Load[App]("", &cfgfile.Options{Flags: flags})
	if err != nil || r.Value.DB.Port != 1 || r.Value.Name != "n" || len(r.Overrides) != 2 {
		t.Fatal(err, r)
	}
}

func TestTemplate(t *testing.T) {
	b, err := cfgfile.Template[App](&cfgfile.Options{Version: 3})
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "version": 3,
  // service name
  "name": "app",
  // data directory, relative to this file
  "data_dir": "",
  "Dirs": null,
  "password": "",
  "timeout": "5s",
  "internal": 0,
  "db": {
    // database host
    "host": "",
    "port": 5432,
  },
}
`
	if string(b) != want {
		t.Fatalf("%s", b)
	}
	p := filepath.Join(t.TempDir(), "app.jsonc")
	must(t, cfgfile.CreateFile[App](p, &cfgfile.Options{Version: 3}))
	if err := cfgfile.CreateFile[App](p, nil); !errors.Is(err, fs.ErrExist) {
		t.Fatal(err)
	}
	if r, err := cfgfile.Load[App](p, &cfgfile.Options{Version: 3}); err != nil || r.Value.DB.Port != 5432 {
		t.Fatal(err)
	}
}

func TestLocate(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	mustWrite(t, filepath.Join(b, "app.jsonc"), []byte("{}"), 0o600)
	if p, err := cfgfile.Locate("app.jsonc", a, b); err != nil || p != filepath.Join(b, "app.jsonc") {
		t.Fatal(p, err)
	}
	if _, err := cfgfile.Locate("none.jsonc", a, b); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	if d := cfgfile.DefaultDirs("app"); len(d) < 1 || d[0] != "." {
		t.Fatal(d)
	}
}

func TestChangeOldNew(t *testing.T) {
	p := write(t, "{\"name\": \"a\"}")
	f, err := cfgfile.Open[App](p, nil)
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan cfgfile.Change[App], 1)
	w, err := f.Watch(context.Background(), cfgfile.WatchOptions[App]{Interval: 5 * time.Millisecond, Debounce: 5 * time.Millisecond,
		OnChange: func(c cfgfile.Change[App]) { ch <- c }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { w.Close(); <-w.Done() }()
	mustWrite(t, p, []byte("{\"name\": \"b\"}"), 0o600)
	select {
	case c := <-ch:
		if c.Old.Name != "a" || c.New.Name != "b" || f.Value().Name != "b" {
			t.Fatalf("%+v", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no change")
	}
}

func TestFinalReviewFixes(t *testing.T) {
	// M1: a secret number that does not fit is reported without its digits.
	type creds struct {
		Pin int `json:"pin"`
	}
	type sec struct {
		Pin   int   `json:"pin" secret:"true"`
		Creds creds `json:"creds" secret:"true"`
	}
	for _, in := range []string{`{"pin": 123456789012345678901234}`, `{"creds": {"pin": 123456789012345678901234}}`, `{"pin": "x"}`} {
		_, err := cfgfile.Load[sec](write(t, in), nil)
		if err == nil || strings.Contains(err.Error(), "1234567890") || !strings.Contains(err.Error(), "secret value not shown") {
			t.Errorf("%s: %v", in, err)
		}
	}
	// M2: nothing under a secret member reaches the template.
	b, err := cfgfile.Template[withCreds](nil)
	if err != nil || strings.Contains(string(b), "hunter2") || !strings.Contains(string(b), `"pass": ""`) {
		t.Fatalf("%v %s", err, b)
	}
	// L3: a bad secret flag value is not echoed by the flag package.
	flags := flag.NewFlagSet("x", flag.ContinueOnError)
	var out strings.Builder
	flags.SetOutput(&out)
	must(t, cfgfile.DefineFlags[sec](flags, nil))
	must(t, flags.Parse([]string{"-pin=topsecret"}))
	_, err = cfgfile.Load[sec]("", &cfgfile.Options{Flags: flags})
	if err == nil || strings.Contains(err.Error()+out.String(), "topsecret") {
		t.Fatal(err, out.String())
	}
	// L4: migrations without a version.
	if _, err := cfgfile.Load[App]("", &cfgfile.Options{Migrations: map[int]cfgfile.Migration{1: nil}}); err == nil {
		t.Fatal("migrations without version accepted")
	}
	// L5: NAME and OTHER_FILE collide.
	type clash struct {
		X  string `json:"x"`
		XF string `json:"x_file"`
	}
	if _, err := cfgfile.Fields[clash](&cfgfile.Options{EnvPrefix: "A_"}); err == nil || !strings.Contains(err.Error(), "A_X_FILE") {
		t.Fatal(err)
	}
}

// M3: the value in effect after a write is what was written, even if the
// disk changes right after; the outside change then shows as a conflict.
func TestWriteBaseline(t *testing.T) {
	p := write(t, "{\"name\": \"a\"}")
	f, err := cfgfile.Open[App](p, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := f.Value()
	c.Name = "b"
	must(t, f.Save(&c))
	mustWrite(t, p, []byte("{\"name\": \"outside\"}"), 0o600)
	if f.Value().Name != "b" {
		t.Fatal(f.Value().Name)
	}
	c.Name = "c"
	if err := f.Save(&c); !errors.Is(err, cfgfile.ErrConflict) {
		t.Fatal(err)
	}
}

type withCreds struct {
	Creds struct {
		Pass string `json:"pass"`
	} `json:"creds" secret:"true"`
}

func (w *withCreds) Defaults() { w.Creds.Pass = "hunter2" }

type tagged struct {
	Name  string `json:"name" required:"true"`
	Mode  string `json:"mode" enum:"fast,safe"`
	Level int    `json:"level" enum:"1,2,3"`
	Key   string `json:"key" secret:"true" enum:"a,b"`
	Sub   struct {
		Host string `json:"host" required:"true"`
	} `json:"sub" required:"true"`
}

func TestRequiredEnum(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"{\"sub\": {\"host\": \"h\"}}", ": name: required member missing"},
		{"{\"name\": \"n\",\n \"sub\": {}}", ":2:9: sub.host: required member missing"},
		{"{\"name\": \"n\", \"sub\": {\"host\": \"h\"},\n \"mode\": \"slow\"}", `:2:10: mode: value not allowed: "slow" is not one of fast, safe`},
		{"{\"name\": \"n\", \"sub\": {\"host\": \"h\"}, \"level\": 4}", "level: value not allowed"},
		{"{\"name\": \"n\", \"sub\": {\"host\": \"h\"}, \"key\": \"zzz\"}", "secret, not shown"},
	} {
		_, err := cfgfile.Load[tagged](write(t, c.in), nil)
		if err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(err.Error(), "zzz") {
			t.Errorf("%s: %v, want %q", c.in, err, c.want)
		}
	}
	if _, err := cfgfile.Load[tagged](write(t, "{\"name\": \"n\", \"sub\": {\"host\": \"h\"}, \"mode\": \"safe\", \"level\": 2}"), nil); err != nil {
		t.Fatal(err)
	}
	// The environment can satisfy required members, and is named in errors.
	o := &cfgfile.Options{EnvPrefix: "T_", LookupEnv: envOf(map[string]string{"T_NAME": "n", "T_SUB_HOST": "h", "T_MODE": "x"})}
	if _, err := cfgfile.Load[tagged]("", o); err == nil || !strings.Contains(err.Error(), "environment variable T_MODE: mode: value not allowed") {
		t.Fatal(err)
	}
	// Save refuses to write a value outside the enum.
	p := write(t, "{\"name\": \"n\", \"sub\": {\"host\": \"h\"}}")
	f, err := cfgfile.Open[tagged](p, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := f.Value()
	v.Mode = "slow"
	if err := f.Save(&v); !errors.Is(err, cfgfile.ErrEnum) {
		t.Fatal(err)
	}
}

func TestSource(t *testing.T) {
	p := write(t, "{\n  \"name\": \"x\",\n  \"db\": {\"port\": 1}\n}")
	pw := filepath.Join(t.TempDir(), "pw")
	mustWrite(t, pw, []byte("s"), 0o600)
	flags := flag.NewFlagSet("x", flag.ContinueOnError)
	must(t, cfgfile.DefineFlags[App](flags, nil))
	must(t, flags.Parse([]string{"-db.host=h"}))
	f, err := cfgfile.Open[App](p, &cfgfile.Options{Flags: flags, EnvPrefix: "A_", LookupEnv: envOf(map[string]string{"A_PASSWORD_FILE": pw})})
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"name":     p + ":2:11",
		"db.port":  p + ":3:18",
		"db":       p + ":3:9",
		"db.host":  "flag -db.host",
		"password": "environment variable A_PASSWORD_FILE",
		"timeout":  "default",
	} {
		jp, _ := jwcc.ParsePath(path)
		if got := f.Source(jp).String(); got != want {
			t.Errorf("%s: %s, want %s", path, got, want)
		}
	}
}
