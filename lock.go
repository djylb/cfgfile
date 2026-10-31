package cfgfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// lockPath is the advisory lock file for path: next to the file it guards
// (the target of a symbolic link), never removed, since removing it would
// let two writers lock different files.
func lockPath(path string) string {
	if t, err := filepath.EvalSymlinks(path); err == nil {
		path = t
	}
	return path + ".lock"
}

// lockWrites takes the advisory write lock for path if Options.LockWrites
// is set, waiting for other writers, and returns the function that
// releases it. Readers never take it. Where the platform has no advisory
// locks, or the lock file cannot be created because the directory is not
// writable (the write itself then fails), it does nothing.
func lockWrites(path string, o *Options) (func(), error) {
	if !o.LockWrites || !lockSupported {
		return func() {}, nil
	}
	f, err := os.OpenFile(lockPath(path), os.O_RDWR|os.O_CREATE, o.perm())
	if err != nil {
		if errors.Is(err, fs.ErrPermission) || readOnlyFS(err) {
			return func() {}, nil
		}
		return nil, err
	}
	if err := lockFile(f); err != nil {
		_ = f.Close() // the lock error is what matters
		return nil, err
	}
	return func() {
		// Closing the file releases the lock even if unlocking failed.
		_ = unlockFile(f)
		_ = f.Close()
	}, nil
}
