package cfgfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"

	"github.com/djylb/cfgfile/jwcc"
)

// FilePath is a file system path that Load resolves against the directory
// of the configuration file when it is relative, so "data" next to
// /etc/app/app.jsonc means /etc/app/data. Values from flags and the
// environment stay relative to the working directory, and Save writes a
// changed value as given.
type FilePath string

var filePathType = reflect.TypeFor[FilePath]()

// absPath resolves s against dir. An absolute s is kept. On Windows a
// rooted path without a volume (\x or /x) refers to the root of dir's
// volume, and a volume-relative path (C:x) is kept as written, since it
// depends on that volume's working directory.
func absPath(dir, s string) string {
	switch {
	case filepath.IsAbs(s), filepath.VolumeName(s) != "":
		return s
	case os.IsPathSeparator(s[0]):
		return filepath.VolumeName(dir) + s
	}
	return filepath.Join(dir, s)
}

// resolvePaths makes the relative FilePath values in v absolute against
// dir.
func resolvePaths(v reflect.Value, dir string) { resolve(v, dir, 0) }

func resolve(v reflect.Value, dir string, depth int) {
	if depth > 32 {
		return
	}
	switch v.Kind() {
	case reflect.String:
		if v.Type() == filePathType && v.CanSet() {
			if s := v.String(); s != "" {
				v.SetString(absPath(dir, s))
			}
		}
	case reflect.Pointer:
		if !v.IsNil() {
			resolve(v.Elem(), dir, depth+1)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				resolve(v.Field(i), dir, depth+1)
			}
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			resolve(v.Index(i), dir, depth+1)
		}
	case reflect.Map:
		if v.Type().Elem() == filePathType {
			for _, k := range v.MapKeys() {
				if s := v.MapIndex(k).String(); s != "" {
					v.SetMapIndex(k, reflect.ValueOf(FilePath(absPath(dir, s))))
				}
			}
		}
	default:
	}
}

// DefaultDirs returns the usual places for the configuration of app, in
// order of precedence: the working directory, the user's configuration
// directory (os.UserConfigDir, e.g. ~/.config/app), then the system
// directory (/etc/app on Unix, also /Library/Application Support/app on
// macOS, %ProgramData%\app on Windows). A path given explicitly, such as a
// -config flag, should take precedence over all of them.
func DefaultDirs(app string) []string {
	dirs := []string{"."}
	if d, err := os.UserConfigDir(); err == nil {
		dirs = append(dirs, filepath.Join(d, app))
	}
	switch runtime.GOOS {
	case "windows":
		if d := os.Getenv("ProgramData"); d != "" {
			dirs = append(dirs, filepath.Join(d, app))
		}
	case "darwin", "ios":
		dirs = append(dirs, filepath.Join("/Library/Application Support", app), filepath.Join("/etc", app))
	case "js", "wasip1", "plan9":
	default:
		dirs = append(dirs, filepath.Join("/etc", app))
	}
	return dirs
}

// Locate returns the first of dir/name for dirs that is an existing file.
// If there is none, the error wraps fs.ErrNotExist and lists the places
// tried; a program without a file can then Open the path it would create.
func Locate(name string, dirs ...string) (string, error) {
	var tried []string
	for _, d := range dirs {
		p := filepath.Join(d, name)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return p, nil
		}
		tried = append(tried, p)
	}
	return "", fmt.Errorf("cfgfile: %s not found (tried %s): %w", name, strings.Join(tried, ", "), fs.ErrNotExist)
}

// Redacted returns v in the generic form encoding/json produces, with
// the values of secret fields replaced by "[redacted]", for logging.
func Redacted[T any](v *T) any {
	x, err := toTree(reflect.ValueOf(v).Elem(), true)
	if err != nil {
		return fmt.Sprintf("<%v>", err)
	}
	return x
}

// Template returns a configuration file for T holding every member with
// its default (from Defaults; fields under a secret member are left zero), the doc tag
// of each member as a comment above it and, when versioned, the version.
func Template[T any](o *Options) ([]byte, error) {
	o = orDefault(o)
	var v T
	if d, ok := any(&v).(Defaulter); ok {
		d.Defaults()
	}
	rv := reflect.ValueOf(&v).Elem()
	if rv.Kind() != reflect.Struct {
		return nil, fmt.Errorf("cfgfile: %v is not a struct", rv.Type())
	}
	var b bytes.Buffer
	b.WriteString("{\n")
	if o.versioned() {
		name, _ := marshal(o.field())
		b.Write(bytes.TrimSpace(name))
		b.WriteString(": " + strconv.Itoa(o.Version) + ",\n")
	}
	if err := template(&b, rv, o, true, false, 0); err != nil {
		return nil, err
	}
	b.WriteString("}\n")
	doc, err := jwcc.Parse(b.Bytes(), nil)
	if err != nil {
		return nil, err
	}
	return jwcc.Format(doc, &jwcc.FormatOptions{Newline: o.newline()}), nil
}

func template(b *bytes.Buffer, v reflect.Value, o *Options, top, secret bool, depth int) error {
	for _, m := range members(v.Type()) {
		if top && o.versioned() && m.name == o.field() {
			continue
		}
		if m.doc != "" {
			for _, line := range strings.Split(m.doc, "\n") {
				b.WriteString("// " + line + "\n")
			}
		}
		name, _ := marshal(m.name)
		b.Write(bytes.TrimSpace(name))
		b.WriteString(": ")
		fv, ok := fieldOf(v, m.index)
		sec := secret || m.secret
		ft := m.typ
		for ft.Kind() == reflect.Pointer && !opaque(ft) {
			ft = ft.Elem()
		}
		switch {
		case ft.Kind() == reflect.Struct && !opaque(ft) && depth < 32:
			b.WriteString("{\n")
			inner := reflect.New(ft).Elem()
			if ok && !sec {
				for fv.Kind() == reflect.Pointer && !fv.IsNil() {
					fv = fv.Elem()
				}
				if fv.Kind() == reflect.Struct {
					inner = fv
				}
			}
			if err := template(b, inner, o, false, sec, depth+1); err != nil {
				return err
			}
			b.WriteString("}")
		case sec || !ok:
			out, err := json.Marshal(reflect.Zero(m.typ).Interface())
			if err != nil {
				return err
			}
			b.Write(out)
		default:
			out, err := marshal(fv.Interface())
			if err != nil {
				return err
			}
			b.Write(bytes.TrimSpace(out))
		}
		b.WriteString(",\n")
	}
	return nil
}

// fieldOf follows an index path, reporting false at a nil embedded
// pointer.
func fieldOf(v reflect.Value, index []int) (reflect.Value, bool) {
	for i, j := range index {
		if i > 0 && v.Kind() == reflect.Pointer {
			if v.IsNil() {
				return v, false
			}
			v = v.Elem()
		}
		v = v.Field(j)
	}
	return v, true
}

// CreateFile writes Template to path if no file exists there; otherwise it
// fails with an error wrapping fs.ErrExist and leaves the file alone.
func CreateFile[T any](path string, o *Options) (err error) {
	o = orDefault(o)
	text, err := Template[T](o)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, o.perm())
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil && !errors.Is(err, fs.ErrExist) {
			err = errors.Join(err, os.Remove(path))
		}
	}()
	if _, err = f.Write(text); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}
