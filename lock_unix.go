//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package cfgfile

import (
	"errors"
	"os"
	"syscall"
)

func lockFile(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

func unlockFile(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }

func readOnlyFS(err error) bool { return errors.Is(err, syscall.EROFS) }
