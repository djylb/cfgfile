// Package cfgfile reads, writes and watches versioned configuration files
// written in JWCC (JSON with comments and trailing commas; see package
// jwcc), and rewrites them without losing the user's comments.
//
// Open loads a file into a struct and returns a File: Value is the
// configuration in effect, Save and Update write changes back, touching
// only what changed and refusing to overwrite edits made by someone else,
// and Watch reloads the file when it changes. Load is the read-only
// shortcut.
//
// Reading applies Defaults, the file (strictly decoded, migrated step by
// step on the syntax tree when versioned), environment variables and
// flags named after the file's paths, then Validate:
//
//	file path   log.level
//	flag        -log.level
//	environment APP_LOG_LEVEL (or APP_LOG_LEVEL_FILE)   with EnvPrefix "APP_"
//
// Values from flags and the environment only change the value in memory;
// Save refuses to write them (ErrOverridden) unless asked to.
//
// Files may be UTF-8 (with or without a byte order mark) or UTF-16 with a
// byte order mark, with LF, CRLF or CR line endings; rewrites keep the
// encoding, the byte order mark and the line endings. Other encodings are
// rejected with the position of the first byte that is not UTF-8.
//
// Includes and merging several files are not supported: a rewrite needs a
// single file that holds every value.
//
// The package has no global state. The only goroutine it starts is the
// one per File.Watch, which ends when its context is done or Close is
// called. All functions and File methods are safe for concurrent use.
package cfgfile
