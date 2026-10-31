//go:build !windows

package cfgfile

import "os"

// openShared opens a file for reading. Elsewhere than Windows an open file
// never stops a rename over it.
func openShared(path string) (*os.File, error) { return os.Open(path) }

// replaceFile renames from over to.
func replaceFile(from, to string) error { return os.Rename(from, to) }
