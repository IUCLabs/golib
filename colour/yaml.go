package colour

import (
	"bytes"
	"errors"
	"io"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// yq's palette (pkg/yqlib/color_print.go). Null, tags, flow punctuation,
// block-scalar indicators and document markers stay uncoloured, as in yq.
const (
	yqKey     = "\x1b[36m"
	yqString  = "\x1b[32m"
	yqNumBool = "\x1b[95m"
	yqAnchor  = "\x1b[93m"
	yqComment = "\x1b[90m"
)

// YAML 1.2 core schema int and float forms.
var yamlNumber = regexp.MustCompile(`^(?:[-+]?[0-9]+|0o[0-7]+|0x[0-9a-fA-F]+|[-+]?(?:\.[0-9]+|[0-9]+(?:\.[0-9]*)?)(?:[eE][-+]?[0-9]+)?|[-+]?\.(?:inf|Inf|INF)|\.(?:nan|NaN|NAN))$`)

// YAML writes in to w with yq's colours. It handles block mappings and
// sequences, plain and quoted scalars, '|' and '>' blocks, single-line flow
// collections, anchors, aliases and comments.
func YAML(w io.Writer, in []byte) error {
	if !validYAML(in) {
		_, err := w.Write(in)
		return err
	}
	var out bytes.Buffer
	block := -1
	for _, line := range strings.SplitAfter(string(in), "\n") {
		body := strings.TrimRight(line, "\r\n")
		eol := line[len(body):]
		trimmed := strings.TrimLeft(body, " ")
		indent := len(body) - len(trimmed)
		if block >= 0 {
			if trimmed == "" || indent > block {
				out.WriteString(body[:indent])
				if trimmed != "" {
					paint(&out, yqString, trimmed)
				}
				out.WriteString(eol)
				continue
			}
			block = -1
		}
		out.WriteString(body[:indent])
		if trimmed == "---" || trimmed == "..." || strings.HasPrefix(trimmed, "--- ") {
			out.WriteString(trimmed)
		} else if yamlNode(&out, trimmed) {
			block = indent
		}
		out.WriteString(eol)
	}
	_, err := w.Write(out.Bytes())
	return err
}

func validYAML(in []byte) bool {
	dec := yaml.NewDecoder(bytes.NewReader(in))
	for {
		var n yaml.Node
		err := dec.Decode(&n)
		if errors.Is(err, io.EOF) {
			return true
		}
		if err != nil {
			return false
		}
	}
}

func paint(out *bytes.Buffer, col, s string) {
	if col == "" {
		out.WriteString(s)
		return
	}
	out.WriteString(col)
	out.WriteString(s)
	out.WriteString(reset)
}

// yamlNode colours one line's content after its indentation and reports
// whether it opens a block scalar.
func yamlNode(out *bytes.Buffer, s string) bool {
	for strings.HasPrefix(s, "- ") || s == "-" {
		n := len(s) - len(strings.TrimLeft(s[1:], " ")) // dash plus spaces
		out.WriteString(s[:n])
		s = s[n:]
	}
	if s == "" {
		return false
	}
	if s[0] == '#' {
		paint(out, yqComment, s)
		return false
	}
	if k := keyEnd(s); k > 0 {
		paint(out, yqKey, s[:k])
		rest := s[k:]
		colon := strings.IndexByte(rest, ':') + 1
		out.WriteString(rest[:colon])
		rest = rest[colon:]
		value := strings.TrimLeft(rest, " ")
		out.WriteString(rest[:len(rest)-len(value)])
		s = value
	}
	return yamlValue(out, s)
}

// keyEnd returns the length of a mapping key at the start of s, or 0 when s is
// not a key/value pair.
func keyEnd(s string) int {
	end := 0
	switch s[0] {
	case '"', '\'':
		end = quoteEnd(s)
	case '[', '{', '&', '*', '!', '|', '>', '?', '%', '@', '`':
		return 0
	default:
		for i := 0; i < len(s); i++ {
			if s[i] == ':' && (i+1 == len(s) || s[i+1] == ' ') {
				return len(strings.TrimRight(s[:i], " "))
			}
			if s[i] == '#' && i > 0 && s[i-1] == ' ' {
				return 0
			}
		}
		return 0
	}
	rest := strings.TrimLeft(s[end:], " ")
	if strings.HasPrefix(rest, ":") && (len(rest) == 1 || rest[1] == ' ') {
		return end
	}
	return 0
}

// quoteEnd returns the index just past the quoted scalar starting s, or len(s)
// when it is unterminated on this line.
func quoteEnd(s string) int {
	q := s[0]
	for i := 1; i < len(s); i++ {
		switch {
		case q == '"' && s[i] == '\\':
			i++
		case s[i] == q && q == '\'' && i+1 < len(s) && s[i+1] == '\'':
			i++
		case s[i] == q:
			return i + 1
		}
	}
	return len(s)
}

// yamlValue colours a scalar or flow collection with any trailing comment.
func yamlValue(out *bytes.Buffer, s string) bool {
	if s == "" {
		return false
	}
	for s[0] == '&' || s[0] == '!' {
		n := strings.IndexByte(s, ' ')
		if n < 0 {
			n = len(s)
		}
		if s[0] == '&' {
			paint(out, yqAnchor, s[:n])
		} else {
			out.WriteString(s[:n])
		}
		v := strings.TrimLeft(s[n:], " ")
		out.WriteString(s[n : len(s)-len(v)])
		if s = v; s == "" {
			return false
		}
	}
	switch s[0] {
	case '#':
		paint(out, yqComment, s)
		return false
	case '|', '>':
		n := strings.IndexByte(s, ' ')
		if n < 0 {
			n = len(s)
		}
		out.WriteString(s[:n])
		comment(out, s[n:])
		return true
	case '[', '{':
		flow(out, s)
		return false
	}
	end := len(s)
	if s[0] == '"' || s[0] == '\'' {
		end = quoteEnd(s)
	} else if i := strings.Index(s, " #"); i >= 0 {
		end = i
	}
	val := strings.TrimRight(s[:end], " ")
	paint(out, scalarColour(val), val)
	comment(out, s[len(val):])
	return false
}

// comment writes rest, colouring a trailing "# ..." comment.
func comment(out *bytes.Buffer, rest string) {
	i := strings.IndexByte(rest, '#')
	if i < 0 {
		out.WriteString(rest)
		return
	}
	out.WriteString(rest[:i])
	paint(out, yqComment, rest[i:])
}

func scalarColour(v string) string {
	switch v[0] {
	case '"', '\'':
		return yqString
	case '*':
		return yqAnchor
	}
	switch v {
	case "null", "Null", "NULL", "~":
		return ""
	case "true", "True", "TRUE", "false", "False", "FALSE":
		return yqNumBool
	}
	if yamlNumber.MatchString(v) {
		return yqNumBool
	}
	return yqString
}

// flow colours a single-line flow collection: punctuation plain, keys and
// scalars as in block style.
func flow(out *bytes.Buffer, s string) {
	for len(s) > 0 {
		switch c := s[0]; {
		case strings.IndexByte("[]{},: ", c) >= 0:
			out.WriteByte(c)
			s = s[1:]
		case c == '#':
			paint(out, yqComment, s)
			return
		default:
			end, scope := len(s), s
			if c == '"' || c == '\'' {
				end = quoteEnd(s)
			} else if i := strings.IndexAny(s, ",]}"); i >= 0 {
				end, scope = i, s[:i]
			}
			tok := s[:end]
			if k := keyEnd(scope); k > 0 {
				paint(out, yqKey, tok[:k])
				s = s[k:]
				continue
			}
			tok = strings.TrimRight(tok, " ")
			paint(out, scalarColour(tok), tok)
			s = s[len(tok):]
		}
	}
}
