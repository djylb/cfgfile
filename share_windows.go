//go:build windows

package cfgfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"
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

// replaceFile renames from over to. It uses a POSIX-semantics rename
// (FileRenameInfoEx, Windows 10 1607 and later on NTFS and ReFS), which
// replaces to even while readers hold it open with FILE_SHARE_DELETE, as
// this package's readers do; they keep reading the old content. Where
// that is not supported it falls back to MoveFileEx. Another program (an
// editor, a virus scanner) holding to open without FILE_SHARE_DELETE makes
// the rename fail; it is retried with a growing pause for about a second
// in all, then reported.
func replaceFile(from, to string) error {
	wait, waited := 5*time.Millisecond, time.Duration(0)
	for {
		err := posixRename(from, to)
		if errors.Is(err, errNoPosixRename) {
			err = os.Rename(from, to)
		}
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

var (
	procSetFileInformationByHandle = kernel32.NewProc("SetFileInformationByHandle")
	errNoPosixRename               = errors.New("POSIX rename not supported")
)

const (
	fileRenameInfoEx              = 22 // FILE_INFO_BY_HANDLE_CLASS
	fileRenameFlagReplaceIfExists = 0x1
	fileRenameFlagPosixSemantics  = 0x2
	accessDelete                  = 0x00010000 // DELETE
	errorInvalidParameter         = syscall.Errno(87)
	errorNotSupported             = syscall.Errno(50)
	errorInvalidFunction          = syscall.Errno(1)
)

// fileRenameInfo is FILE_RENAME_INFO; FileName continues past the struct.
type fileRenameInfo struct {
	Flags          uint32 // union with ReplaceIfExists
	RootDirectory  syscall.Handle
	FileNameLength uint32 // in bytes, without the terminating NUL
	FileName       [1]uint16
}

// posixRename renames from over to through a handle to from. It returns
// an error wrapping errNoPosixRename where the system or file system
// cannot do it.
func posixRename(from, to string) error {
	abs, err := filepath.Abs(to)
	if err != nil {
		return err
	}
	name, err := syscall.UTF16FromString(abs)
	if err != nil {
		return err
	}
	if procSetFileInformationByHandle.Find() != nil {
		return errNoPosixRename
	}
	p, err := syscall.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	h, err := syscall.CreateFile(p, accessDelete|syscall.SYNCHRONIZE,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
	defer func() { _ = syscall.CloseHandle(h) }() // a rename error is what matters
	size := unsafe.Offsetof(fileRenameInfo{}.FileName) + uintptr(len(name))*2
	buf := make([]uint64, (size+7)/8) // 8-byte aligned
	info := (*fileRenameInfo)(unsafe.Pointer(&buf[0]))
	info.Flags = fileRenameFlagReplaceIfExists | fileRenameFlagPosixSemantics
	info.FileNameLength = uint32(len(name)-1) * 2
	copy(unsafe.Slice(&info.FileName[0], len(name)), name)
	r, _, e := procSetFileInformationByHandle.Call(uintptr(h), fileRenameInfoEx, uintptr(unsafe.Pointer(info)), size)
	if r != 0 {
		return nil
	}
	if errors.Is(e, errorInvalidParameter) || errors.Is(e, errorNotSupported) || errors.Is(e, errorInvalidFunction) {
		return fmt.Errorf("%w: %v", errNoPosixRename, e)
	}
	return &os.LinkError{Op: "rename", Old: from, New: to, Err: e}
}
