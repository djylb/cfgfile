package cfgfile

import (
	"fmt"
	"slices"

	"github.com/djylb/cfgfile/jwcc"
)

// SourceKind is where a value came from.
type SourceKind int

const (
	// FromDefault means neither the file, the environment nor a flag set it.
	FromDefault SourceKind = iota
	// FromFile means the file holds it, at Source.File:Line:Column.
	FromFile
	// FromEnv means it came from the environment variable Source.Name (possibly NAME_FILE).
	FromEnv
	// FromFlag means it came from the flag -Source.Name.
	FromFlag
)

// Source tells where a value came from. It never holds the value, so it is
// safe to show for secrets.
type Source struct {
	Kind         SourceKind
	File         string
	Line, Column int
	Name         string
}

func (s Source) String() string {
	switch s.Kind {
	case FromFile:
		return fmt.Sprintf("%s:%d:%d", s.File, s.Line, s.Column)
	case FromEnv:
		return "environment variable " + s.Name
	case FromFlag:
		return "flag -" + s.Name
	default:
		return "default"
	}
}

type override struct {
	path jwcc.Path
	src  Source
}

// sources answers Source queries for one load.
type sources struct {
	file      string
	text      []byte      // the document as loaded (after migration)
	doc       *jwcc.Value // parsed text, nil without a file
	overrides []override
}

func (s *sources) source(p jwcc.Path) Source {
	for _, o := range s.overrides {
		if slices.Equal(o.path, p) {
			return o.src
		}
	}
	if s.doc != nil {
		if v := s.doc.Find(p); v != nil && v.Start >= 0 {
			l, c := jwcc.Position(s.text, v.Start)
			return Source{Kind: FromFile, File: s.file, Line: l, Column: c}
		}
	}
	return Source{Kind: FromDefault}
}

func (s *sources) paths() []jwcc.Path {
	out := make([]jwcc.Path, len(s.overrides))
	for i, o := range s.overrides {
		out[i] = o.path
	}
	return out
}

// Source returns where the value at p came from: a flag, an environment
// variable, a position in the file, or the default. A position refers to
// the document as loaded; after an in-memory migration that is Result.Text,
// not the file on disk.
func (r *Result[T]) Source(p jwcc.Path) Source { return r.src.source(p) }

// Source returns where the value in effect at p came from; see
// Result.Source.
func (f *File[T]) Source(p jwcc.Path) Source { return f.Result().Source(p) }
