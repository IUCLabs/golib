package colour

import (
	"bytes"
	"errors"
	"io"
	"strconv"
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

// Kinds of scalar left open at the end of a line.
const (
	openNone  = 0
	openBlock = '|'
	openPlain = 'p'
)

// YAML writes in to w with yq's colours. It handles block mappings and
// sequences, plain and quoted scalars (including multi-line ones), '|' and
// '>' blocks, single-line flow collections, anchors, aliases and comments.
func YAML(w io.Writer, in []byte) error {
	if !validYAML(in) {
		_, err := w.Write(in)
		return err
	}
	var out bytes.Buffer
	// open is the scalar continuing onto the next line: a block or plain
	// scalar owned by the node at column parent, or an unterminated quote.
	open, parent := byte(openNone), 0
	for _, line := range strings.SplitAfter(string(in), "\n") {
		body := strings.TrimRight(line, "\r\n")
		eol := line[len(body):]
		trimmed := strings.TrimLeft(body, " ")
		indent := len(body) - len(trimmed)
		out.WriteString(body[:indent])
		switch {
		case open == '"' || open == '\'':
			if end := quoteClose(trimmed, open); end < 0 {
				paint(&out, yqString, trimmed)
			} else {
				paint(&out, yqString, trimmed[:end])
				comment(&out, trimmed[end:])
				open = openNone
			}
		case open != openNone && (trimmed == "" || indent > parent && (open == openBlock || trimmed[0] != '#')):
			paint(&out, yqString, trimmed)
		case trimmed == "---" || trimmed == "..." || strings.HasPrefix(trimmed, "--- "):
			out.WriteString(trimmed)
			open = openNone
		default:
			var col int
			col, open = yamlNode(&out, trimmed)
			parent = indent + col
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
	if col == "" || s == "" {
		out.WriteString(s)
		return
	}
	out.WriteString(col)
	out.WriteString(s)
	out.WriteString(reset)
}

// yamlNode colours one line's content after its indentation. It returns the
// column, relative to s, of the innermost node (the last '-' or the key) and
// the kind of scalar left open at the end of the line.
func yamlNode(out *bytes.Buffer, s string) (int, byte) {
	col, off := 0, 0
	for strings.HasPrefix(s, "- ") || s == "-" {
		n := len(s) - len(strings.TrimLeft(s[1:], " ")) // dash plus spaces
		out.WriteString(s[:n])
		col, off, s = off, off+n, s[n:]
	}
	if s == "" {
		return col, openNone
	}
	if s[0] == '#' {
		paint(out, yqComment, s)
		return col, openNone
	}
	if k := keyEnd(s); k > 0 {
		col = off
		paint(out, yqKey, s[:k])
		rest := s[k:]
		colon := strings.IndexByte(rest, ':') + 1
		out.WriteString(rest[:colon])
		rest = rest[colon:]
		value := strings.TrimLeft(rest, " ")
		out.WriteString(rest[:len(rest)-len(value)])
		s = value
	}
	return col, yamlValue(out, s)
}

// keyEnd returns the length of a mapping key at the start of s, or 0 when s is
// not a key/value pair.
func keyEnd(s string) int {
	end := 0
	switch {
	case s[0] == '"' || s[0] == '\'':
		end = quoteEnd(s)
	case s[0] == '?' && (len(s) == 1 || s[1] == ' '):
		return 0
	case strings.IndexByte("[{&*!|>%@`", s[0]) >= 0:
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
	if end := quoteClose(s[1:], s[0]); end >= 0 {
		return end + 1
	}
	return len(s)
}

// quoteClose returns the index just past the quote q closing a scalar whose
// opening quote precedes s, or -1 when s does not close it.
func quoteClose(s string, q byte) int {
	for i := 0; i < len(s); i++ {
		switch {
		case q == '"' && s[i] == '\\':
			i++
		case s[i] == q && q == '\'' && i+1 < len(s) && s[i+1] == '\'':
			i++
		case s[i] == q:
			return i + 1
		}
	}
	return -1
}

// yamlValue colours a scalar or flow collection with any trailing comment and
// returns the kind of scalar it leaves open.
func yamlValue(out *bytes.Buffer, s string) byte {
	if s == "" {
		return openNone
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
			return openNone
		}
	}
	switch s[0] {
	case '#':
		paint(out, yqComment, s)
		return openNone
	case '|', '>':
		n := strings.IndexByte(s, ' ')
		if n < 0 {
			n = len(s)
		}
		out.WriteString(s[:n])
		comment(out, s[n:])
		return openBlock
	case '[', '{':
		flow(out, s)
		return openNone
	}
	end, open := len(s), byte(openPlain)
	switch s[0] {
	case '"', '\'':
		if end, open = quoteClose(s[1:], s[0])+1, openNone; end == 0 {
			end, open = len(s), s[0]
		}
	case '*':
		open = openNone
	default:
		if i := strings.Index(s, " #"); i >= 0 {
			end, open = i, openNone
		}
	}
	val := strings.TrimRight(s[:end], " ")
	paint(out, scalarColour(val), val)
	comment(out, s[len(val):])
	return open
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
	case "null", "Null", "NULL", "~", ".inf", ".Inf", ".INF", "-.inf", "-.Inf", "-.INF", ".nan", ".NaN", ".NAN":
		return ""
	case "true", "True", "TRUE", "false", "False", "FALSE":
		return yqNumBool
	}
	if col, ok := number(v); ok {
		return col
	}
	return yqString
}

// number mirrors goccy/go-yaml v1.19.2 token.toNumber, which yq's lexer uses:
// decimal ints and floats get the number colour; hex, octal (0o or a leading
// 0) and binary ints are typed but yq leaves them uncoloured.
func number(v string) (string, bool) {
	if v == "" || v[0] == '_' || strings.Count(v, ".") > 1 {
		return "", false
	}
	neg := v[0] == '-'
	n := strings.ReplaceAll(strings.TrimPrefix(strings.TrimPrefix(v, "+"), "-"), "_", "")
	base, col := 10, ""
	switch {
	case strings.HasPrefix(n, "0x"):
		n, base = n[2:], 16
	case strings.HasPrefix(n, "0o"):
		n, base = n[2:], 8
	case strings.HasPrefix(n, "0b"):
		n, base = n[2:], 2
	case strings.HasPrefix(n, "0") && len(n) > 1 && !strings.Contains(v, "."):
		base = 8
	case strings.Contains(v, "."):
		if neg {
			n = "-" + n
		}
		_, err := strconv.ParseFloat(n, 64)
		return yqNumBool, err == nil
	default:
		col = yqNumBool
	}
	var err error
	if neg {
		_, err = strconv.ParseInt("-"+n, base, 64)
	} else {
		_, err = strconv.ParseUint(n, base, 64)
	}
	return col, err == nil
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
