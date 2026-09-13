// Package jsonx gives adapters JSON they can use from bash: get a value by
// path, set a value by path, escape a string. Paths look like
// .stable.headless, .mods[2].name, .[0], .versions.[1].id, or
// ["1963720"].depots["1963722"].manifests.public.gid.
package jsonx

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type segment struct {
	key   string
	index int
	isIdx bool
}

func parsePath(path string) ([]segment, error) {
	var segs []segment
	s := strings.TrimSpace(path)
	if s == "." || s == "" {
		return segs, nil
	}
	for len(s) > 0 {
		switch {
		case s[0] == '.':
			s = s[1:]
			// jq spelling: ".[0]" and a trailing "." carry no key of their own.
			if s == "" || s[0] == '[' {
				continue
			}
			end := strings.IndexAny(s, ".[")
			if end < 0 {
				end = len(s)
			}
			if end == 0 {
				return nil, fmt.Errorf("empty key in path %q", path)
			}
			segs = append(segs, segment{key: s[:end]})
			s = s[end:]
		case s[0] == '[':
			close := strings.IndexByte(s, ']')
			if close < 0 {
				return nil, fmt.Errorf("unterminated [ in path %q", path)
			}
			inner := s[1:close]
			s = s[close+1:]
			if len(inner) >= 2 && inner[0] == '"' && inner[len(inner)-1] == '"' {
				segs = append(segs, segment{key: inner[1 : len(inner)-1]})
			} else {
				n, err := strconv.Atoi(inner)
				if err != nil {
					return nil, fmt.Errorf("bad index %q in path %q", inner, path)
				}
				segs = append(segs, segment{index: n, isIdx: true})
			}
		default:
			return nil, fmt.Errorf("unexpected %q in path %q", s[:1], path)
		}
	}
	return segs, nil
}

var ErrMissing = errors.New("path not found")

func walk(v any, segs []segment) (any, error) {
	for _, sg := range segs {
		switch cur := v.(type) {
		case map[string]any:
			if sg.isIdx {
				return nil, ErrMissing
			}
			next, ok := cur[sg.key]
			if !ok {
				return nil, ErrMissing
			}
			v = next
		case []any:
			if !sg.isIdx || sg.index < 0 || sg.index >= len(cur) {
				return nil, ErrMissing
			}
			v = cur[sg.index]
		default:
			return nil, ErrMissing
		}
	}
	return v, nil
}

// Get returns the value at path as text: strings unquoted, scalars as their
// JSON text, objects and arrays as compact JSON. Missing → ErrMissing.
func Get(data []byte, path string) (string, error) {
	segs, err := parsePath(path)
	if err != nil {
		return "", err
	}
	var root any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return "", fmt.Errorf("invalid JSON: %w", err)
	}
	v, err := walk(root, segs)
	if err != nil {
		return "", err
	}
	return Text(v), nil
}

// Text renders a decoded value the way Get does.
func Text(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	case json.Number:
		return t.String()
	case bool:
		return strconv.FormatBool(t)
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

// Set assigns value at path, creating intermediate objects. When raw is
// true the value is parsed as JSON; otherwise it is stored as a string.
func Set(data []byte, path, value string, raw bool) ([]byte, error) {
	segs, err := parsePath(path)
	if err != nil {
		return nil, err
	}
	if len(segs) == 0 {
		return nil, errors.New("cannot set the root")
	}
	var root any
	if len(bytes.TrimSpace(data)) == 0 {
		root = map[string]any{}
	} else {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		if err := dec.Decode(&root); err != nil {
			return nil, fmt.Errorf("invalid JSON: %w", err)
		}
	}
	var val any = value
	if raw {
		dec := json.NewDecoder(strings.NewReader(value))
		dec.UseNumber()
		if err := dec.Decode(&val); err != nil {
			return nil, fmt.Errorf("value is not JSON: %w", err)
		}
	}
	root, err = assign(root, segs, val)
	if err != nil {
		return nil, err
	}
	return marshalIndent(root)
}

func assign(v any, segs []segment, val any) (any, error) {
	if len(segs) == 0 {
		return val, nil
	}
	sg := segs[0]
	if sg.isIdx {
		arr, ok := v.([]any)
		if !ok {
			arr = []any{}
		}
		for len(arr) <= sg.index {
			arr = append(arr, nil)
		}
		next, err := assign(arr[sg.index], segs[1:], val)
		if err != nil {
			return nil, err
		}
		arr[sg.index] = next
		return arr, nil
	}
	obj, ok := v.(map[string]any)
	if !ok {
		obj = map[string]any{}
	}
	next, err := assign(obj[sg.key], segs[1:], val)
	if err != nil {
		return nil, err
	}
	obj[sg.key] = next
	return obj, nil
}

func marshalIndent(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Escape returns s as a JSON string literal, quotes included.
func Escape(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.Encode(s)
	return strings.TrimRight(buf.String(), "\n")
}

// Array renders strings as a JSON array literal.
func Array(items []string) string {
	if items == nil {
		items = []string{}
	}
	b, _ := json.Marshal(items)
	return string(b)
}

// Valid reports whether data parses as JSON.
func Valid(data []byte) bool { return json.Valid(data) }
