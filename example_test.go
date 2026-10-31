package cfgfile_test

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/djylb/cfgfile"
	"github.com/djylb/cfgfile/jwcc"
)

type ExampleConfig struct {
	Listen string `json:"listen" doc:"address to listen on"`
	Log    struct {
		Level string `json:"level" doc:"debug, info, warn or error"`
	} `json:"log"`
}

func (c *ExampleConfig) Defaults() {
	c.Listen = "127.0.0.1:8080"
	c.Log.Level = "info"
}

func (c *ExampleConfig) Validate() error {
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
		return nil
	}
	return fmt.Errorf("log.level: unknown level %q", c.Log.Level)
}

// Open a file, change a value and save it: comments and layout stay.
func ExampleOpen() {
	dir, _ := os.MkdirTemp("", "example")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "app.jsonc")
	_ = os.WriteFile(path, []byte(`{
  "version": 1,
  // reachable from the LAN
  "listen": "0.0.0.0:8080",
}
`), 0o600)

	f, err := cfgfile.Open[ExampleConfig](path, &cfgfile.Options{Version: 1})
	if err != nil {
		fmt.Println(err)
		return
	}
	cfg := f.Value()
	fmt.Println(cfg.Listen, cfg.Log.Level)

	cfg.Log.Level = "debug"
	if err := f.Save(&cfg); err != nil {
		fmt.Println(err)
		return
	}
	b, _ := os.ReadFile(path)
	fmt.Print(string(b))
	// Output:
	// 0.0.0.0:8080 info
	// {
	//   "version": 1,
	//   // reachable from the LAN
	//   "listen": "0.0.0.0:8080",
	//   "log": {
	//     "level": "debug"
	//   },
	// }
}

// Flags and environment variables named after the file's paths override
// it in memory only.
func ExampleDefineFlags() {
	fs := flag.NewFlagSet("app", flag.ContinueOnError)
	config := fs.String("config", "app.jsonc", "configuration file")
	o := &cfgfile.Options{EnvPrefix: "APP_", Flags: fs}
	if err := cfgfile.DefineFlags[ExampleConfig](fs, o); err != nil {
		fmt.Println(err)
		return
	}
	_ = fs.Parse([]string{"-log.level=warn"})
	r, err := cfgfile.Load[ExampleConfig](filepath.Join(os.TempDir(), "missing", *config), o)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(r.Exists, r.Value.Log.Level, r.Overrides)
	// Output: false warn [log.level]
}

// A migration renames a member of version 1 on the syntax tree.
func ExampleMigration() {
	dir, _ := os.MkdirTemp("", "example")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "app.jsonc")
	_ = os.WriteFile(path, []byte(`{"version": 1, "addr": ":80"}`), 0o600)

	o := &cfgfile.Options{
		Version: 2,
		Migrations: map[int]cfgfile.Migration{
			1: func(doc *jwcc.Value) error { return doc.Rename(jwcc.Path{"addr"}, "listen") },
		},
		Persist: true,
	}
	r, err := cfgfile.Load[ExampleConfig](path, o)
	if err != nil {
		fmt.Println(err)
		return
	}
	b, _ := os.ReadFile(path)
	fmt.Println(r.Value.Listen, r.Persisted, string(b))
	// Output: :80 true {"version": 2, "listen": ":80"}
}

// Watch delivers valid changes; invalid edits keep the previous value.
func ExampleFile_Watch() {
	dir, _ := os.MkdirTemp("", "example")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "app.jsonc")
	_ = os.WriteFile(path, []byte(`{"listen": ":80"}`), 0o600)

	f, err := cfgfile.Open[ExampleConfig](path, nil)
	if err != nil {
		fmt.Println(err)
		return
	}
	changes := make(chan cfgfile.Change[ExampleConfig], 1)
	w, err := f.Watch(context.Background(), cfgfile.WatchOptions[ExampleConfig]{
		Interval: 10 * time.Millisecond,
		OnChange: func(c cfgfile.Change[ExampleConfig]) { changes <- c },
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	defer w.Close()
	_ = os.WriteFile(path, []byte(`{"listen": ":81"}`), 0o600)
	c := <-changes
	fmt.Println(c.Old.Listen, "->", c.New.Listen, c.Changed)
	// Output: :80 -> :81 [listen]
}

// Template writes every member with its default and documentation.
func ExampleTemplate() {
	b, _ := cfgfile.Template[ExampleConfig](&cfgfile.Options{Version: 1})
	fmt.Print(string(b))
	// Output:
	// {
	//   "version": 1,
	//   // address to listen on
	//   "listen": "127.0.0.1:8080",
	//   "log": {
	//     // debug, info, warn or error
	//     "level": "info",
	//   },
	// }
}
