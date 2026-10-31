# cfgfile

Versioned configuration files for Go, written in JWCC (JSON with comments
and trailing commas), with comment-preserving rewrites. Standard library
only.

- `jwcc`: parser and printer that keep every byte (comments, whitespace,
  trailing commas), positions as line, column and JSON path, `Standardize`
  for strict JSON of equal length, and edits by path (`Set`, `Delete`,
  `Rename`, `Move`) that leave everything else untouched. Usable on its own.
- `cfgfile`: `Load[T]` with strict decoding, version checks, step
  migrations on the syntax tree, optional write-back with a backup,
  environment and flag overlay, and validation; `Update` and `Save` for
  safe program rewrites.
- `cmd/cfgfile-schema`: JSON Schema from struct tags for editors.
- `cmd/cfgfile-fmt`: canonical formatting (`jwcc.Format`), with `-w`, `-l`,
  `-d`, `-check` and `-strict` (standard JSON); `cfgfile.FormatFile` offers
  the same to programs, for a `config fmt` subcommand.

## Loading

```go
type Config struct {
	Version int    `json:"version"`
	DataDir string `json:"data_dir" doc:"data directory"`
	Log     struct {
		Level string `json:"level" enum:"debug,info,warn,error"`
	} `json:"log"`
	Timeout cfgfile.Duration `json:"timeout"`
}

func (c *Config) Defaults()       { c.Log.Level = "info" }
func (c *Config) Validate() error { return nil }

opts := &cfgfile.Options{
	Version:   2,
	EnvPrefix: "APP_",
	Persist:   true,
	Migrations: map[int]cfgfile.Migration{
		1: func(doc *jwcc.Value) error { return doc.Rename(jwcc.Path{"datadir"}, "data_dir") },
	},
}
opts.Flags, _ = cfgfile.BindFlags[Config](flag.CommandLine, opts)
flag.Parse()
res, err := cfgfile.Load[Config]("app.jsonc", opts)
```

A missing file is not an error: the result holds defaults plus overrides.
`Load` rejects unknown members, `null` for non-pointer fields, members that
match a field only case-insensitively, and files whose version is newer
than `Options.Version` (`ErrTooNew`). Errors carry file, line, column and
path: `app.jsonc:4:5: log.levle: unknown member`.

## Versions and migrations

The top-level integer `version` (name configurable) starts at 1 and is
required; a file without it is rejected with a message naming the member
to add.
`Migrations[N]` turns version N into N+1 on the syntax tree; `Load` then
sets the version. With `Persist`, a migrated file is written back: the
original is first copied to `app.jsonc.v1.bak` (an existing backup is never
replaced), then the file is replaced atomically. If writing fails, for
example on a read-only file system, `Load` still returns the migrated
configuration and reports `ErrNotPersisted` in `Result.Warnings`.

## Overlay mapping

Every leaf of the struct can also come from a flag and an environment
variable whose names are derived from its path in the file:

| file path | flag | environment (`EnvPrefix: "APP_"`) |
|---|---|---|
| `data_dir` | `-data_dir` | `APP_DATA_DIR` |
| `log.level` | `-log.level` | `APP_LOG_LEVEL` |
| `admin.bootstrap.admin_user` | `-admin.bootstrap.admin_user` | `APP_ADMIN_BOOTSTRAP_ADMIN_USER` |

Path elements are json tag names; embedded structs are flattened; `.`
and `-` become `_` in variable names, which are upper case. Leaves are
non-struct fields and types that decode themselves; slices and maps are
leaves. Strings are taken verbatim, `encoding.TextUnmarshaler` types go
through `UnmarshalText`, and everything else is read as JSON (`APP_PORTS=[80,443]`).
Precedence: flags, environment, file, `Defaults`. Overrides are never
written to the file: `Result.Snapshot.Overrides` lists the overridden
paths, and `Save` keeps the file's value there (remove a path from the list
to write it). `Fields[T]` lists the mapping.

## Rewriting

`Result.Snapshot` records the size and SHA-256 of the file as read.
`Update(path, snap, opts, fn)` re-reads the file, fails with `ErrConflict`
if it changed, applies `fn` to the syntax tree, checks the result decodes
and validates, and writes it atomically (temporary file, fsync, rename,
directory fsync; the file mode is kept). `Save` derives the edits from a
changed struct and touches only members whose value changed; fields are
compared one by one, so a zero value the caller set is written even with
`omitempty`.

`WriteFile` replaces a symbolic link's target and keeps the link; a hard
link is broken (the path gets a new file). If only the final directory
sync fails, the file has been replaced: the error wraps `ErrNotDurable`
and `Update`/`Save` still return the new snapshot. Files larger than the
parse limit (`Limits.MaxSize`, default 1 MiB; twice that for UTF-16) are
not read.

The conflict check and the rename are not one atomic step: a writer that
does not use this package can still slip in between and be overwritten.
There is no file lock. The replaced file keeps its mode but not its owner;
run the writer as the owning user.

## Encodings and line endings

- UTF-8 with or without a byte order mark; the mark is kept on rewrite and
  does not count in columns.
- UTF-16 LE or BE with a byte order mark: transcoded to UTF-8 for reading
  (positions refer to the UTF-8 text; `Result.Text` is UTF-8) and written
  back as UTF-16 with the same mark.
- Anything else, such as legacy 8-bit encodings, is rejected at the first
  invalid byte: `app.jsonc:2:6: invalid UTF-8 byte 0xd6: the file is not
  UTF-8; save it as UTF-8`. Bytes are never replaced silently.
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

```go
w, err := cfgfile.Watch(ctx, "app.jsonc", opts, cfgfile.WatchOptions[Config]{
	OnChange: func(c cfgfile.Change[Config]) { apply(c.Result.Value, c.Changed) },
	OnError:  func(err error) { log.Print(err) },
}, res)
// on SIGHUP: w.Reload()
```

The watcher polls every `Interval` (default 2s): a change of size,
modification time or file identity (atomic rename), or a modification time
recent enough that a coarse clock could hide a write, leads to a content
hash, and only new content is loaded, after `Debounce` (default 200ms) of
quiet. A reload runs the full `Load` pipeline; migrations are written back
only with `Persist`. Invalid files and removal (`ErrRemoved`) go to
`OnError` and the previous value stays in effect; a valid value that
differs from the previous one goes to `OnChange` with the top-level members
that changed. Which members can be applied without a restart is the
caller's decision. `Watcher.Save` and `Watcher.Update` write without
triggering a reload.

Each watcher runs exactly one goroutine; callbacks run on it one at a time.
`Close` is idempotent and does not block; after it returns no new callback
starts, and `Done` is closed once the goroutine (and a callback that was
already running) has finished. Cancelling the context has the same effect.

## Schema

```go
//go:generate go run github.com/djylb/cfgfile/cmd/cfgfile-schema -type Config -o app.schema.json
```

Point the file's `"$schema"` member at the published schema. The schema
serves editors only; `Load` does not read it.

## Concurrency

No global state; the only goroutine is one per `Watcher`. All functions
are safe for concurrent use;
a `*jwcc.Value` may be read concurrently but needs exclusive access to edit.

## License

Apache-2.0
