package cfgfile

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"

	"github.com/djylb/cfgfile/jwcc"
)

// Snapshot identifies the content of a file as it was read. The zero value
// stands for a file that did not exist.
type Snapshot struct {
	Exists bool
	Size   int64
	Hash   [sha256.Size]byte
	// Overrides lists the paths whose loaded value came from a flag or an
	// environment variable. Save keeps the file's value at these paths;
	// remove a path from the list to write the in-memory value instead.
	// Update and Save carry it over to the snapshot they return.
	Overrides []jwcc.Path
}

func (s Snapshot) same(t Snapshot) bool {
	return s.Exists == t.Exists && s.Size == t.Size && s.Hash == t.Hash
}

func snapshotOf(data []byte, exists bool) Snapshot {
	if !exists {
		return Snapshot{}
	}
	return Snapshot{Exists: true, Size: int64(len(data)), Hash: sha256.Sum256(data)}
}

// readCurrent reads path and fails with ErrConflict unless it matches s.
func readCurrent(path string, s Snapshot, o *Options) ([]byte, error) {
	data, err := readFile(path, o)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if !snapshotOf(data, exists).same(s) {
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
			f.Close()
			os.Remove(f.Name())
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
		os.Remove(f.Name())
	}
	return err
}

func persist(path string, orig, text []byte, from int, snap Snapshot) error {
	if _, err := readCurrent(path, snap, nil); err != nil {
		return err
	}
	if err := backup(path, orig, from); err != nil {
		return err
	}
	return WriteFile(path, text, 0o600)
}

// Update edits the file at path through fn and writes it back atomically.
// The file must still match snap (from Result.Snapshot or a previous
// Update), else ErrConflict; a file that did not exist starts as an
// object holding only the version. An older file is migrated first (and
// backed up as by Load). The edited document must decode into a T and pass
// its Validate; flags and the environment are not applied. Comments and
// formatting fn does not touch are kept.
//
// The check and the rename are not atomic together: a writer that bypasses
// this package can still be overwritten in that window.
func Update[T any](path string, snap Snapshot, o *Options, fn func(doc *jwcc.Value) error) (Snapshot, error) {
	if err := o.check(); err != nil {
		return Snapshot{}, err
	}
	data, err := readCurrent(path, snap, o)
	if err != nil {
		return Snapshot{}, err
	}
	src, enc, err := decodeText(data)
	if err != nil {
		return Snapshot{}, &Error{File: path, Err: err}
	}
	if !snap.Exists {
		name, _ := marshal(o.field())
		nl := "\n"
		if o.CRLF {
			nl = "\r\n"
		}
		src = fmt.Appendf(nil, "{%s  %s: %d%s}%s", nl, bytes.TrimSpace(name), o.Version, nl, nl)
	}
	doc, ver, err := prepare(path, src, o)
	if err != nil {
		return Snapshot{}, err
	}
	if err := fn(doc); err != nil {
		return Snapshot{}, err
	}
	text := doc.Pack()
	var t T
	if d, ok := any(&t).(Defaulter); ok {
		d.Defaults()
	}
	if err := decode(path, text, &t, o); err != nil {
		return Snapshot{}, err
	}
	if v, ok := any(&t).(Validator); ok {
		if err := v.Validate(); err != nil {
			return Snapshot{}, &Error{File: path, Err: err}
		}
	}
	if snap.Exists && ver < o.Version {
		if err := backup(path, data, ver); err != nil {
			return Snapshot{}, err
		}
	}
	perm := o.Perm
	if perm == 0 {
		perm = 0o600
	}
	out := encodeText(text, enc)
	err = WriteFile(path, out, perm)
	if err != nil && !errors.Is(err, ErrNotDurable) {
		return Snapshot{}, err
	}
	ns := snapshotOf(out, true)
	ns.Overrides = snap.Overrides
	return ns, err // with ErrNotDurable the file was replaced and ns is valid
}

// Save writes v to path through Update, changing only members whose value
// differs from what the file currently decodes to (defaults included).
// Values are compared field by field, ignoring omitempty, so a zero value
// the caller set is written too:
// changed values are set, members v no longer has are deleted, and
// everything else, comments included, is left alone. Arrays are replaced
// whole. Paths in snap.Overrides keep the file's value, so flag and
// environment overrides are never written unless the caller removes them
// from that list.
func Save[T any](path string, snap Snapshot, v *T, o *Options) (Snapshot, error) {
	want, err := toTree(reflect.ValueOf(v).Elem())
	if err != nil {
		return Snapshot{}, err
	}
	return Update[T](path, snap, o, func(doc *jwcc.Value) error {
		var cur T
		if d, ok := any(&cur).(Defaulter); ok {
			d.Defaults()
		}
		if err := decode(path, doc.Pack(), &cur, o); err != nil {
			return err
		}
		have, err := toTree(reflect.ValueOf(&cur).Elem())
		if err != nil {
			return err
		}
		hm, _ := have.(map[string]any)
		wm, _ := want.(map[string]any)
		delete(hm, o.field())
		delete(wm, o.field())
		delete(hm, "$schema")
		delete(wm, "$schema")
		for _, p := range snap.Overrides {
			keepAt(hm, wm, p)
		}
		return diff(doc, nil, hm, wm)
	})
}

// toTree converts v to the generic form encoding/json decodes to, with
// every struct member present (omitempty is ignored).
func toTree(v reflect.Value) (any, error) {
	t := v.Type()
	if t.Kind() == reflect.Pointer && !opaque(t) && t.Elem().Kind() == reflect.Struct && !opaque(t.Elem()) {
		if v.IsNil() {
			return nil, nil
		}
		return toTree(v.Elem())
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
		x, err := toTree(fv)
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
	b, err := marshal(want)
	if err != nil {
		return err
	}
	return doc.Set(p, bytes.TrimSpace(b))
}
