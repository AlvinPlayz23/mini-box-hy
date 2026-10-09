package build

import (
	"fmt"
	"strings"
)

// ExpandArgs substitutes $VAR and ${VAR} (with ${VAR:-default} and ${VAR:+alt})
// from vars. Unknown variables expand to the empty string. It returns an error
// for malformed braces.
func ExpandArgs(s string, vars map[string]string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '$' {
			b.WriteByte(s[i])
			i++
			continue
		}
		if i+1 >= len(s) {
			b.WriteByte('$')
			break
		}
		switch n := s[i+1]; {
		case n == '$':
			b.WriteByte('$')
			i += 2
		case n == '{':
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				return "", fmt.Errorf("expand %q: missing '}' in variable reference", s)
			}
			val, err := expandBraced(s[i+2:i+2+end], vars)
			if err != nil {
				return "", err
			}
			b.WriteString(val)
			i += end + 3
		case isNameStart(n):
			j := i + 1
			for j < len(s) && isNameChar(s[j]) {
				j++
			}
			b.WriteString(vars[s[i+1:j]])
			i = j
		default:
			b.WriteByte('$')
			b.WriteByte(n)
			i += 2
		}
	}
	return b.String(), nil
}

// expandBraced expands the expression inside ${...}: NAME optionally followed
// by one of the operators ":-", "-", ":+", "+".
func expandBraced(expr string, vars map[string]string) (string, error) {
	i := 0
	for i < len(expr) && isNameChar(expr[i]) {
		i++
	}
	name, rest := expr[:i], expr[i:]
	if name == "" || !isNameStart(name[0]) {
		return "", fmt.Errorf("invalid variable name in expansion ${%s}", expr)
	}
	switch {
	case rest == "":
		return vars[name], nil
	case strings.HasPrefix(rest, ":-"):
		if v := vars[name]; v != "" {
			return v, nil
		}
		return rest[2:], nil
	case strings.HasPrefix(rest, ":+"):
		if vars[name] != "" {
			return rest[2:], nil
		}
		return "", nil
	case strings.HasPrefix(rest, "-"):
		if v, ok := vars[name]; ok {
			return v, nil
		}
		return rest[1:], nil
	case strings.HasPrefix(rest, "+"):
		if _, ok := vars[name]; ok {
			return rest[1:], nil
		}
		return "", nil
	}
	return "", fmt.Errorf("unsupported modifier in expansion ${%s}", expr)
}

func isNameStart(c byte) bool {
	return c == '_' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}

func isNameChar(c byte) bool {
	return isNameStart(c) || ('0' <= c && c <= '9')
}
