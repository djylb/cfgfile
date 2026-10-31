//go:build windows

package cfgfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"time"
)

// openShared opens a file for reading with FILE_SHARE_DELETE besides read
// and write sharing, so that this package's own readers (Load, the
// watcher) never stop a writer from renaming a new version over the file.
func openShared(path string) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL|syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

const errorSharingViolation syscall.Errno = 32

// replaceRetry bounds how long replaceFile waits for other programs.
const replaceRetry = time.Second

// replaceFile renames from over to. Another program (an editor, a virus
// scanner) holding to open without FILE_SHARE_DELETE makes the rename
// fail; it is retried with a growing pause for about a second in all, then
// reported.
func replaceFile(from, to string) error {
	wait, waited := 5*time.Millisecond, time.Duration(0)
	for {
		err := os.Rename(from, to)
		if err == nil || !(errors.Is(err, syscall.ERROR_ACCESS_DENIED) || errors.Is(err, errorSharingViolation)) {
			return err
		}
		if waited >= replaceRetry {
			return fmt.Errorf("cfgfile: cannot replace %s, another program holds it open: %w", to, err)
		}
		time.Sleep(wait)
		waited += wait
		wait = min(2*wait, 200*time.Millisecond)
	}
}
