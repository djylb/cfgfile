package cfgfile

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"

	"github.com/djylb/cfgfile/jwcc"
)

// snapshot identifies the content of a file as it was read. The zero
// value stands for a file that did not exist.
type snapshot struct {
	exists bool
	size   int64
	hash   [sha256.Size]byte
}

func snapshotOf(data []byte, exists bool) snapshot {
	if !exists {
		return snapshot{}
	}
	return snapshot{exists: true, size: int64(len(data)), hash: sha256.Sum256(data)}
}

// readCurrent reads path and fails with ErrConflict unless it matches s.
func readCurrent(path string, s snapshot, o *Options) ([]byte, error) {
	data, err := readFile(path, o)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if snapshotOf(data, exists) != s {
		return nil, fmt.Errorf("%w: %s", ErrConflict, path)
	}
	return data, nil
}

// WriteFile replaces path atomically: it writes a uniquely named temporary
// file in the same directory, syncs it, renames it over path and syncs the
// directory. Readers see the old or the new content, never a mix. The
// temporary file is removed on every failure.
//
// If path is a symbolic link, the file it points to is replaced and the
// link is kept. A hard link is broken: the path gets a new file and other
// names keep the old content. An existing file keeps its mode; a new one
// gets perm. Ownership is not copied.
//
// If only the final directory sync fails, the file has been replaced and
// the error wraps ErrNotDurable.
func WriteFile(path string, data []byte, perm fs.FileMode) (err error) {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		target, err := filepath.EvalSymlinks(path)
		if err != nil {
			return err
		}
		path = target
	}
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	if st, err := os.Stat(path); err == nil {
		perm = st.Mode().Perm()
	}
	f, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, discard(f))
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Chmod(perm); err != nil && runtime.GOOS != "windows" && runtime.GOOS != "plan9" {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	if serr := syncDir(dir); serr != nil {
		return fmt.Errorf("%w: %w", ErrNotDurable, serr)
	}
	return nil
}

func syncDir(dir string) error {
	switch runtime.GOOS {
	case "windows", "plan9", "js", "wasip1":
		return nil // directories cannot be synced; rename is durable or best effort
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

// BackupPath is the name of the backup taken before migrating path from
// version v.
func BackupPath(path string, v int) string { return path + ".v" + strconv.Itoa(v) + ".bak" }

// backup copies orig to BackupPath unless a backup exists.
func backup(path string, orig []byte, v int) error {
	perm := fs.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		perm = st.Mode().Perm()
	}
	f, err := os.OpenFile(BackupPath(path, v), os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = f.Write(orig)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		if rerr := os.Remove(f.Name()); rerr != nil {
			err = errors.Join(err, rerr)
		}
	}
	return err
}

// discard closes and removes a temporary file after a failure; problems
// doing so are reported alongside the failure.
func discard(f *os.File) error {
	var errs []error
	if err := f.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		errs = append(errs, err)
	}
	if err := os.Remove(f.Name()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func persist(path string, orig, text []byte, from int, snap snapshot, o *Options) error {
	unlock, err := lockWrites(path, o)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := readCurrent(path, snap, o); err != nil {
		return err
	}
	if err := backup(path, orig, from); err != nil {
		return err
	}
	return WriteFile(path, text, 0o600)
}

// update edits the file at path through fn, writes it back atomically
// and returns the bytes written.
// The file must still match snap, else ErrConflict; a file that did not
// exist starts as an empty object (holding the version, if versioned). An
// older file is migrated first and backed up. The edited document must
// decode into a T and pass its Validate; flags and the environment are not
// applied.
//
// The check and the rename are not atomic together: a writer that bypasses
// this package can still be overwritten in that window.
func update[T any](path string, snap snapshot, o *Options, fn func(doc *jwcc.Value) error) ([]byte, error) {
	unlock, err := lockWrites(path, o)
	if err != nil {
		return nil, err
	}
	defer unlock()
	data, err := readCurrent(path, snap, o)
	if err != nil {
		return nil, err
	}
	src, enc, err := decodeText(data)
	if err != nil {
		return nil, &Error{File: path, Err: err}
	}
	if !snap.exists {
		nl := o.newline()
		if o.versioned() {
			name, _ := marshal(o.field())
			src = fmt.Appendf(nil, "{%s  %s: %d%s}%s", nl, bytes.TrimSpace(name), o.Version, nl, nl)
		} else {
			src = []byte("{" + nl + "}" + nl)
		}
	}
	doc, ver, err := prepare(path, src, o)
	if err != nil {
		return nil, err
	}
	if err := fn(doc); err != nil {
		return nil, err
	}
	text := doc.Pack()
	var t T
	if d, ok := any(&t).(Defaulter); ok {
		d.Defaults()
	}
	if err := decode(path, text, &t, o, nil); err != nil {
		return nil, err
	}
	tags := sources{file: path, text: text}
	if tags.doc, err = jwcc.Parse(text, o.Limits); err != nil {
		return nil, &Error{File: path, Err: err}
	}
	// Required members may come from flags or the environment, which
	// writing does not apply; enum values are checked here.
	if err := checkTags(reflect.ValueOf(&t).Elem(), &tags, false); err != nil {
		return nil, err
	}
	if v, ok := any(&t).(Validator); ok {
		if err := v.Validate(); err != nil {
			return nil, &Error{File: path, Err: err}
		}
	}
	if snap.exists && ver < o.Version {
		if err := backup(path, data, ver); err != nil {
			return nil, err
		}
	}
	out := encodeText(text, enc)
	err = WriteFile(path, out, o.perm())
	if err != nil && !errors.Is(err, ErrNotDurable) {
		return nil, err
	}
	return out, err // with ErrNotDurable the file was replaced
}

// saveEdits returns the edits that make the file hold v: members whose
// value differs from what the file decodes to (defaults included) are set,
// members v no longer has are deleted. Fields are compared one by one,
// ignoring omitempty. At an overridden path the file keeps its value
// unless v changed it from loaded (the value with overrides); then the
// result is ErrOverridden unless writeOverridden.
func saveEdits[T any](path string, o *Options, v, loaded *T, overrides []jwcc.Path, writeOverridden bool) (func(*jwcc.Value) error, error) {
	want, err := toTree(reflect.ValueOf(v).Elem(), false)
	if err != nil {
		return nil, err
	}
	was, err := toTree(reflect.ValueOf(loaded).Elem(), false)
	if err != nil {
		return nil, err
	}
	var changed []string
	var keep []jwcc.Path
	for _, p := range overrides {
		a, _ := valueAt(want, p)
		b, _ := valueAt(was, p)
		if reflect.DeepEqual(a, b) {
			keep = append(keep, p)
		} else {
			changed = append(changed, p.String())
		}
	}
	if len(changed) > 0 && !writeOverridden {
		return nil, fmt.Errorf("%w: %s (use SaveOverridden to write it)", ErrOverridden, strings.Join(changed, ", "))
	}
	return func(doc *jwcc.Value) error {
		var cur T
		if d, ok := any(&cur).(Defaulter); ok {
			d.Defaults()
		}
		if err := decode(path, doc.Pack(), &cur, o, nil); err != nil {
			return err
		}
		resolvePaths(reflect.ValueOf(&cur).Elem(), filepath.Dir(path))
		have, err := toTree(reflect.ValueOf(&cur).Elem(), false)
		if err != nil {
			return err
		}
		hm, _ := have.(map[string]any)
		wm, _ := want.(map[string]any)
		wm = maps.Clone(wm)
		if o.versioned() {
			delete(hm, o.field())
			delete(wm, o.field())
		}
		delete(hm, "$schema")
		delete(wm, "$schema")
		for _, p := range keep {
			keepAt(hm, wm, p)
		}
		return diff(doc, nil, hm, wm)
	}, nil
}

// valueAt returns the value at p in a generic tree.
func valueAt(tree any, p jwcc.Path) (any, bool) {
	for _, k := range p {
		m, ok := tree.(map[string]any)
		if !ok {
			return nil, false
		}
		if tree, ok = m[k]; !ok {
			return nil, false
		}
	}
	return tree, true
}

// toTree converts v to the generic form encoding/json decodes to, with
// every struct member present (omitempty is ignored). With redact, secret
// members that are set become "[redacted]".
func toTree(v reflect.Value, redact bool) (any, error) {
	t := v.Type()
	if t.Kind() == reflect.Pointer && !opaque(t) && t.Elem().Kind() == reflect.Struct && !opaque(t.Elem()) {
		if v.IsNil() {
			return nil, nil
		}
		return toTree(v.Elem(), redact)
	}
	if t.Kind() != reflect.Struct || opaque(t) {
		return asTree(v.Interface())
	}
	m := map[string]any{}
outer:
	for _, mem := range members(t) {
		fv := v
		for i, j := range mem.index {
			if i > 0 && fv.Kind() == reflect.Pointer {
				if fv.IsNil() {
					continue outer // field of a nil embedded pointer
				}
				fv = fv.Elem()
			}
			fv = fv.Field(j)
		}
		if redact && mem.secret {
			if !fv.IsZero() {
				m[mem.name] = "[redacted]"
			} else {
				m[mem.name] = nil
			}
			continue
		}
		x, err := toTree(fv, redact)
		if err != nil {
			return nil, err
		}
		m[mem.name] = x
	}
	return m, nil
}

func asTree(v any) (any, error) {
	b, err := marshal(v)
	if err != nil {
		return nil, err
	}
	var x any
	dec := jsonDecoder(b)
	if err := dec.Decode(&x); err != nil {
		return nil, err
	}
	return x, nil
}

// keepAt makes want hold have's value (or absence) at p. Objects that
// only exist in want to hold p are removed with it.
func keepAt(have, want map[string]any, p jwcc.Path) {
	if len(p) == 0 {
		return
	}
	k := p[0]
	hv, hok := have[k]
	if len(p) == 1 {
		if hok {
			want[k] = hv
		} else {
			delete(want, k)
		}
		return
	}
	wm, ok := want[k].(map[string]any)
	if !ok {
		return // want replaced the parent; nothing finer to keep
	}
	hm, _ := hv.(map[string]any)
	if hm == nil {
		hm = map[string]any{}
	}
	keepAt(hm, wm, p[1:])
	if len(wm) == 0 && hv == nil {
		if hok {
			want[k] = nil
		} else {
			delete(want, k)
		}
	}
}

// absent marks a member the current file does not decode to.
type absent struct{}

func diff(doc *jwcc.Value, p jwcc.Path, have, want any) error {
	hm, hok := have.(map[string]any)
	wm, wok := want.(map[string]any)
	if hok && wok {
		if cur := doc.Find(p); cur == nil || cur.Obj != nil {
			for _, k := range sortedKeys(hm) {
				if _, ok := wm[k]; !ok {
					cp := append(append(jwcc.Path(nil), p...), k)
					if doc.Find(cp) != nil {
						if err := doc.Delete(cp); err != nil {
							return err
						}
					}
				}
			}
			for _, k := range sortedKeys(wm) {
				hv, ok := hm[k]
				if !ok {
					hv = absent{}
				}
				if err := diff(doc, append(append(jwcc.Path(nil), p...), k), hv, wm[k]); err != nil {
					return err
				}
			}
			return nil
		}
	}
	if reflect.DeepEqual(have, want) {
		return nil
	}
	return doc.SetValue(p, want)
}
