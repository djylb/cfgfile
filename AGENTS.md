# AGENTS.md

- Before you finish, `gofmt -l .` must print nothing and `go vet ./...`,
  `go test -race ./...`, `GOOS=js GOARCH=wasm go vet ./...` and the js/wasm
  tests (`GOOS=js GOARCH=wasm go test ./...` with
  `$(go env GOROOT)/lib/wasm` on `PATH`) must pass, and the fuzz targets
  (`FuzzParse`, `FuzzPatch`, `FuzzFormat` in `jwcc`) must survive a short run. Give every
  test run a `-timeout` and every fuzz run a `-fuzztime`; add
  `-fuzzminimizetime 0` on loaded hosts (minimising looks like a stall).
- Standard library only, also for tests; `deps_test.go` checks it.
- `jwcc` stays usable on its own and must not import the root package.
- Format must stay idempotent and keep the decoded value.
- Parse then Pack must return the input byte for byte; Standardize must keep
  the length. Edits must leave bytes they do not touch unchanged.
- Parsing stays bounded by `Options` (depth and size); add no unbounded
  recursion or input-sized allocation beyond the input itself.
- No global mutable state; `Options` are only read. The only goroutine is
  the one per `Watcher` (documented in `Watch`); start no other. Never hold
  a lock of this package while running a callback.
- Rewrites keep the file's encoding, byte order mark and line endings;
  never replace undecodable bytes.
- File writes go through `WriteFile` (temp file, fsync, rename, directory
  fsync); never write a configuration file in place.
- The README, docs and commits name no other project and quote no benchmark
  numbers.
- Commit subjects are short, English and imperative.
- Never commit host names, IP addresses or credentials.
- Loops that retry non-blocking calls (tests included) must yield, e.g. with
  `runtime.Gosched()`: js/wasm has no preemption, so a spinning goroutine
  starves every other one, and `go test -timeout` cannot fire.
