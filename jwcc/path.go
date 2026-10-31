package jwcc

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Path addresses a value: each element is an object member name or, for an
// array, a decimal index.
//
// The text form joins plain names with '.', writes indexes as [n] and quotes
// other names as ["..."] with JSON escapes: admin.listen, peers[0].name,
// labels["a.b"]. A name made only of digits prints as [n]; on an object it
// still selects the member of that name.
type Path []string

// ParsePath parses the text form of a path. The empty string is the root.
func ParsePath(s string) (Path, error) {
	var p Path
	for i := 0; i < len(s); {
		switch {
		case s[i] == '[':
			j := i + 1
			if j < len(s) && s[j] == '"' {
				end := closingQuote(s, j)
				if end < 0 || end+1 >= len(s) || s[end+1] != ']' {
					return nil, fmt.Errorf("jwcc: bad path %q: unterminated [\"", s)
				}
				var name string
				if err := json.Unmarshal([]byte(s[j:end+1]), &name); err != nil {
					return nil, fmt.Errorf("jwcc: bad path %q: %v", s, err)
				}
				p = append(p, name)
				i = end + 2
			} else {
				end := strings.IndexByte(s[j:], ']')
				if end < 0 {
					return nil, fmt.Errorf("jwcc: bad path %q: unterminated [", s)
				}
				if _, ok := arrayIndex(s[j : j+end]); !ok {
					return nil, fmt.Errorf("jwcc: bad path %q: bad index", s)
				}
				p = append(p, s[j:j+end])
				i = j + end + 1
			}
		case s[i] == '.' && i > 0:
			i++
			fallthrough
		default:
			j := i
			for j < len(s) && s[j] != '.' && s[j] != '[' {
				j++
			}
			if j == i {
				return nil, fmt.Errorf("jwcc: bad path %q: empty name", s)
			}
			p = append(p, s[i:j])
			i = j
		}
	}
	return p, nil
}

func closingQuote(s string, i int) int {
	for j := i + 1; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case '"':
			return j
		}
	}
	return -1
}

func (p Path) String() string {
	var b strings.Builder
	for i, s := range p {
		switch {
		case s != "" && isDigits(s):
			b.WriteString("[" + s + "]")
		case plainName(s):
			if i > 0 {
				b.WriteByte('.')
			}
			b.WriteString(s)
		default:
			b.WriteString("[" + string(quote(s)) + "]")
		}
	}
	return b.String()
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func plainName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c == '_' || c == '-' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func arrayIndex(s string) (int, bool) {
	if s == "" || !isDigits(s) || len(s) > 1 && s[0] == '0' {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

func itoa(i int) string { return strconv.Itoa(i) }

// quote returns s as a JSON string without HTML escaping.
func quote(s string) []byte {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return []byte(strings.TrimSuffix(b.String(), "\n"))
}
