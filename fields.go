package cfgfile

import (
	"encoding"
	"encoding/json"
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
	name  string
	index []int
	typ   reflect.Type
	doc   string
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
				found = append(found, member{name: name, index: idx, typ: sf.Type, doc: sf.Tag.Get("doc")})
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

// Field is a leaf of a configuration type: a value that the file, a flag
// and an environment variable can each set.
type Field struct {
	// Path is the path of the value in the file, such as log.level.
	Path jwcc.Path
	// Flag is the flag name: the path in text form, such as "log.level".
	Flag string
	// Env is the environment variable: the prefix followed by the path
	// with '.', '-' and brackets turned into '_' and letters upper-cased,
	// such as APP_LOG_LEVEL.
	Env string
	// Doc is the field's doc tag.
	Doc   string
	Type  reflect.Type
	index [][]int
}

// Fields lists the leaves of T, which must be a struct. A leaf is a field
// whose type is not a struct, or decodes itself through
// encoding.TextUnmarshaler or json.Unmarshaler; slices and maps are leaves.
// The top-level version member (Options.VersionField) is not a field.
// Two leaves mapping to the same environment variable are an error.
func Fields[T any](o *Options) ([]Field, error) {
	return fieldsOf(reflect.TypeFor[T](), o)
}

func fieldsOf(t reflect.Type, o *Options) ([]Field, error) {
	envPrefix := ""
	versionField := "version"
	if o != nil {
		envPrefix, versionField = o.EnvPrefix, o.field()
	}
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("cfgfile: %v is not a struct", t)
	}
	var out []Field
	var walk func(t reflect.Type, path jwcc.Path, index [][]int, depth int)
	walk = func(t reflect.Type, path jwcc.Path, index [][]int, depth int) {
		for _, m := range members(t) {
			if depth == 0 && m.name == versionField {
				continue
			}
			p := append(append(jwcc.Path(nil), path...), m.name)
			ix := append(append([][]int(nil), index...), m.index)
			ft := m.typ
			for ft.Kind() == reflect.Pointer && !opaque(ft) {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct && !opaque(ft) && depth < 32 {
				walk(ft, p, ix, depth+1)
				continue
			}
			out = append(out, Field{Path: p, Flag: p.String(), Env: envName(envPrefix, p), Doc: m.doc, Type: m.typ, index: ix})
		}
	}
	walk(t, nil, nil, 0)
	if envPrefix == "" && o != nil && o.Flags != nil {
		return out, nil // flags only: environment names do not matter
	}
	seen := map[string]string{}
	for _, f := range out {
		if prev, ok := seen[f.Env]; ok {
			return nil, fmt.Errorf("cfgfile: %s and %s both map to %s", prev, f.Flag, f.Env)
		}
		seen[f.Env] = f.Flag
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

// Flags holds flag values bound by BindFlags.
type Flags struct {
	typ reflect.Type
	set map[string]string
}

// BindFlags defines one flag per leaf of T on fs (see Fields). Values are
// checked when the flag is parsed and applied by Load over the file and
// the environment. Flag usage is the doc tag.
// o supplies the version member to skip and may be nil.
func BindFlags[T any](fs *flag.FlagSet, o *Options) (*Flags, error) {
	fields, err := Fields[T](o)
	if err != nil {
		return nil, err
	}
	fl := &Flags{typ: reflect.TypeFor[T](), set: map[string]string{}}
	for _, f := range fields {
		usage := f.Doc
		if usage == "" {
			usage = "sets " + f.Flag
		}
		fs.Func(f.Flag, usage, func(s string) error {
			if err := parseLeaf(reflect.New(f.Type).Elem(), s); err != nil {
				return err
			}
			fl.set[f.Flag] = s
			return nil
		})
	}
	return fl, nil
}

// overlay applies flags and environment variables to v and returns the
// paths it set.
func overlay(v reflect.Value, o *Options) ([]jwcc.Path, error) {
	if o.Flags == nil && o.EnvPrefix == "" {
		return nil, nil
	}
	if o.Flags != nil && o.Flags.typ != v.Type() {
		return nil, fmt.Errorf("cfgfile: flags bound for %v, loading %v", o.Flags.typ, v.Type())
	}
	fields, err := fieldsOf(v.Type(), o)
	if err != nil {
		return nil, err
	}
	var set []jwcc.Path
	lookup := o.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	for _, f := range fields {
		var s, src string
		var ok bool
		if o.Flags != nil {
			s, ok = o.Flags.set[f.Flag]
			src = "flag -" + f.Flag
		}
		if !ok && o.EnvPrefix != "" && lookup != nil {
			s, ok = lookup(f.Env)
			src = "environment variable " + f.Env
		}
		if !ok {
			continue
		}
		if err := parseLeaf(f.target(v), s); err != nil {
			return nil, fmt.Errorf("cfgfile: %s: %w", src, err)
		}
		set = append(set, f.Path)
	}
	return set, nil
}
