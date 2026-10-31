# cfgfile

Versioned configuration files for Go, written in JWCC (JSON with comments
and trailing commas), that programs can rewrite without losing the
user's comments. Standard library only, besides
[timeshim](https://github.com/djylb/timeshim) for the watcher's injectable
clock.

- `cfgfile`: `Open[T]` reads a file into a struct (strict decoding,
  versions and migrations, environment and flag overrides, validation) and
  saves changes back atomically, touching only what changed; `Watch`
  reloads it on change. `Load[T]` is the read-only shortcut.
- `jwcc`: parser and printer that keep every byte, error positions as
  line, column and JSON path, edits by path (`Set`, `Delete`, `Rename`,
  `Move`) and a canonical formatter. Usable on its own.
- `cmd/cfgfile-schema`: JSON Schema from struct tags, for editors.
- `cmd/cfgfile-fmt`: formatting with `-w`, `-l`, `-d`, `-check`, and
  `-strict` for plain JSON; `cfgfile.Format` and `cfgfile.ToJSON` offer the
  same to programs (for example for a `config fmt` subcommand).

## A complete program

First run, read, modify, save and watch:

```go
// Command app shows the whole cycle: find or create the file, read it with
// flags and environment on top, change a value, save it, and watch it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/djylb/cfgfile"
)

type Config struct {
	Listen  string           `json:"listen" doc:"address to listen on"`
	DataDir cfgfile.FilePath `json:"data_dir" doc:"data directory, relative to this file"`
	Token   string           `json:"token" secret:"true" doc:"API token (or APP_TOKEN_FILE)"`
	Log     struct {
		Level string `json:"level" doc:"debug, info, warn or error"`
	} `json:"log"`
}

func (c *Config) Defaults() {
	c.Listen = "127.0.0.1:8080"
	c.DataDir = "data"
	c.Log.Level = "info"
}

func (c *Config) Validate() error {
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
		return nil
	}
	return fmt.Errorf("log.level: unknown level %q", c.Log.Level)
}

func main() {
	opts := &cfgfile.Options{Version: 1, EnvPrefix: "APP_", Flags: flag.CommandLine}
	path := flag.String("config", "", "configuration file")
	if err := cfgfile.DefineFlags[Config](flag.CommandLine, opts); err != nil {
		log.Fatal(err)
	}
	flag.Parse()

	// First run: find the file, or write a commented template.
	if *path == "" {
		p, err := cfgfile.Locate("app.jsonc", cfgfile.DefaultDirs("app")...)
		if errors.Is(err, fs.ErrNotExist) {
			p = "app.jsonc"
			if err := cfgfile.CreateFile[Config](p, opts); err != nil {
				log.Fatal(err)
			}
		} else if err != nil {
			log.Fatal(err)
		}
		*path = p
	}

	// Read: defaults, file, environment, flags, Validate.
	f, err := cfgfile.Open[Config](*path, opts)
	if err != nil {
		log.Fatal(err) // e.g. app.jsonc:4:5: log.levle: unknown member
	}
	for _, w := range f.Result().Warnings {
		log.Print(w)
	}
	cfg := f.Value()
	log.Printf("config: %+v", cfgfile.Redacted(&cfg))

	// Modify and save: only the changed member is rewritten.
	cfg.Listen = "127.0.0.1:9090"
	if err := f.Save(&cfg); err != nil {
		log.Fatal(err) // ErrConflict, ErrOverridden, validation errors
	}

	// Watch for edits; SIGHUP forces a reload.
	w, err := f.Watch(context.Background(), cfgfile.WatchOptions[Config]{
		OnChange: func(c cfgfile.Change[Config]) { log.Printf("changed: %v", c.Changed) },
		OnError:  func(err error) { log.Print(err) },
	})
	if err != nil {
		log.Fatal(err)
	}
	defer w.Close()
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP, os.Interrupt)
	for s := range hup {
		if s == os.Interrupt {
			return
		}
		w.Reload()
	}
}
```

## Traps

- **Values from flags and the environment are never written to the file.**
  Changing one in memory and calling `Save` fails with `ErrOverridden`
  naming the paths; use `SaveOverridden` to write them on purpose.
- **Someone else edited the file:** `Save` and `Update` fail with
  `ErrConflict` instead of overwriting; call `Reload` and retry. Two
  `File`s on the same path conflict the same way; share one `File`.
- **A versioned file needs its version member.** With `Options.Version` set
  a file without `"version"` is rejected with a message saying what to add.
  Leave `Version` at 0 for files that are never migrated.
- **Defaults are not written by `Save`**, only changes; use `Template` or
  `CreateFile` for a file with every member and its documentation.
- **Relative paths** are relative to the working directory unless the
  field is a `cfgfile.FilePath`, which is resolved against the file's
  directory.
- **No includes, no multi-file merging.** A rewrite needs one file that is
  the single source of every value; merged files would make it ambiguous
  which file a change belongs in.
- `Save` writes new members in the file's style: the indentation unit of
  the surrounding members (else the file's first indentation, else two
  spaces) and the trailing-comma style of the enclosing object; objects go
  one member per line, arrays of plain values stay on one line.

## Reading

`Open` and `Load` apply, in order: `Defaults()`, the file, environment
variables, flags, `Validate()`. A missing file is not an error. Decoding is
strict: unknown members (or, with `Unknown: UnknownWarn`, a warning per
member), `null` for non-pointer fields, case-only matches and extra
elements of fixed-size arrays are rejected. Errors carry file, line, column
and path: `app.jsonc:4:5: log.levle: unknown member`. Files larger than
`Limits.MaxSize` (default 1 MiB; twice that for UTF-16) are not read.

## Versions and migrations

With `Options.Version` set, the top-level integer `version` (name
configurable) is required, a newer file is refused (`ErrTooNew`), and
`Migrations[N]` turns version N into N+1 on the syntax tree, so it can
handle members the struct no longer has; the version is then updated.
With `Persist`, a migrated file is written back after copying the original
to `app.jsonc.v1.bak` (never replaced). If that fails, for example on a
read-only file system, the migrated value is still used and
`ErrNotPersisted` is reported in `Result.Warnings`.

## Flags and environment

Every leaf of the struct can come from a flag and an environment variable
named after its path in the file:

| file path | flag | environment (`EnvPrefix: "APP_"`) |
|---|---|---|
| `data_dir` | `-data_dir` | `APP_DATA_DIR` or `APP_DATA_DIR_FILE` |
| `log.level` | `-log.level` | `APP_LOG_LEVEL` |
| `admin.bootstrap.user` | `-admin.bootstrap.user` | `APP_ADMIN_BOOTSTRAP_USER` |

Path elements are json names; embedded structs are flattened; `.` and `-`
become `_` in variable names, which are upper case. `NAME_FILE` reads the
value from a file (for secrets mounted as files). Leaves are non-struct
fields and types that decode themselves; slices and maps are leaves and are
written as JSON (`APP_PORTS=[80,443]`); strings are taken verbatim and
`encoding.TextUnmarshaler` types use `UnmarshalText`. Precedence: flags,
environment, file, `Defaults`.

`DefineFlags[T](fs, opts)` defines the flags with the defaults as default
values and the `doc` tag (or the type) as usage. Set `Options.Flags` to the
`FlagSet`; after `Parse`, the flags that were given override the rest.
Tags: `flag:"-"` and `env:"-"` exclude a field; `secret:"true"` keeps its
value out of errors, flag defaults, templates and `Redacted` (a rejected
secret flag value is reported by `Load` without the value, not by the flag
package).

## Validation tags and sources

Beside `Validate()`, two tags are checked on load and emitted in the
schema, so that editors and the program agree: `required:"true"` (the
file, a flag or the environment must set the member; `ErrRequired`) and
`enum:"a,b,c"` (`ErrEnum`, also checked before `Save` writes). Errors carry
the position in the file or the flag or variable that set the value; a
secret value is never shown.

`File.Source(path)` (and `Result.Source`) tells where a value came from:
`app.jsonc:4:5`, `environment variable APP_TOKEN_FILE`, `flag -log.level`
or `default`. It never includes the value.

## Saving

`File.Save(&v)` compares `v` with what the file holds field by field
(ignoring `omitempty`, so zero values are written) and edits only the
members that differ; everything else, comments included, stays.
`File.Update(fn)` edits the syntax tree directly. Both validate the result
before writing, write atomically (temporary file in the same directory,
fsync, rename, directory fsync; the mode is kept; a symbolic link's target
is replaced and the link kept; a hard link is broken), and then read the
file back. If only the directory sync fails the file is written and the
error wraps `ErrNotDurable`. The conflict check and the rename are not one
atomic step; to close that window between cooperating processes, set
`LockWrites`: every write then holds an advisory lock on `<file>.lock`
(flock on Linux, macOS and the BSDs, `LockFileEx` on Windows, nothing
elsewhere or when the directory is read-only). Readers never take it, and
the lock file is left in place.

## Encodings and line endings

- UTF-8 with or without a byte order mark; the mark is kept on rewrite and
  does not count in columns.
- UTF-16 LE or BE with a byte order mark: transcoded to UTF-8 for reading
  (positions refer to the UTF-8 text; `Result.Text` is UTF-8) and written
  back as UTF-16 with the same mark.
- Anything else, such as legacy 8-bit encodings, is rejected at the first
  invalid byte: `app.jsonc:2:6: invalid UTF-8 byte 0xd6: the file is not
  UTF-8; save it as UTF-8`. Bytes are never replaced silently.
- The dialect is JWCC: comments and trailing commas, nothing else.
  Duplicate member names are rejected, a line comment may end at the end
  of the input, and a lone CR ends a line comment as it ends a line in
  editors.
- LF, CRLF and a lone CR all end a line. Edits write new line breaks in the
  file's dominant style, so a file never ends up mixed. Files created by
  `Update`/`Save` use LF, or CRLF with `Options.CRLF`.

Characters that input methods put in place of JSON punctuation are
rejected by name: `3:7: full-width colon '：'; use ':'` (also full-width
commas, braces, brackets, curly quotes, non-breaking and full-width
spaces).

## Formatting

`jwcc.Format` writes one member per line with consistent indentation (two
spaces or `Indent`), `"name": value`, at most one blank line between
members, and comments kept beside their members and re-indented. Member
order, literal text, the byte order mark and the line-ending style are
kept. Trailing commas follow `Trailing`: added to every multi-line
container (default), kept as written, or removed. Arrays of plain values
written on one line stay on one line. Formatting is idempotent and never
changes the decoded value.

## Hot reload

`File.Watch(ctx, opts)` polls every `Interval` (default 2s). A change of
size, modification time or file identity (atomic rename), or a
modification time recent enough that a coarse clock could hide a write,
leads to a content hash, and only new content is loaded, after `Debounce`
(default 200ms) of quiet. A reload runs the whole read pipeline; invalid
files and removal (`ErrRemoved`) go to `OnError` and keep the previous
value; a different valid value becomes `File.Value` and goes to `OnChange`
with the old and new values and the top-level members that changed.
`Clock` (a `timeshim.Clock`, default `timeshim.Real`) schedules the polls,
so tests can drive them with a `timeshim.Fake`; the recent-modification
check always compares file times with real time. Which
members can be applied without a restart is the caller's decision. The
`File`'s own `Save` and `Update` do not cause callbacks. `Watcher.Reload`
forces a reload, for example on SIGHUP.

Each watch runs exactly one goroutine, which ends when the context is done
or `Close` is called; callbacks run on it one at a time. `Close` is
idempotent and does not block; after it returns no new callback starts, and
`Done` is closed once the goroutine (and a callback already running) has
finished.

## Finding the file

`DefaultDirs("app")` lists, in order of precedence, the working directory,
the user's configuration directory (`os.UserConfigDir`) and the system
directory (`/etc/app`; also `/Library/Application Support/app` on macOS;
`%ProgramData%\app` on Windows). `Locate(name, dirs...)` returns the first
that exists. A path given explicitly (`-config`) should win over all.

## Schema

```go
//go:generate go run github.com/djylb/cfgfile/cmd/cfgfile-schema -type Config -o app.schema.json
```

Point the file's `"$schema"` member at the published schema. The schema
serves editors only; `Load` does not read it.

## Concurrency

No global state; the only goroutine is the one per watch. All functions
and `File` methods are safe for concurrent use; writes through one `File`
are serialised.

## License

Apache-2.0
