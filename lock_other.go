//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd || windows)

package cfgfile

import "os"

// No advisory locks here (for example Solaris, AIX, Plan 9, js/wasm):
// LockWrites does nothing.
const lockSupported = false

func lockFile(*os.File) error   { return nil }
func unlockFile(*os.File) error { return nil }
func readOnlyFS(error) bool     { return false }
