package cfgfile

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sync"

	"github.com/djylb/cfgfile/jwcc"
)

// File is an open configuration file: the value in effect and what is
// needed to write the file back safely. It is the main entry point:
//
//	f, err := cfgfile.Open[Config]("app.jsonc", opts)
//	cfg := f.Value()
//	cfg.Log.Level = "debug"
//	err = f.Save(&cfg)
//
// File remembers the content it read, so Save and Update refuse to
// overwrite changes made by someone else since (ErrConflict), and it
// remembers which values came from flags or the environment, so they are
// not written to the file by accident (ErrOverridden). Methods are safe
// for concurrent use; writes are serialised.
type File[T any] struct {
	path string
	o    *Options

	op sync.Mutex // serialises reading and writing the file

	mu       sync.Mutex // guards the fields below
	cur      *Result[T]
	snap     snapshot // the content cur was loaded from: the write baseline
	removed  bool     // the watcher saw the file disappear
	watching bool
}

// Open loads the configuration at path, as Load, and returns a handle for
// reading, saving and watching it. A missing file is not an error: the
// value then holds the defaults and overrides, and the first Save or
// Update creates the file (see CreateFile to write a commented template
// instead).
func Open[T any](path string, o *Options) (*File[T], error) {
	if path == "" {
		return nil, errors.New("cfgfile: Open needs a path")
	}
	o = orDefault(o)
	r, snap, err := load[T](path, o)
	if err != nil {
		return nil, err
	}
	return &File[T]{path: path, o: o, cur: r, snap: snap}, nil
}

// Path returns the file's path.
func (f *File[T]) Path() string { return f.path }

// Dir returns the directory relative Path fields are resolved against.
func (f *File[T]) Dir() string { return filepath.Dir(f.path) }

// Value returns a copy of the configuration in effect. The copy is
// shallow: do not modify maps or slices shared with it in place.
func (f *File[T]) Value() T { return f.Result().Value }

// Result returns the outcome of the last load. Treat it as read-only.
func (f *File[T]) Result() *Result[T] {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cur
}

// Save writes v to the file, changing only the members whose value
// differs from what the file holds, so comments and untouched formatting
// stay. Fields are compared one by one (omitempty is ignored), so a zero
// value is written too. Values that came from a flag or the environment
// are not written; if v changes one of them, Save fails with
// ErrOverridden naming it (see SaveOverridden). After writing, the file is
// read back and becomes the value in effect.
//
// Save fails with ErrConflict if the file changed since it was last read;
// Reload and retry. If only the final directory sync fails, the file has
// been written and the error wraps ErrNotDurable.
func (f *File[T]) Save(v *T) error { return f.save(v, false) }

// SaveOverridden is Save, but writes values set by flags or the
// environment too when v changed them.
func (f *File[T]) SaveOverridden(v *T) error { return f.save(v, true) }

func (f *File[T]) save(v *T, writeOverridden bool) error {
	cur := f.Result()
	edits, err := saveEdits(f.path, f.o, v, &cur.Value, cur.Overrides, writeOverridden)
	if err != nil {
		return err
	}
	return f.Update(edits)
}

// Update edits the file's syntax tree through fn and writes it back
// atomically; comments and formatting fn does not touch are kept. The
// result must decode into a T and pass its Validate. A missing file starts
// as an empty object; an older file is migrated first and backed up. Errors
// are as for Save.
func (f *File[T]) Update(fn func(doc *jwcc.Value) error) error {
	f.op.Lock()
	defer f.op.Unlock()
	f.mu.Lock()
	snap := f.snap
	f.mu.Unlock()
	nsnap, werr := update[T](f.path, snap, f.o, fn)
	if werr != nil && !errors.Is(werr, ErrNotDurable) {
		return werr
	}
	data, err := readFile(f.path, f.o)
	var r *Result[T]
	var rsnap snapshot
	if err == nil {
		r, rsnap, err = loadData[T](f.path, data, true, f.o)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = false
	if err != nil {
		// Written, but the result does not load with the overlay: keep the
		// value, but make the next write start from what is on disk.
		f.snap = nsnap
		return fmt.Errorf("cfgfile: file written, but reloading it failed: %w", err)
	}
	f.cur, f.snap = r, rsnap
	return werr
}

// Reload reads the file again now, with the environment and flags, and
// makes the result the value in effect. On error the previous value stays.
func (f *File[T]) Reload() error {
	_, _, err := f.reload(true)
	return err
}

// reload loads the file if its content differs from the baseline (or
// always, if force). It returns the previous and the new result when the
// value in effect was replaced.
func (f *File[T]) reload(force bool) (old, now *Result[T], err error) {
	f.op.Lock()
	defer f.op.Unlock()
	data, err := readFile(f.path, f.o)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.removed {
			return nil, nil, nil
		}
		f.removed = true
		return nil, nil, &Error{File: f.path, Err: ErrRemoved}
	case err != nil:
		return nil, nil, err
	}
	f.mu.Lock()
	same := snapshotOf(data, true) == f.snap
	f.removed = false
	f.mu.Unlock()
	if same && !force {
		return nil, nil, nil
	}
	r, snap, err := loadData[T](f.path, data, true, f.o)
	if err != nil {
		return nil, nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	old, f.cur, f.snap = f.cur, r, snap
	return old, r, nil
}
