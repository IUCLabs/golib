// Package colour highlights JSON the way jq does and YAML the way yq does. It
// works on tokens and re-emits the input byte for byte between ANSI escapes, so
// formatting, whitespace and key order survive; input it cannot parse is
// written through uncoloured.
package colour

import (
	"os"
	"strings"

	"golang.org/x/term"
)

// Mode is a --colour flag's value: Auto decides from the environment.
type Mode int

const (
	Auto Mode = iota
	Always
	Never
)

const reset = "\x1b[0m"

// Enabled reports whether output to f should be coloured. Auto requires f to
// be a terminal and NO_COLOR (https://no-color.org) to be unset or empty, as jq
// does, and additionally TERM not to be "dumb".
func Enabled(mode Mode, f *os.File) bool {
	switch mode {
	case Always:
		return true
	case Never:
		return false
	}
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// jqDefault is jq's DEFAULT_COLORS: null, false, true, number, string, array,
// object, object key.
var jqDefault = [8]string{"0;90", "0;39", "0;39", "0;39", "0;32", "1;39", "1;39", "1;34"}

const (
	jqNull = iota
	jqFalse
	jqTrue
	jqNumber
	jqString
	jqArray
	jqObject
	jqKey
)

// jqColours returns jq's palette with JQ_COLORS applied as jq 1.8.1's
// jq_set_colors parses it: colon-separated runs of digits and ';' override the
// defaults in order, a trailing empty field is ignored, the eighth field ends at
// its first other character and anything after it is ignored, and any other
// character in the first seven fields discards the whole variable.
func jqColours() [8]string {
	codes := jqDefault
	spec := os.Getenv("JQ_COLORS")
	for i := 0; ; i++ {
		rest := strings.TrimLeft(spec, "0123456789;")
		field := spec[:len(spec)-len(rest)]
		if rest == "" || i+1 == len(codes) {
			if field != "" {
				codes[i] = field
			}
			break
		}
		if rest[0] != ':' {
			return sgr(jqDefault)
		}
		codes[i] = field
		spec = rest[1:]
	}
	return sgr(codes)
}

func sgr(codes [8]string) [8]string {
	for i, c := range codes {
		codes[i] = "\x1b[" + c + "m"
	}
	return codes
}
