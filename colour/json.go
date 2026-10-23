package colour

import (
	"bytes"
	"encoding/json"
	"io"
)

// JSON writes in to w with jq's colours, honouring JQ_COLORS. Brackets, braces,
// commas and colons take their container's colour, as in jq.
func JSON(w io.Writer, in []byte) error {
	if !json.Valid(in) {
		_, err := w.Write(in)
		return err
	}
	c := jqColours()
	var out bytes.Buffer
	var stack []byte
	paint := func(col string, tok []byte) {
		out.WriteString(col)
		out.Write(tok)
		out.WriteString(reset)
	}
	container := func() string {
		if len(stack) > 0 && stack[len(stack)-1] == '{' {
			return c[jqObject]
		}
		return c[jqArray]
	}
	for i := 0; i < len(in); {
		switch b := in[i]; b {
		case ' ', '\t', '\r', '\n':
			out.WriteByte(b)
			i++
		case '{', '[':
			stack = append(stack, b)
			paint(container(), in[i:i+1])
			i++
		case '}', ']':
			paint(container(), in[i:i+1])
			stack = stack[:len(stack)-1]
			i++
		case ',', ':':
			paint(container(), in[i:i+1])
			i++
		case '"':
			j := i + 1
			for in[j] != '"' {
				if in[j] == '\\' {
					j++
				}
				j++
			}
			j++
			col := c[jqString]
			if k := skipSpace(in, j); k < len(in) && in[k] == ':' {
				col = c[jqKey]
			}
			paint(col, in[i:j])
			i = j
		default:
			j := i
			for j < len(in) && !bytes.ContainsRune([]byte(" \t\r\n,]}"), rune(in[j])) {
				j++
			}
			col := c[jqNumber]
			switch string(in[i:j]) {
			case "null":
				col = c[jqNull]
			case "true":
				col = c[jqTrue]
			case "false":
				col = c[jqFalse]
			}
			paint(col, in[i:j])
			i = j
		}
	}
	_, err := w.Write(out.Bytes())
	return err
}

func skipSpace(in []byte, i int) int {
	for i < len(in) && (in[i] == ' ' || in[i] == '\t' || in[i] == '\r' || in[i] == '\n') {
		i++
	}
	return i
}
