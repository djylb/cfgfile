package cfgfile

import (
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/djylb/cfgfile/jwcc"
)

// checkTags applies the required and enum tags to the value v as loaded
// from src. A required member must be in the file or set by a flag or the
// environment; an enum member must hold one of the listed values (checked
// on the text form: strings verbatim, TextMarshaler text, else JSON). The
// zero value of an unset enum member is accepted.
func checkTags(v reflect.Value, src *sources, required bool) error {
	return checkStruct(v, nil, src, required, false, 0)
}

func checkStruct(v reflect.Value, path jwcc.Path, src *sources, required, secret bool, depth int) error {
	if depth > 32 {
		return nil
	}
	for _, m := range members(v.Type()) {
		p := append(append(jwcc.Path(nil), path...), m.name)
		fv, ok := fieldOf(v, m.index)
		if !ok {
			continue
		}
		sec := secret || m.secret
		from := src.source(p)
		if required && m.required && from.Kind == FromDefault && !src.setBelow(p) {
			return src.errorAt(path, p, ErrRequired)
		}
		if len(m.enum) > 0 && !(from.Kind == FromDefault && fv.IsZero()) {
			text := formatLeaf(fv)
			if !slices.Contains(m.enum, text) {
				msg := fmt.Errorf("%w: %q is not one of %s", ErrEnum, text, strings.Join(m.enum, ", "))
				if sec {
					msg = fmt.Errorf("%w: value (secret, not shown) is not one of %s", ErrEnum, strings.Join(m.enum, ", "))
				}
				return src.errorAt(p, p, msg)
			}
		}
		for fv.Kind() == reflect.Pointer && !opaque(fv.Type()) {
			if fv.IsNil() {
				break
			}
			fv = fv.Elem()
		}
		if fv.Kind() == reflect.Struct && !opaque(fv.Type()) {
			if err := checkStruct(fv, p, src, required, sec, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// setBelow reports an override at or below p.
func (s *sources) setBelow(p jwcc.Path) bool {
	for _, o := range s.overrides {
		if len(o.path) >= len(p) && slices.Equal(o.path[:len(p)], p) {
			return true
		}
	}
	return false
}

// errorAt reports err for path p, positioned at where (or its nearest
// ancestor in the file) or naming the flag or variable that set it.
func (s *sources) errorAt(where, p jwcc.Path, err error) error {
	for i := len(where); i >= 0; i-- {
		src := s.source(where[:i])
		switch src.Kind {
		case FromFile:
			return &Error{File: s.file, Line: src.Line, Column: src.Column, Path: p, Err: err}
		case FromEnv, FromFlag:
			return &Error{File: src.String(), Path: p, Err: err}
		default:
		}
	}
	return &Error{File: s.file, Path: p, Err: err}
}
