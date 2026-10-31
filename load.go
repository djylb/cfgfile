package cfgfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"strconv"

	"github.com/djylb/cfgfile/jwcc"
)

// Errors returned by Load, Update and Save; test with errors.Is.
var (
	ErrTooNew       = errors.New("cfgfile: file version is newer than this program supports")
	ErrNoVersion    = errors.New("cfgfile: file has no valid version")
	ErrNoMigration  = errors.New("cfgfile: no migration for file version")
	ErrConflict     = errors.New("cfgfile: file changed since it was read")
	ErrNotPersisted = errors.New("cfgfile: migrated configuration not written back")
	ErrNotDurable   = errors.New("cfgfile: file replaced, but the directory sync failed")
)

// Migration rewrites a document of version N into version N+1. It edits
// the syntax tree, so it can handle members the current type no longer
// has. Load sets the version member afterwards.
type Migration func(doc *jwcc.Value) error

// Options configures Load, Update and Save. Version is required.
type Options struct {
	// Version is the file version this program reads and writes (>= 1).
	Version int
	// VersionField is the top-level member holding the version; default
	// "version".
	VersionField string
	// Migrations[N] migrates version N to N+1.
	Migrations map[int]Migration
	// Persist makes Load write a migrated file back: the original is first
	// copied to <path>.v<old>.bak (never overwritten), then the file is
	// replaced atomically. If that fails Load still succeeds and reports
	// the failure in Result.Warnings, wrapped with ErrNotPersisted.
	Persist bool
	// EnvPrefix enables environment overlay ("" disables it); see Field.
	EnvPrefix string
	// LookupEnv reads the environment; default os.LookupEnv.
	LookupEnv func(string) (string, bool)
	// Flags, from BindFlags, override the environment and the file.
	Flags *Flags
	// Limits bounds parsing; nil uses the jwcc defaults. Files larger than
	// Limits.MaxSize (twice that for UTF-16) are not read.
	Limits *jwcc.Options
	// Perm is the mode of a file Update or Save creates; default 0600.
	// Existing files keep their mode.
	Perm fs.FileMode
	// CRLF makes a file that Update or Save creates use CRLF line endings
	// instead of LF. Existing files keep their own line endings.
	CRLF bool
}

func (o *Options) field() string {
	if o.VersionField == "" {
		return "version"
	}
	return o.VersionField
}

// Defaulter is implemented by configuration types that set defaults before
// the file is decoded; members absent from the file keep them.
type Defaulter interface{ Defaults() }

// Validator is implemented by configuration types that check themselves
// after the file and the overlay are applied.
type Validator interface{ Validate() error }

// Result is the outcome of Load.
type Result[T any] struct {
	Value T
	// Exists reports that the file was found.
	Exists bool
	// FileVersion is the version found in the file (0 without a file).
	FileVersion int
	// Migrated reports that migrations ran; Persisted that the result was
	// written back.
	Migrated, Persisted bool
	// Text is the document after migration (nil without a file): the input
	// of a dry run. It is UTF-8 even for a UTF-16 file.
	Text []byte
	// Snapshot identifies the file on disk, for Update and Save.
	Snapshot Snapshot
	// Warnings are non-fatal problems, such as ErrNotPersisted.
	Warnings []error
}

// Error locates a problem in a configuration file.
type Error struct {
	File         string
	Line, Column int
	Path         jwcc.Path
	Err          error
}

func (e *Error) Error() string {
	s := e.File
	if e.Line > 0 {
		s += ":" + strconv.Itoa(e.Line) + ":" + strconv.Itoa(e.Column)
	}
	if len(e.Path) > 0 {
		s += ": " + e.Path.String()
	}
	return s + ": " + e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

// msgErr is a message with an optional cause for errors.Is.
type msgErr struct {
	msg string
	err error
}

func (e *msgErr) Error() string { return e.msg }
func (e *msgErr) Unwrap() error { return e.err }

// Load reads the configuration at path into a T, which must be a struct:
//
//  1. Defaults, if *T is a Defaulter.
//  2. The file, if path is not empty and the file exists: parse, check the
//     version (newer than Options.Version is ErrTooNew), run migrations,
//     then decode strictly: unknown members, null for non-pointer fields,
//     and members that differ from a field name only in case are errors.
//     The top-level "$schema" member and the version member are always
//     allowed.
//  3. Environment variables, then flags (see Field).
//  4. Validate, if *T is a Validator.
//  5. With Options.Persist, write a migrated file back.
//
// Load is safe for concurrent use and starts no goroutines.
func Load[T any](path string, o *Options) (*Result[T], error) {
	if err := o.check(); err != nil {
		return nil, err
	}
	var data []byte
	exists := false
	if path != "" {
		var err error
		data, err = readFile(path, o)
		switch {
		case err == nil:
			exists = true
		case !errors.Is(err, fs.ErrNotExist):
			return nil, err
		}
	}
	return loadData[T](path, data, exists, o)
}

// loadData is Load on content already read from path.
func loadData[T any](path string, data []byte, exists bool, o *Options) (*Result[T], error) {
	r := &Result[T]{Exists: exists}
	v := reflect.ValueOf(&r.Value).Elem()
	if v.Kind() != reflect.Struct {
		return nil, fmt.Errorf("cfgfile: %v is not a struct", v.Type())
	}
	if d, ok := any(&r.Value).(Defaulter); ok {
		d.Defaults()
	}
	r.Snapshot = snapshotOf(data, r.Exists)
	var enc textEncoding
	if r.Exists {
		text, e, err := decodeText(data)
		if err != nil {
			return nil, &Error{File: path, Err: err}
		}
		enc = e
		doc, ver, err := prepare(path, text, o)
		if err != nil {
			return nil, err
		}
		r.FileVersion, r.Migrated = ver, ver < o.Version
		r.Text = doc.Pack()
		if err := decode(path, r.Text, &r.Value, o); err != nil {
			return nil, err
		}
	}
	overrides, err := overlay(v, o)
	if err != nil {
		return nil, err
	}
	r.Snapshot.Overrides = overrides
	if val, ok := any(&r.Value).(Validator); ok {
		if err := val.Validate(); err != nil {
			return nil, &Error{File: path, Err: err}
		}
	}
	if r.Migrated && o.Persist {
		err := persist(path, data, encodeText(r.Text, enc), r.FileVersion, r.Snapshot)
		if errors.Is(err, ErrNotDurable) {
			r.Warnings = append(r.Warnings, err)
			err = nil
		}
		if err != nil {
			r.Warnings = append(r.Warnings, fmt.Errorf("%w: %w", ErrNotPersisted, err))
		} else {
			r.Persisted = true
			r.Snapshot = snapshotOf(encodeText(r.Text, enc), true)
			r.Snapshot.Overrides = overrides
		}
	}
	return r, nil
}

func (o *Options) check() error {
	if o == nil || o.Version < 1 {
		return errors.New("cfgfile: Options.Version must be at least 1")
	}
	return nil
}

// prepare parses data, checks its version and runs migrations.
func prepare(path string, data []byte, o *Options) (*jwcc.Value, int, error) {
	doc, err := jwcc.Parse(data, o.Limits)
	if err != nil {
		var se *jwcc.SyntaxError
		if errors.As(err, &se) {
			return nil, 0, &Error{File: path, Line: se.Line, Column: se.Column, Path: se.Path, Err: &msgErr{se.Msg, errors.Unwrap(se)}}
		}
		return nil, 0, &Error{File: path, Err: err}
	}
	vp := jwcc.Path{o.field()}
	ver := 0
	if doc.Obj == nil {
		return nil, 0, &Error{File: path, Line: 1, Column: 1, Err: errors.New("top level is not an object")}
	}
	if vv := doc.Find(vp); vv != nil {
		n, err := strconv.Atoi(string(vv.Lit))
		if err == nil && n >= 1 {
			ver = n
		}
	}
	if ver == 0 {
		return nil, 0, &Error{File: path, Path: vp, Err: fmt.Errorf("%w: add a top-level %q member with the integer version the file was written for (this program writes version %d)", ErrNoVersion, o.field(), o.Version)}
	}
	if ver > o.Version {
		return nil, 0, &Error{File: path, Path: vp, Err: fmt.Errorf("%w: file %d, program %d", ErrTooNew, ver, o.Version)}
	}
	for n := ver; n < o.Version; n++ {
		m := o.Migrations[n]
		if m == nil {
			return nil, 0, &Error{File: path, Err: fmt.Errorf("%w %d", ErrNoMigration, n)}
		}
		if err := m(doc); err != nil {
			return nil, 0, &Error{File: path, Err: fmt.Errorf("migrating version %d to %d: %w", n, n+1, err)}
		}
		if doc.Obj == nil {
			return nil, 0, &Error{File: path, Err: fmt.Errorf("migrating version %d: top level is not an object", n)}
		}
		if err := doc.Set(vp, []byte(strconv.Itoa(n+1))); err != nil {
			return nil, 0, &Error{File: path, Err: fmt.Errorf("migrating version %d: %w", n, err)}
		}
	}
	return doc, ver, nil
}

// decode strictly decodes the document text into dst.
func decode(path string, text []byte, dst any, o *Options) error {
	doc, err := jwcc.Parse(text, o.Limits)
	if err != nil {
		return &Error{File: path, Err: err}
	}
	std := doc.Standardize()
	at := func(off int, p jwcc.Path, err error) error {
		l, c := jwcc.Position(text, off)
		return &Error{File: path, Line: l, Column: c, Path: p, Err: err}
	}
	c := checker{std: std, versionField: o.field(), at: at}
	if err := c.tree(doc, reflect.TypeOf(dst).Elem(), nil); err != nil {
		return err
	}
	// checker.tree has rejected unknown members with their positions, and
	// allows "$schema" and the version member that T may not have.
	dec := json.NewDecoder(bytes.NewReader(std))
	if err := dec.Decode(dst); err != nil {
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) {
			off := int(te.Offset) - 1
			p := doc.Locate(off)
			if n := doc.Find(p); n != nil {
				off = n.Start
			}
			return at(off, p, fmt.Errorf("cannot use %s as %v", te.Value, te.Type))
		}
		return &Error{File: path, Err: err}
	}
	return nil
}

// checker walks a document beside its target type.
type checker struct {
	std          []byte
	versionField string
	at           func(int, jwcc.Path, error) error
}

// tree reports what encoding/json would accept silently or without a
// position: unknown or case-mismatched members, null for values that cannot
// be null, and values that decode themselves and fail.
func (c *checker) tree(v *jwcc.Value, t reflect.Type, p jwcc.Path) error {
	at := c.at
	if v.Kind() == 'n' {
		switch t.Kind() {
		case reflect.Pointer, reflect.Interface, reflect.Slice, reflect.Map:
			return nil
		}
		return at(v.Start, p, fmt.Errorf("null is not allowed for %v", t))
	}
	for t.Kind() == reflect.Pointer && !opaque(t) {
		t = t.Elem()
	}
	if opaque(t) {
		// Decode it alone so that its error gets a position.
		if err := json.Unmarshal(c.std[v.Start:v.End], reflect.New(t).Interface()); err != nil {
			var te *json.UnmarshalTypeError
			if errors.As(err, &te) {
				err = fmt.Errorf("cannot use %s as %v", te.Value, t)
			}
			return at(v.Start, p, err)
		}
		return nil
	}
	switch t.Kind() {
	case reflect.Struct:
		if v.Obj == nil {
			return nil // encoding/json reports the type error
		}
		byName := map[string]reflect.Type{}
		for _, m := range members(t) {
			byName[m.name] = m.typ
		}
		for i := range v.Obj.Members {
			m := &v.Obj.Members[i]
			key := m.Key()
			cp := append(append(jwcc.Path(nil), p...), key)
			ft, ok := byName[key]
			if !ok {
				if len(p) == 0 && (key == "$schema" || key == c.versionField) {
					continue
				}
				return at(m.Name.Start, cp, errors.New("unknown member"))
			}
			if err := c.tree(&m.Value, ft, cp); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Arr != nil && t.Kind() == reflect.Array && len(v.Arr.Elems) > t.Len() {
			e := &v.Arr.Elems[t.Len()]
			return at(e.Start, append(append(jwcc.Path(nil), p...), strconv.Itoa(t.Len())), fmt.Errorf("more than %d elements for %v", t.Len(), t))
		}
		if v.Arr != nil {
			for i := range v.Arr.Elems {
				if err := c.tree(&v.Arr.Elems[i], t.Elem(), append(append(jwcc.Path(nil), p...), strconv.Itoa(i))); err != nil {
					return err
				}
			}
		}
	case reflect.Map:
		if v.Obj != nil {
			for i := range v.Obj.Members {
				m := &v.Obj.Members[i]
				if err := c.tree(&m.Value, t.Elem(), append(append(jwcc.Path(nil), p...), m.Key())); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
