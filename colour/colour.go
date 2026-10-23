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

// Enabled reports whether output to f should be coloured. Auto follows jq:
// f must be a terminal, NO_COLOR (https://no-color.org) must be unset or empty
// and TERM must not be "dumb".
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

// jqColours returns jq's palette with JQ_COLORS applied the way jq parses it:
// up to eight colon-separated SGR codes override the defaults in order, a
// trailing empty field is ignored, and anything but digits and ';' discards the
// whole variable.
func jqColours() [8]string {
	codes := jqDefault
	spec := os.Getenv("JQ_COLORS")
	if spec == "" {
		return sgr(codes)
	}
	fields := strings.Split(spec, ":")
	if fields[len(fields)-1] == "" {
		fields = fields[:len(fields)-1]
	}
	if len(fields) > len(codes) {
		fields = fields[:len(codes)]
	}
	for i, f := range fields {
		if strings.Trim(f, "0123456789;") != "" {
			return sgr(jqDefault)
		}
		codes[i] = f
	}
	return sgr(codes)
}

func sgr(codes [8]string) [8]string {
	for i, c := range codes {
		codes[i] = "\x1b[" + c + "m"
	}
	return codes
}
