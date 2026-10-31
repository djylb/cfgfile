package cfgfile_test

import (
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/djylb/cfgfile"
	"github.com/djylb/cfgfile/jwcc"
)

type Bootstrap struct {
	AdminUser string `json:"admin_user"`
}

type Admin struct {
	Listen    string     `json:"listen" doc:"admin listen address"`
	Bootstrap *Bootstrap `json:"bootstrap,omitempty"`
}

type Log struct {
	Level  string `json:"level" enum:"debug,info,warn,error"`
	Format string `json:"format"`
}

type Config struct {
	Version int               `json:"version"`
	DataDir string            `json:"data_dir"`
	Admin   Admin             `json:"admin"`
	Log     Log               `json:"log"`
	Timeout cfgfile.Duration  `json:"timeout"`
	Limit   cfgfile.Size      `json:"limit"`
	Ports   []int             `json:"ports,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"`
}

func (c *Config) Defaults() {
	c.Log.Level = "info"
	c.Timeout = cfgfile.Duration(30 * time.Second)
}

func (c *Config) Validate() error {
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
		return nil
	}
	return errors.New("bad log level " + c.Log.Level)
}

const v2 = `{
  "$schema": "x.json",
  "version": 2,
  // where data lives
  "data_dir": "/var/lib/app",
  "admin": {
    "listen": "127.0.0.1:8081", // local only
  },
  "log": {"level": "debug"},
  "limit": "64MiB",
}
`

// v1 had "datadir" at the top and "admin.addr".
const v1 = `{
  "version": 1,
  // where data lives
  "datadir": "/var/lib/app",
  "admin": {
    "addr": "127.0.0.1:8081", // local only
  },
}
`

func opts() *cfgfile.Options {
	return &cfgfile.Options{
		Version: 2,
		Migrations: map[int]cfgfile.Migration{
			1: func(doc *jwcc.Value) error {
				if err := doc.Rename(jwcc.Path{"datadir"}, "data_dir"); err != nil {
					return err
				}
				return doc.Rename(jwcc.Path{"admin", "addr"}, "listen")
			},
		},
		LookupEnv: func(string) (string, bool) { return "", false },
	}
}

func write(t *testing.T, s string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "app.jsonc")
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad(t *testing.T) {
	r, err := cfgfile.Load[Config](write(t, v2), opts())
	if err != nil {
		t.Fatal(err)
	}
	c := r.Value
	if c.DataDir != "/var/lib/app" || c.Admin.Listen != "127.0.0.1:8081" || c.Log.Level != "debug" ||
		c.Timeout != cfgfile.Duration(30*time.Second) || c.Limit != 64<<20 || r.Migrated || c.Version != 2 {
		t.Fatalf("%+v", r)
	}
}

func TestNoFile(t *testing.T) {
	o := opts()
	o.EnvPrefix = "APP_"
	env := map[string]string{"APP_LOG_LEVEL": "warn", "APP_ADMIN_BOOTSTRAP_ADMIN_USER": "root", "APP_PORTS": "[1,2]", "APP_TIMEOUT": "5s"}
	o.LookupEnv = func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := cfgfile.DefineFlags[Config](fs, o); err != nil {
		t.Fatal(err)
	}
	if f := fs.Lookup("timeout"); f == nil || f.DefValue != "30s" || f.Usage != "duration" {
		t.Fatalf("%+v", f)
	}
	if f := fs.Lookup("admin.listen"); f.Usage != "admin listen address" {
		t.Fatalf("%+v", f)
	}
	if err := fs.Parse([]string{"-log.level=error", "-limit=1KiB"}); err != nil {
		t.Fatal(err)
	}
	if fs.Parse([]string{"-limit=lots"}) == nil {
		t.Fatal("bad flag value accepted")
	}
	o.Flags = fs
	r, err := cfgfile.Load[Config](filepath.Join(t.TempDir(), "none.jsonc"), o)
	if err != nil {
		t.Fatal(err)
	}
	c := r.Value
	if r.Exists || c.Log.Level != "error" || c.Admin.Bootstrap.AdminUser != "root" || len(c.Ports) != 2 ||
		c.Timeout != cfgfile.Duration(5*time.Second) || c.Limit != 1024 {
		t.Fatalf("%+v", c)
	}
	env["APP_LOG_LEVEL"] = "loud"
	o.Flags = nil
	if _, err := cfgfile.Load[Config]("", o); err == nil || !strings.Contains(err.Error(), "loud") {
		t.Fatal(err)
	}
}

func TestFields(t *testing.T) {
	fs, err := cfgfile.Fields[Config](&cfgfile.Options{Version: 1, EnvPrefix: "APP_"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range fs {
		got = append(got, f.Flag+"="+f.Env)
	}
	want := "data_dir=APP_DATA_DIR admin.listen=APP_ADMIN_LISTEN admin.bootstrap.admin_user=APP_ADMIN_BOOTSTRAP_ADMIN_USER log.level=APP_LOG_LEVEL log.format=APP_LOG_FORMAT timeout=APP_TIMEOUT limit=APP_LIMIT ports=APP_PORTS labels=APP_LABELS"
	if strings.Join(got, " ") != want {
		t.Fatal(got)
	}
	type clash struct {
		A struct {
			B int `json:"b_c"`
		} `json:"a"`
		AB struct {
			C int `json:"c"`
		} `json:"a_b"`
	}
	if _, err := cfgfile.Fields[clash](&cfgfile.Options{EnvPrefix: "X_"}); err == nil {
		t.Fatal("clash not detected")
	}
}

func TestStrictErrors(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"{\"version\": 2,\n  \"log\": {\"levle\": 1}}", ":2:11: log.levle: unknown member"},
		{"{\"version\": 2,\n  \"Data_Dir\": \"x\"}", ":2:3: Data_Dir: unknown member"},
		{"{\"version\": 2,\n  \"log\": null}", ":2:10: log: null is not allowed"},
		{"{\"version\": 2,\n  \"ports\": [1, \"x\"]}", ":2:16: ports[1]: cannot use string"},
		{"{\"version\": 2,\n  \"timeout\": \"soon\"}", ":2:14: timeout: time: invalid duration"},
		{"{\"version\": 2,\n  \"log\": {\"level\": 1,}", ":2:23: unexpected end"},
		{"{\"version\": 3}", "newer"},
		{"{\"version\": \"2\"}", "no valid version"},
		{"[]", "not an object"},
		{"{\"version\": 2, \"log\": {\"level\": \"x\"}}", "bad log level"},
	} {
		_, err := cfgfile.Load[Config](write(t, c.in), opts())
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: err = %v, want %q", c.in, err, c.want)
		}
	}
	_, err := cfgfile.Load[Config](write(t, `{"version": 3}`), opts())
	if !errors.Is(err, cfgfile.ErrTooNew) {
		t.Fatal(err)
	}
	o := opts()
	o.Migrations = nil
	if _, err := cfgfile.Load[Config](write(t, v1), o); !errors.Is(err, cfgfile.ErrNoMigration) {
		t.Fatal(err)
	}
}

func TestMigratePersist(t *testing.T) {
	p := write(t, v1)
	o := opts()
	o.Persist = true
	r, err := cfgfile.Load[Config](p, o)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Migrated || !r.Persisted || r.FileVersion != 1 || r.Value.DataDir != "/var/lib/app" || len(r.Warnings) > 0 {
		t.Fatalf("%+v", r)
	}
	got, _ := os.ReadFile(p)
	want := strings.NewReplacer(`"version": 1`, `"version": 2`, `"datadir"`, `"data_dir"`, `"addr"`, `"listen"`).Replace(v1)
	if string(got) != want {
		t.Fatalf("migrated file:\n%s", got)
	}
	bak, _ := os.ReadFile(cfgfile.BackupPath(p, 1))
	if string(bak) != v1 {
		t.Fatal("backup")
	}
	if st, _ := os.Stat(p); runtime.GOOS != "windows" && st.Mode().Perm() != 0o644 {
		t.Fatal("mode not kept", st.Mode())
	}
	// A second migration of another v1 file keeps the first backup.
	mustWrite(t, p, []byte(strings.Replace(v1, "/var", "/srv", 1)), 0o644)
	if _, err := cfgfile.Load[Config](p, o); err != nil {
		t.Fatal(err)
	}
	if bak, _ = os.ReadFile(cfgfile.BackupPath(p, 1)); string(bak) != v1 {
		t.Fatal("backup overwritten")
	}
}

func TestReadOnlyFallback(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "js" || runtime.GOOS == "wasip1" || runtime.GOOS == "plan9" || os.Getuid() == 0 {
		t.Skip("directory permissions not enforced")
	}
	p := write(t, v1)
	dir := filepath.Dir(p)
	must(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { must(t, os.Chmod(dir, 0o700)) })
	o := opts()
	o.Persist = true
	r, err := cfgfile.Load[Config](p, o)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Migrated || r.Persisted || len(r.Warnings) != 1 || !errors.Is(r.Warnings[0], cfgfile.ErrNotPersisted) || r.Value.Admin.Listen == "" {
		t.Fatalf("%+v", r)
	}
	if got, _ := os.ReadFile(p); string(got) != v1 {
		t.Fatal("file changed")
	}
}

func TestUpdateSave(t *testing.T) {
	p := write(t, v2)
	o := opts()
	f, err := cfgfile.Open[Config](p, o)
	if err != nil {
		t.Fatal(err)
	}
	c := f.Value()
	c.Log.Level = "warn"
	c.Labels = map[string]string{"a": "b"}
	if err := f.Save(&c); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	want := strings.Replace(v2, `{"level": "debug"}`, `{"level": "warn"}`, 1)
	want = strings.Replace(want, "  \"limit\": \"64MiB\",\n", "  \"limit\": \"64MiB\",\n  \"labels\": {\n    \"a\": \"b\",\n  },\n", 1)
	if string(got) != want {
		t.Fatalf("saved:\n%s\nwant:\n%s", got, want)
	}
	if f.Value().Log.Level != "warn" {
		t.Fatal("value in effect not updated")
	}
	if err := f.Update(func(doc *jwcc.Value) error {
		return doc.SetValue(jwcc.Path{"log", "level"}, "nope")
	}); err == nil {
		t.Fatal("invalid update written")
	}
	if err := f.Update(func(doc *jwcc.Value) error {
		return doc.Delete(jwcc.Path{"labels"})
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); string(got) != strings.Replace(v2, `"debug"`, `"warn"`, 1) {
		t.Fatalf("%s", got)
	}
	// Someone else edits the file: Save refuses, Reload then Save works.
	mustWrite(t, p, []byte(strings.Replace(v2, `"debug"`, `"error"`, 1)), 0o644)
	c = f.Value()
	c.DataDir = "/d"
	if err := f.Save(&c); !errors.Is(err, cfgfile.ErrConflict) {
		t.Fatal(err)
	}
	must(t, f.Reload())
	c = f.Value()
	c.DataDir = "/d"
	must(t, f.Save(&c))
	if got := f.Value(); got.DataDir != "/d" || got.Log.Level != "error" {
		t.Fatalf("%+v", got)
	}
	// Creating a file.
	np := filepath.Join(t.TempDir(), "new.jsonc")
	nf, err := cfgfile.Open[Config](np, o)
	if err != nil {
		t.Fatal(err)
	}
	if nf.Result().Exists {
		t.Fatal("exists")
	}
	c = nf.Value()
	c.DataDir = "/d"
	must(t, nf.Save(&c))
	if got, _ := os.ReadFile(np); string(got) != "{\n  \"version\": 2,\n  \"data_dir\": \"/d\"\n}\n" {
		t.Fatalf("%q", got)
	}
}

func TestConcurrentLoad(t *testing.T) {
	p := write(t, v2)
	o := opts()
	o.EnvPrefix = "APP_"
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 20 {
				if _, err := cfgfile.Load[Config](p, o); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
}

func TestSize(t *testing.T) {
	for in, want := range map[string]cfgfile.Size{"0": 0, "12": 12, "1KB": 1000, "2KiB": 2048, "3 GiB": 3 << 30, "5B": 5} {
		var s cfgfile.Size
		if err := s.UnmarshalText([]byte(in)); err != nil || s != want {
			t.Errorf("%q: %v %v", in, s, err)
		}
	}
	for _, in := range []string{"", "x", "1XB", "-1", "16384PiB"} {
		var s cfgfile.Size
		if s.UnmarshalText([]byte(in)) == nil {
			t.Errorf("%q accepted", in)
		}
	}
	for s, want := range map[cfgfile.Size]string{0: "0B", 1000: "1000B", 1 << 20: "1MiB", 3 << 40: "3TiB"} {
		if b, _ := s.MarshalText(); string(b) != want {
			t.Errorf("%d: %s", s, b)
		}
	}
}

func TestSaveKeepsOverrides(t *testing.T) {
	p := write(t, v2)
	o := opts()
	o.EnvPrefix = "APP_"
	env := map[string]string{"APP_LOG_LEVEL": "error", "APP_ADMIN_BOOTSTRAP_ADMIN_USER": "root", "APP_DATA_DIR": "/tmp/x"}
	o.LookupEnv = func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	f, err := cfgfile.Open[Config](p, o)
	if err != nil {
		t.Fatal(err)
	}
	if r := f.Result(); len(r.Overrides) != 3 || r.Value.Log.Level != "error" {
		t.Fatalf("%+v", r.Overrides)
	}
	c := f.Value()
	c.Log.Format = "json"
	must(t, f.Save(&c))
	got, _ := os.ReadFile(p)
	if want := strings.Replace(v2, `{"level": "debug"}`, `{"level": "debug","format": "json"}`, 1); string(got) != want {
		t.Fatalf("saved:\n%s\nwant:\n%s", got, want)
	}
	// Changing an overridden value is an error unless asked for.
	c = f.Value()
	c.DataDir = "/new"
	if err := f.Save(&c); !errors.Is(err, cfgfile.ErrOverridden) || !strings.Contains(err.Error(), "data_dir") {
		t.Fatal(err)
	}
	must(t, f.SaveOverridden(&c))
	if got, _ := os.ReadFile(p); !strings.Contains(string(got), `"data_dir": "/new"`) || strings.Contains(string(got), "root") || strings.Contains(string(got), "error") {
		t.Fatalf("%s", got)
	}
}

func TestNoVersionMessage(t *testing.T) {
	_, err := cfgfile.Load[Config](write(t, `{"data_dir": "x"}`), opts())
	if !errors.Is(err, cfgfile.ErrNoVersion) || !strings.Contains(err.Error(), `add a top-level "version" member`) || !strings.Contains(err.Error(), "version 2") {
		t.Fatal(err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path string, data []byte, perm os.FileMode) {
	t.Helper()
	must(t, os.WriteFile(path, data, perm))
}
