// Package cfgfile loads versioned configuration files written in JWCC
// (JSON with comments and trailing commas; see package jwcc) and rewrites
// them without losing comments.
//
// Load decodes a file into a struct strictly, migrates older versions step
// by step on the syntax tree, optionally writes the migrated file back
// (with a backup) and overlays environment variables and flags whose names
// are derived from the same struct tags as the file paths:
//
//	file path   log.level
//	flag        -log.level
//	environment APP_LOG_LEVEL   (EnvPrefix "APP_")
//
// Precedence is flags, then environment, then file, then Defaults. The
// overlay only changes the value in memory.
//
// Update and Save rewrite a file through patches, refuse to overwrite a
// file that changed since it was read (ErrConflict), and replace it
// atomically.
//
// Watch reloads a file when it changes (polling, debounced) and delivers
// valid new values to a callback.
//
// Files may be UTF-8 (with or without a byte order mark) or UTF-16 with a
// byte order mark, with LF, CRLF or CR line endings; rewrites keep the
// encoding, the byte order mark and the line endings. Other encodings are
// rejected with the position of the first byte that is not UTF-8.
//
// The package has no global state. The only goroutine it starts is the
// one per Watcher, which ends when the Watcher's context is done or Close
// is called (see Watcher.Done). All functions are safe for concurrent use.
// Options are only read.
package cfgfile
