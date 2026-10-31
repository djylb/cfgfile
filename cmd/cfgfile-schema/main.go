// Command cfgfile-schema writes a JSON Schema (draft 2020-12) for a
// configuration struct, for editor completion and checking. It reads the Go
// source of one package, so it also works for package main:
//
//	//go:generate go run github.com/djylb/cfgfile/cmd/cfgfile-schema -type Config -o app.schema.json
//
// Members come from json tags as encoding/json reads them. Tags add:
//
//	doc:"text"         description (else the field's comment)
//	enum:"a,b,c"       allowed values
//	default:"value"    default (JSON text, else a string)
//
// Types with an UnmarshalText method, cfgfile.Duration and cfgfile.Size are
// strings; types from other packages are unconstrained. Objects reject
// unknown members, as Load does; the top level also allows "$schema".
// The schema only serves editors: Load does not read it.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

func main() {
	typ := flag.String("type", "", "struct type name (required)")
	dir := flag.String("dir", ".", "package directory")
	out := flag.String("o", "", "output file (default stdout)")
	id := flag.String("id", "", "$id of the schema")
	vf := flag.String("version-field", "version", "top-level version member (always allowed)")
	flag.Parse()
	if *typ == "" {
		flag.Usage()
		os.Exit(2)
	}
	b, err := generate(*dir, *typ, *id, *vf)
	if err == nil {
		if *out == "" {
			_, err = os.Stdout.Write(b)
		} else {
			err = os.WriteFile(*out, b, 0o644)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "cfgfile-schema:", err)
		os.Exit(1)
	}
}

// obj is a JSON object that keeps member order.
type obj []kv

type kv struct {
	k string
	v any
}

func (o obj) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, e := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(e.k)
		v, err := json.Marshal(e.v)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

type gen struct {
	types    map[string]*ast.TypeSpec
	text     map[string]bool // has UnmarshalText
	defs     obj
	defined  map[string]bool
	visiting map[string]bool
}

func generate(dir, typ, id, versionField string) ([]byte, error) {
	g := &gen{types: map[string]*ast.TypeSpec{}, text: map[string]bool{}, defined: map[string]bool{}, visiting: map[string]bool{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, n), nil, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.GenDecl:
				for _, s := range d.Specs {
					if ts, ok := s.(*ast.TypeSpec); ok {
						g.types[ts.Name.Name] = ts
					}
				}
			case *ast.FuncDecl:
				if d.Recv != nil && len(d.Recv.List) == 1 && d.Name.Name == "UnmarshalText" {
					t := d.Recv.List[0].Type
					if s, ok := t.(*ast.StarExpr); ok {
						t = s.X
					}
					if id, ok := t.(*ast.Ident); ok {
						g.text[id.Name] = true
					}
				}
			}
		}
	}
	ts, ok := g.types[typ]
	if !ok {
		return nil, fmt.Errorf("type %s not found in %s", typ, dir)
	}
	st, ok := ts.Type.(*ast.StructType)
	if !ok {
		return nil, fmt.Errorf("%s is not a struct", typ)
	}
	root := obj{{"$schema", "https://json-schema.org/draft/2020-12/schema"}}
	if id != "" {
		root = append(root, kv{"$id", id})
	}
	root = append(root, kv{"title", typ})
	body := g.object(st, true)
	props := body[1].v.(obj)
	if !slices.ContainsFunc(props, func(e kv) bool { return e.k == versionField }) {
		props = slices.Insert(props, 1, kv{versionField, obj{{"type", "integer"}, {"minimum", 1}}})
		body[1].v = props
	}
	root = append(root, body...)
	if len(g.defs) > 0 {
		root = append(root, kv{"$defs", g.defs})
	}
	var b bytes.Buffer
	raw, err := json.Marshal(root)
	if err != nil {
		return nil, err
	}
	if err := json.Indent(&b, raw, "", "  "); err != nil {
		return nil, err
	}
	b.WriteByte('\n')
	return b.Bytes(), nil
}

func (g *gen) object(st *ast.StructType, top bool) obj {
	props := obj{}
	if top {
		props = append(props, kv{"$schema", obj{{"type", "string"}}})
	}
	g.fields(st, &props)
	return obj{{"type", "object"}, {"properties", props}, {"additionalProperties", false}}
}

// fields adds the members of st: its own fields first, then those of
// embedded structs, so that shallower fields win as in encoding/json.
func (g *gen) fields(st *ast.StructType, props *obj) {
	var embedded []*ast.StructType
	for _, f := range st.Fields.List {
		tag := reflect.StructTag("")
		if f.Tag != nil {
			s, _ := strconv.Unquote(f.Tag.Value)
			tag = reflect.StructTag(s)
		}
		jt := tag.Get("json")
		if jt == "-" {
			continue
		}
		name, _, _ := strings.Cut(jt, ",")
		if len(f.Names) == 0 { // embedded
			t := f.Type
			if s, ok := t.(*ast.StarExpr); ok {
				t = s.X
			}
			if id, ok := t.(*ast.Ident); ok && name == "" {
				if ts, ok := g.types[id.Name]; ok {
					if est, ok := ts.Type.(*ast.StructType); ok {
						embedded = append(embedded, est)
						continue
					}
				}
			}
			if name == "" {
				name = exprName(t)
			}
			g.add(props, name, f, tag)
			continue
		}
		for _, n := range f.Names {
			if !n.IsExported() {
				continue
			}
			nm := name
			if nm == "" {
				nm = n.Name
			}
			g.add(props, nm, f, tag)
		}
	}
	for _, est := range embedded {
		g.fields(est, props)
	}
}

// nullable also allows null, as Load does for pointers, slices and maps.
func nullable(s obj) obj {
	for i, e := range s {
		if e.k == "type" {
			if t, ok := e.v.(string); ok {
				s[i].v = []string{t, "null"}
			}
			return s
		}
		if e.k == "$ref" {
			return obj{{"anyOf", []obj{s, {{"type", "null"}}}}}
		}
	}
	return s // unconstrained already allows null
}

func exprText(e ast.Expr) string {
	if b, ok := e.(*ast.BasicLit); ok {
		return b.Value
	}
	return ""
}

func exprName(t ast.Expr) string {
	switch t := t.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return t.Sel.Name
	}
	return ""
}

func (g *gen) add(props *obj, name string, f *ast.Field, tag reflect.StructTag) {
	s := g.schema(f.Type)
	desc := tag.Get("doc")
	if desc == "" {
		for _, c := range []*ast.CommentGroup{f.Doc, f.Comment} {
			if c != nil {
				desc = strings.TrimSpace(c.Text())
				break
			}
		}
	}
	if desc != "" {
		s = append(obj{{"description", desc}}, s...)
	}
	if e := tag.Get("enum"); e != "" {
		var vals []any
		for _, v := range strings.Split(e, ",") {
			vals = append(vals, literal(v, s))
		}
		s = append(s, kv{"enum", vals})
	}
	if d, ok := tag.Lookup("default"); ok {
		s = append(s, kv{"default", literal(d, s)})
	}
	for i := range *props {
		if (*props)[i].k == name {
			return // shallower or earlier field wins
		}
	}
	*props = append(*props, kv{name, s})
}

// literal reads a tag value as JSON unless the schema is a string.
func literal(v string, s obj) any {
	for _, e := range s {
		if e.k == "type" && e.v == "string" {
			return v
		}
	}
	var x any
	if json.Unmarshal([]byte(v), &x) == nil {
		return x
	}
	return v
}

func (g *gen) schema(t ast.Expr) obj {
	switch t := t.(type) {
	case *ast.StarExpr:
		return nullable(g.schema(t.X))
	case *ast.ArrayType:
		if t.Len != nil { // fixed size: not null, at most Len elements
			s := obj{{"type", "array"}, {"items", g.schema(t.Elt)}}
			if n, err := strconv.Atoi(exprText(t.Len)); err == nil {
				s = append(s, kv{"maxItems", n})
			}
			return s
		}
		if id, ok := t.Elt.(*ast.Ident); ok && id.Name == "byte" {
			return nullable(obj{{"type", "string"}, {"contentEncoding", "base64"}})
		}
		return nullable(obj{{"type", "array"}, {"items", g.schema(t.Elt)}})
	case *ast.MapType:
		return nullable(obj{{"type", "object"}, {"additionalProperties", g.schema(t.Value)}})
	case *ast.StructType:
		return g.object(t, false)
	case *ast.InterfaceType:
		return obj{}
	case *ast.SelectorExpr:
		switch t.Sel.Name {
		case "Duration":
			if x, ok := t.X.(*ast.Ident); ok && x.Name == "time" {
				return obj{{"type", "integer"}}
			}
			return obj{{"type", "string"}, {"pattern", `^(0|[-+]?([0-9]*(\.[0-9]*)?(ns|us|µs|ms|s|m|h))+)$`}}
		case "Size":
			return obj{{"type", "string"}, {"pattern", `^[0-9]+ ?([KMGTP]i?B|B)?$`}}
		}
		return obj{}
	case *ast.Ident:
		switch t.Name {
		case "string":
			return obj{{"type", "string"}}
		case "bool":
			return obj{{"type", "boolean"}}
		case "int", "int8", "int16", "int32", "int64", "uint", "uint8", "uint16", "uint32", "uint64", "uintptr", "byte", "rune":
			return obj{{"type", "integer"}}
		case "float32", "float64":
			return obj{{"type", "number"}}
		case "any":
			return obj{}
		}
		if g.text[t.Name] {
			return obj{{"type", "string"}}
		}
		ts, ok := g.types[t.Name]
		if !ok {
			return obj{}
		}
		if _, isStruct := ts.Type.(*ast.StructType); !isStruct {
			if g.visiting[t.Name] {
				return obj{}
			}
			g.visiting[t.Name] = true
			defer delete(g.visiting, t.Name)
			return g.schema(ts.Type)
		}
		if !g.defined[t.Name] {
			g.defined[t.Name] = true
			i := len(g.defs)
			g.defs = append(g.defs, kv{t.Name, nil})
			g.defs[i].v = g.schema(ts.Type)
		}
		return obj{{"$ref", "#/$defs/" + t.Name}}
	}
	return obj{}
}
