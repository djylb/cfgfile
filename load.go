package cfgfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"github.com/djylb/cfgfile/jwcc"
)

// Errors of this package; test with errors.Is.
var (
	ErrTooNew       = errors.New("cfgfile: file version is newer than this program supports")
	ErrNoVersion    = errors.New("cfgfile: file has no valid version")
	ErrNoMigration  = errors.New("cfgfile: no migration for file version")
	ErrConflict     = errors.New("cfgfile: file changed since it was read")
	ErrOverridden   = errors.New("cfgfile: value set by a flag or environment variable was changed")
	ErrNotPersisted = errors.New("cfgfile: migrated configuration not written back")
	ErrNotDurable   = errors.New("cfgfile: file replaced, but the directory sync failed")
	ErrUnknown      = errors.New("unknown member")
	ErrEncoding     = errors.New("cfgfile: unsupported file encoding")
	ErrRemoved      = errors.New("cfgfile: configuration file removed")
)

// Migration rewrites a document of version N into version N+1. It edits
// the syntax tree, so it can handle members the current type no longer
// has. Load sets the version member afterwards.
type Migration func(doc *jwcc.Value) error

// Unknown selects what happens to members of the file that the type does
// not have.
type Unknown int

const (
	// UnknownReject makes an unknown member an error (the default).
	UnknownReject Unknown = iota
	// UnknownWarn ignores unknown members and reports each one in
	// Result.Warnings, wrapped with ErrUnknown.
	UnknownWarn
)

// Options configures Load and Open. A nil *Options is the zero value: an
// unversioned file, no environment, no flags.
type Options struct {
	// Version is the file version this program reads and writes. Zero
	// means unversioned: no version member is required or written.
	Version int
	// VersionField is the top-level member holding the version; default
	// "version".
	VersionField string
	// Migrations[N] migrates version N to N+1.
	Migrations map[int]Migration
	// EnvPrefix enables environment overlay ("" disables it); see Field.
	EnvPrefix string
	// LookupEnv reads the environment; default os.LookupEnv.
	LookupEnv func(string) (string, bool)
	// Flags is read after it has been parsed: every flag that was set on
	// the command line and is named like a field (see Field and
	// DefineFlags) overrides the environment and the file.
	Flags *flag.FlagSet
	// Limits bounds parsing; nil uses the jwcc defaults. Files larger than
	// Limits.MaxSize (twice that for UTF-16) are not read.
	Limits *jwcc.Limits
	// Unknown selects how unknown members are handled.
	Unknown Unknown
	// NewFilePerm is the mode of a file this package creates; default 0600.
	// Existing files keep their mode.
	NewFilePerm fs.FileMode
	// Persist makes Load write a migrated file back: the original is first
	// copied to <path>.v<old>.bak (never overwritten), then the file is
	// replaced atomically. If that fails Load still succeeds and reports
	// the failure in Result.Warnings, wrapped with ErrNotPersisted.
	Persist bool
	// NewFileCRLF makes a file this package creates use CRLF line endings
	// instead of LF. Existing files keep their own line endings.
	NewFileCRLF bool
}

var noOptions Options

func (o *Options) field() string {
	if o.VersionField == "" {
		return "version"
	}
	return o.VersionField
}

func (o *Options) versioned() bool { return o.Version > 0 }

func (o *Options) perm() fs.FileMode {
	if o.NewFilePerm == 0 {
		return 0o600
	}
	return o.NewFilePerm
}

func (o *Options) newline() string {
	if o.NewFileCRLF {
		return "\r\n"
	}
	return "\n"
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
	// FileVersion is the version found in the file (0 without a file or
	// when unversioned).
	FileVersion int
	// Migrated reports that migrations ran; Persisted that the result was
	// written back.
	Migrated, Persisted bool
	// Text is the document after migration (nil without a file), for
	// example for a dry run. It is UTF-8 even for a UTF-16 file.
	Text []byte
	// Overrides lists the paths set by a flag or an environment variable.
	Overrides []jwcc.Path
	// Warnings are non-fatal problems, such as ErrNotPersisted and, with
	// UnknownWarn, ErrUnknown.
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

// Load reads the configuration at path into a T, which must be a struct.
// It is the stateless form of Open, for programs that only read:
//
//  1. Defaults, if *T is a Defaulter.
//  2. The file, if path is not empty and the file exists: parse, check the
//     version (newer than Options.Version is ErrTooNew), run migrations,
//     then decode strictly: unknown members (see Options.Unknown), null for
//     non-pointer fields, and members that differ from a field name only in
//     case are errors. The top-level "$schema" member and, when versioned,
//     the version member are always allowed. Relative Path fields are
//     resolved against the file's directory.
//  3. Environment variables, then flags (see Field).
//  4. Validate, if *T is a Validator.
//  5. With Options.Persist, write a migrated file back.
//
// Load is safe for concurrent use and starts no goroutines.
func Load[T any](path string, o *Options) (*Result[T], error) {
	r, _, err := load[T](path, o)
	return r, err
}

func load[T any](path string, o *Options) (*Result[T], snapshot, error) {
	o = orDefault(o)
	if err := o.check(); err != nil {
		return nil, snapshot{}, err
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
			return nil, snapshot{}, err
		}
	}
	return loadData[T](path, data, exists, o)
}

func orDefault(o *Options) *Options {
	if o == nil {
		return &noOptions
	}
	return o
}

// loadData is Load on content already read from path.
func loadData[T any](path string, data []byte, exists bool, o *Options) (*Result[T], snapshot, error) {
	r := &Result[T]{Exists: exists}
	v := reflect.ValueOf(&r.Value).Elem()
	if v.Kind() != reflect.Struct {
		return nil, snapshot{}, fmt.Errorf("cfgfile: %v is not a struct", v.Type())
	}
	if d, ok := any(&r.Value).(Defaulter); ok {
		d.Defaults()
	}
	snap := snapshotOf(data, exists)
	var enc textEncoding
	if exists {
		text, e, err := decodeText(data)
		if err != nil {
			return nil, snapshot{}, &Error{File: path, Err: err}
		}
		enc = e
		doc, ver, err := prepare(path, text, o)
		if err != nil {
			return nil, snapshot{}, err
		}
		r.FileVersion, r.Migrated = ver, ver < o.Version
		r.Text = doc.Pack()
		if err := decode(path, r.Text, &r.Value, o, &r.Warnings); err != nil {
			return nil, snapshot{}, err
		}
		resolvePaths(v, filepath.Dir(path))
	}
	overrides, err := overlay(v, o)
	if err != nil {
		return nil, snapshot{}, err
	}
	r.Overrides = overrides
	if val, ok := any(&r.Value).(Validator); ok {
		if err := val.Validate(); err != nil {
			return nil, snapshot{}, &Error{File: path, Err: err}
		}
	}
	if r.Migrated && o.Persist {
		out := encodeText(r.Text, enc)
		err := persist(path, data, out, r.FileVersion, snap, o)
		if errors.Is(err, ErrNotDurable) {
			r.Warnings = append(r.Warnings, err)
			err = nil
		}
		if err != nil {
			r.Warnings = append(r.Warnings, fmt.Errorf("%w: %w", ErrNotPersisted, err))
		} else {
			r.Persisted = true
			snap = snapshotOf(out, true)
		}
	}
	return r, snap, nil
}

func (o *Options) check() error {
	switch {
	case o.Version < 0:
		return errors.New("cfgfile: Options.Version must not be negative")
	case o.Version == 0 && len(o.Migrations) > 0:
		return errors.New("cfgfile: Options.Migrations needs Options.Version")
	}
	return nil
}

// secretAt reports whether p lies under a secret member of t.
func secretAt(t reflect.Type, p jwcc.Path) bool {
	for _, seg := range p {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		switch t.Kind() {
		case reflect.Struct:
			found := false
			for _, m := range members(t) {
				if m.name == seg {
					if m.secret {
						return true
					}
					t, found = m.typ, true
					break
				}
			}
			if !found {
				return false
			}
		case reflect.Slice, reflect.Array, reflect.Map:
			t = t.Elem()
		default:
			return false
		}
	}
	return false
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
	if doc.Obj == nil {
		return nil, 0, &Error{File: path, Line: 1, Column: 1, Err: errors.New("top level is not an object")}
	}
	if !o.versioned() {
		return doc, 0, nil
	}
	vp := jwcc.Path{o.field()}
	ver := 0
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
func decode(path string, text []byte, dst any, o *Options, warnings *[]error) error {
	doc, err := jwcc.Parse(text, o.Limits)
	if err != nil {
		return &Error{File: path, Err: err}
	}
	std := doc.Standardize()
	at := func(off int, p jwcc.Path, err error) error {
		l, c := jwcc.Position(text, off)
		return &Error{File: path, Line: l, Column: c, Path: p, Err: err}
	}
	c := checker{std: std, versionField: o.field(), versioned: o.versioned(), at: at, unknown: o.Unknown, warnings: warnings}
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
			if secretAt(reflect.TypeOf(dst).Elem(), p) {
				kind, _, _ := strings.Cut(te.Value, " ")
				return at(off, p, fmt.Errorf("cannot use %s as %v (secret value not shown)", kind, te.Type))
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
	unknown      Unknown
	warnings     *[]error
	versioned    bool
	secret       bool // inside a secret member: keep values out of errors
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
		default:
			return at(v.Start, p, fmt.Errorf("null is not allowed for %v", t))
		}
	}
	for t.Kind() == reflect.Pointer && !opaque(t) {
		t = t.Elem()
	}
	if opaque(t) {
		// Decode it alone so that its error gets a position.
		if err := json.Unmarshal(c.std[v.Start:v.End], reflect.New(t).Interface()); err != nil {
			var te *json.UnmarshalTypeError
			switch {
			case c.secret:
				err = fmt.Errorf("invalid %v (secret value not shown)", t)
			case errors.As(err, &te):
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
		byName := map[string]member{}
		for _, m := range members(t) {
			byName[m.name] = m
		}
		for i := range v.Obj.Members {
			m := &v.Obj.Members[i]
			key := m.Key()
			cp := append(append(jwcc.Path(nil), p...), key)
			fm, ok := byName[key]
			ft := fm.typ
			if !ok {
				if len(p) == 0 && (key == "$schema" || c.versioned && key == c.versionField) {
					continue
				}
				err := at(m.Name.Start, cp, ErrUnknown)
				switch c.unknown {
				case UnknownReject:
					return err
				case UnknownWarn:
					if c.warnings != nil {
						*c.warnings = append(*c.warnings, err)
					}
				default:
					return err
				}
				continue
			}
			inner := *c
			inner.secret = c.secret || fm.secret
			if err := inner.tree(&m.Value, ft, cp); err != nil {
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
	default:
		// Scalars and interfaces: encoding/json reports type errors.
	}
	return nil
}
