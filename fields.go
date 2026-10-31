package cfgfile

import (
	"encoding"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/djylb/cfgfile/jwcc"
)

var (
	textUnmarshaler = reflect.TypeFor[encoding.TextUnmarshaler]()
	jsonUnmarshaler = reflect.TypeFor[json.Unmarshaler]()
)

// opaque reports a type that decodes itself.
func opaque(t reflect.Type) bool {
	return reflect.PointerTo(t).Implements(textUnmarshaler) || reflect.PointerTo(t).Implements(jsonUnmarshaler) ||
		t.Implements(textUnmarshaler) || t.Implements(jsonUnmarshaler)
}

// member is a struct field as encoding/json sees it.
type member struct {
	name          string
	index         []int
	typ           reflect.Type
	doc           string
	secret        bool
	noFlag, noEnv bool
	required      bool
	enum          []string
}

// members lists the JSON members of struct type t as encoding/json sees
// them: embedded structs without a name tag are flattened; a shallower
// field hides deeper ones; among fields of equal depth with the same name,
// a single tagged one wins and otherwise all are dropped.
func members(t reflect.Type) []member {
	var out []member
	seen := map[string]bool{}
	visited := map[reflect.Type]bool{}
	type level struct {
		t     reflect.Type
		index []int
	}
	cur := []level{{t, nil}}
	for len(cur) > 0 {
		var next []level
		var found []member
		var tagged []bool
		for _, l := range cur {
			if visited[l.t] {
				continue
			}
			visited[l.t] = true
			for i := range l.t.NumField() {
				sf := l.t.Field(i)
				tag := sf.Tag.Get("json")
				if tag == "-" {
					continue
				}
				name, _, _ := strings.Cut(tag, ",")
				tagName := name
				idx := append(append([]int(nil), l.index...), i)
				ft := sf.Type
				if sf.Anonymous && name == "" {
					if ft.Kind() == reflect.Pointer {
						ft = ft.Elem()
					}
					if ft.Kind() == reflect.Struct {
						next = append(next, level{ft, idx})
						continue
					}
				}
				if !sf.IsExported() {
					continue
				}
				if name == "" {
					name = sf.Name
				}
				found = append(found, member{name: name, index: idx, typ: sf.Type, doc: sf.Tag.Get("doc"),
					secret: sf.Tag.Get("secret") == "true", noFlag: sf.Tag.Get("flag") == "-", noEnv: sf.Tag.Get("env") == "-",
					required: sf.Tag.Get("required") == "true", enum: enumTag(sf.Tag.Get("enum"))})
				tagged = append(tagged, tagName != "")
			}
		}
		count, tags := map[string]int{}, map[string]int{}
		for i, m := range found {
			count[m.name]++
			if tagged[i] {
				tags[m.name]++
			}
		}
		for i, m := range found {
			if seen[m.name] {
				continue
			}
			if count[m.name] == 1 || tags[m.name] == 1 && tagged[i] {
				seen[m.name] = true
				out = append(out, m)
			}
		}
		for name := range count {
			seen[name] = true // a conflict at this depth hides deeper fields too
		}
		cur = next
	}
	return out
}

func enumTag(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// Field is a leaf of a configuration type: a value that the file, a flag
// and an environment variable can each set.
//
// Struct tags control it beside json: doc:"text" documents it (flag usage,
// generated files, schema); flag:"-" and env:"-" keep it from flags or
// the environment; secret:"true" keeps its value out of errors, flag
// defaults, generated files and Redacted; required:"true" makes Load fail
// (ErrRequired) unless the file, a flag or the environment sets it;
// enum:"a,b,c" limits it to the listed values (ErrEnum). The schema tool
// emits required and enum too.
type Field struct {
	// Path is the path of the value in the file, such as log.level.
	Path jwcc.Path
	// Flag is the flag name: the path in text form, such as "log.level";
	// empty with flag:"-".
	Flag string
	// Env is the environment variable: the prefix followed by the path
	// with '.', '-' and brackets turned into '_' and letters upper-cased,
	// such as APP_LOG_LEVEL; empty with env:"-" or without a prefix.
	// Env+"_FILE" names a file holding the value instead.
	Env string
	// Doc is the field's doc tag.
	Doc    string
	Secret bool
	Type   reflect.Type
	index  [][]int
}

// Fields lists the leaves of T, which must be a struct. A leaf is a field
// whose type is not a struct, or decodes itself through
// encoding.TextUnmarshaler or json.Unmarshaler; slices and maps are leaves.
// The top-level version member of a versioned file is not a field. Two
// leaves mapping to the same environment variable are an error.
func Fields[T any](o *Options) ([]Field, error) {
	return fieldsOf(reflect.TypeFor[T](), orDefault(o))
}

func fieldsOf(t reflect.Type, o *Options) ([]Field, error) {
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("cfgfile: %v is not a struct", t)
	}
	var out []Field
	var walk func(t reflect.Type, path jwcc.Path, index [][]int, depth int, secret, noFlag, noEnv bool)
	walk = func(t reflect.Type, path jwcc.Path, index [][]int, depth int, secret, noFlag, noEnv bool) {
		for _, m := range members(t) {
			if depth == 0 && o.versioned() && m.name == o.field() {
				continue
			}
			p := append(append(jwcc.Path(nil), path...), m.name)
			ix := append(append([][]int(nil), index...), m.index)
			sec, nf, ne := secret || m.secret, noFlag || m.noFlag, noEnv || m.noEnv
			ft := m.typ
			for ft.Kind() == reflect.Pointer && !opaque(ft) {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct && !opaque(ft) && depth < 32 {
				walk(ft, p, ix, depth+1, sec, nf, ne)
				continue
			}
			f := Field{Path: p, Doc: m.doc, Secret: sec, Type: m.typ, index: ix}
			if !nf {
				f.Flag = p.String()
			}
			if !ne && o.EnvPrefix != "" {
				f.Env = envName(o.EnvPrefix, p)
			}
			out = append(out, f)
		}
	}
	walk(t, nil, nil, 0, false, false, false)
	seen := map[string]string{}
	for _, f := range out {
		if f.Env == "" {
			continue
		}
		for _, name := range []string{f.Env, f.Env + "_FILE"} {
			if prev, ok := seen[name]; ok {
				return nil, fmt.Errorf("cfgfile: %s and %s both use the environment variable %s", prev, f.Path, name)
			}
			seen[name] = f.Path.String()
		}
	}
	return out, nil
}

func envName(prefix string, p jwcc.Path) string {
	s := strings.Join(p, "_")
	s = strings.Map(func(r rune) rune {
		switch r {
		case '.', '-', '[', ']', '"', ' ':
			return '_'
		}
		return r
	}, s)
	return prefix + strings.ToUpper(s)
}

// target returns the field's value inside root, allocating nil pointers on
// the way.
func (f *Field) target(root reflect.Value) reflect.Value {
	v := root
	for i, ix := range f.index {
		if i > 0 {
			for v.Kind() == reflect.Pointer {
				if v.IsNil() {
					v.Set(reflect.New(v.Type().Elem()))
				}
				v = v.Elem()
			}
		}
		for _, j := range ix {
			for v.Kind() == reflect.Pointer {
				if v.IsNil() {
					v.Set(reflect.New(v.Type().Elem()))
				}
				v = v.Elem()
			}
			v = v.Field(j)
		}
	}
	return v
}

// peek returns the field's value inside root without allocating, and
// false if a nil pointer is on the way.
func (f *Field) peek(root reflect.Value) (reflect.Value, bool) {
	v := root
	for i, ix := range f.index {
		for k, j := range ix {
			if i > 0 || k > 0 {
				for v.Kind() == reflect.Pointer {
					if v.IsNil() {
						return v, false
					}
					v = v.Elem()
				}
			}
			v = v.Field(j)
		}
	}
	return v, true
}

// parseLeaf sets dst from s: strings verbatim, TextUnmarshalers through
// UnmarshalText, everything else as JSON text.
func parseLeaf(dst reflect.Value, s string) error {
	if dst.Kind() == reflect.Pointer {
		n := reflect.New(dst.Type().Elem())
		if err := parseLeaf(n.Elem(), s); err != nil {
			return err
		}
		dst.Set(n)
		return nil
	}
	if u, ok := dst.Addr().Interface().(encoding.TextUnmarshaler); ok {
		return u.UnmarshalText([]byte(s))
	}
	if dst.Kind() == reflect.String {
		dst.SetString(s)
		return nil
	}
	return json.Unmarshal([]byte(s), dst.Addr().Interface())
}

// formatLeaf is the inverse of parseLeaf, for flag defaults.
func formatLeaf(v reflect.Value) string {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return ""
		}
		v = v.Elem()
	}
	if m, ok := v.Interface().(encoding.TextMarshaler); ok {
		b, err := m.MarshalText()
		if err == nil {
			return string(b)
		}
	}
	if v.Kind() == reflect.String {
		return v.String()
	}
	if v.IsZero() {
		return ""
	}
	b, err := json.Marshal(v.Interface())
	if err != nil {
		return ""
	}
	return string(b)
}

// leafFlag is the flag.Value DefineFlags defines: it checks the text
// against the field's type and keeps it for Load.
type leafFlag struct {
	typ    reflect.Type
	text   string
	isBool bool
	secret bool // checked by Load, whose error hides the value
}

func (l *leafFlag) String() string {
	if l == nil {
		return ""
	}
	return l.text
}

func (l *leafFlag) Set(s string) error {
	// The flag package prints a rejected value; a secret one is checked by
	// Load instead, which keeps it out of the error.
	if l.secret {
		l.text = s
		return nil
	}
	if err := parseLeaf(reflect.New(l.typ).Elem(), s); err != nil {
		return err
	}
	l.text = s
	return nil
}

func (l *leafFlag) IsBoolFlag() bool { return l.isBool }

// DefineFlags defines one flag per leaf of T on fs (see Field), with the
// defaults from T's Defaults as default values (not for secret fields) and
// the doc tag, or else the type, as usage. Values are checked when fs is
// parsed. Set Options.Flags to fs so that Load applies the flags that
// were given; flags defined by other means but named like a field are
// applied too.
func DefineFlags[T any](fs *flag.FlagSet, o *Options) error {
	fields, err := Fields[T](o)
	if err != nil {
		return err
	}
	var def T
	if d, ok := any(&def).(Defaulter); ok {
		d.Defaults()
	}
	root := reflect.ValueOf(&def).Elem()
	for _, f := range fields {
		if f.Flag == "" {
			continue
		}
		usage := f.Doc
		if usage == "" {
			usage = typeName(f.Type)
		}
		l := &leafFlag{typ: f.Type, isBool: elem(f.Type).Kind() == reflect.Bool, secret: f.Secret}
		if v, ok := f.peek(root); ok && !f.Secret {
			l.text = formatLeaf(v)
		}
		fs.Var(l, f.Flag, usage)
	}
	return nil
}

func elem(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

func typeName(t reflect.Type) string {
	t = elem(t)
	switch {
	case opaque(t):
		return strings.ToLower(t.Name())
	case t.Kind() == reflect.Slice || t.Kind() == reflect.Array:
		return "JSON array"
	case t.Kind() == reflect.Map:
		return "JSON object"
	}
	return t.Kind().String()
}

// overlay applies the environment, then flags, to v and returns the paths
// it set.
func overlay(v reflect.Value, o *Options) ([]override, error) {
	if o.Flags == nil && o.EnvPrefix == "" {
		return nil, nil
	}
	flags := map[string]string{}
	if o.Flags != nil {
		if !o.Flags.Parsed() {
			return nil, errors.New("cfgfile: Options.Flags has not been parsed")
		}
		o.Flags.Visit(func(f *flag.Flag) { flags[f.Name] = f.Value.String() })
	}
	fields, err := fieldsOf(v.Type(), o)
	if err != nil {
		return nil, err
	}
	var set []override
	lookup := o.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	for _, f := range fields {
		var s string
		var ok bool
		var src Source
		if f.Flag != "" {
			s, ok = flags[f.Flag]
			src = Source{Kind: FromFlag, Name: f.Flag}
		}
		if !ok && f.Env != "" {
			var name string
			s, name, ok, err = envValue(lookup, f.Env)
			if err != nil {
				return nil, err
			}
			src = Source{Kind: FromEnv, Name: name}
		}
		if !ok {
			continue
		}
		if err := parseLeaf(f.target(v), s); err != nil {
			if f.Secret {
				err = errors.New("invalid value (secret value not shown)")
			}
			return nil, fmt.Errorf("cfgfile: %s: %w", src, err)
		}
		set = append(set, override{f.Path, src})
	}
	return set, nil
}

// envValue reads name, or the file named by name_FILE, and returns the
// variable used.
func envValue(lookup func(string) (string, bool), name string) (string, string, bool, error) {
	s, ok := lookup(name)
	file, fok := lookup(name + "_FILE")
	switch {
	case ok && fok:
		return "", "", false, fmt.Errorf("cfgfile: both %s and %s_FILE are set", name, name)
	case !fok:
		return s, name, ok, nil
	}
	b, err := readFile(file, nil)
	if err != nil {
		return "", "", false, fmt.Errorf("cfgfile: %s_FILE: %w", name, err)
	}
	return strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r"), name + "_FILE", true, nil
}
