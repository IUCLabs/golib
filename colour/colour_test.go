package colour

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

var ansi = regexp.MustCompile("\x1b\\[[0-9;]*m")

// mark rewrites escapes into readable tags so golden strings stay legible.
func mark(s string) string {
	tags := map[string]string{
		reset: "</>", yqKey: "<key>", yqString: "<str>", yqNumBool: "<num>",
		yqAnchor: "<anc>", yqComment: "<com>",
		"\x1b[0;90m": "<null>", "\x1b[0;39m": "<lit>", "\x1b[0;32m": "<str>",
		"\x1b[1;39m": "<pun>", "\x1b[1;34m": "<key>",
	}
	return ansi.ReplaceAllStringFunc(s, func(e string) string {
		if t, ok := tags[e]; ok {
			return t
		}
		return e
	})
}

func render(t *testing.T, f func(*bytes.Buffer, []byte) error, in string) string {
	t.Helper()
	var out bytes.Buffer
	if err := f(&out, []byte(in)); err != nil {
		t.Fatal(err)
	}
	if got := ansi.ReplaceAllString(out.String(), ""); got != in {
		t.Fatalf("stripped output differs from input:\n%q\n%q", got, in)
	}
	return mark(out.String())
}

func jsonF(b *bytes.Buffer, in []byte) error { return JSON(b, in) }
func yamlF(b *bytes.Buffer, in []byte) error { return YAML(b, in) }

func TestJSON(t *testing.T) {
	t.Setenv("JQ_COLORS", "")
	cases := map[string]string{
		`null`:   `<null>null</>`,
		`false`:  `<lit>false</>`,
		`-1.5e3`: `<lit>-1.5e3</>`,
		"{\n  \"a\\\"b\" : \"x\",\n  \"n\": [1, true, {}]\n}\n": "<pun>{</>\n  <key>\"a\\\"b\"</> <pun>:</> <str>\"x\"</><pun>,</>\n" +
			"  <key>\"n\"</><pun>:</> <pun>[</><lit>1</><pun>,</> <lit>true</><pun>,</> <pun>{</><pun>}</><pun>]</>\n<pun>}</>\n",
	}
	for in, want := range cases {
		if got := render(t, jsonF, in); got != want {
			t.Errorf("JSON(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestJQColors(t *testing.T) {
	cases := map[string]string{
		"":                  "\x1b[0;90mnull\x1b[0m",
		"1;31":              "\x1b[1;31mnull\x1b[0m",
		"1;31:":             "\x1b[1;31mnull\x1b[0m",
		"x":                 "\x1b[0;90mnull\x1b[0m",
		"0:0:0:0:0:0:0:0:z": "\x1b[0mnull\x1b[0m",
	}
	for env, want := range cases {
		t.Setenv("JQ_COLORS", env)
		var out bytes.Buffer
		if err := JSON(&out, []byte("null")); err != nil {
			t.Fatal(err)
		}
		if out.String() != want {
			t.Errorf("JQ_COLORS=%q: got %q want %q", env, out.String(), want)
		}
	}
	// Empty middle fields become a bare ESC[m, as in jq.
	t.Setenv("JQ_COLORS", "::::::7:4")
	var out bytes.Buffer
	_ = JSON(&out, []byte(`{"k":"v"}`))
	if want := "\x1b[7m{\x1b[0m\x1b[4m\"k\"\x1b[0m\x1b[7m:\x1b[0m\x1b[m\"v\"\x1b[0m\x1b[7m}\x1b[0m"; out.String() != want {
		t.Errorf("got %q want %q", out.String(), want)
	}
}

const vaultYAML = `---
data:
  # creds
  password: "s3cr:et" # rotated
  ttl: 768h
  count: 0x1F
  ratio: -.5
  on: True
  gone: ~
  note: |-
    line one

    line: two
  folded: > # c
    text
  tags: [a, "b", 3, null]
  meta: {k: v, "q": 1, u: http://x}
  'single': 'it''s'
  empty: []
lease:
  - id: &a abc
    copy: *a
  - - nested
  - !!str 42
  -
...
`

func TestYAML(t *testing.T) {
	want := `---
<key>data</>:
  <com># creds</>
  <key>password</>: <str>"s3cr:et"</> <com># rotated</>
  <key>ttl</>: <str>768h</>
  <key>count</>: <num>0x1F</>
  <key>ratio</>: <num>-.5</>
  <key>on</>: <num>True</>
  <key>gone</>: ~
  <key>note</>: |-
    <str>line one</>

    <str>line: two</>
  <key>folded</>: > <com># c</>
    <str>text</>
  <key>tags</>: [<str>a</>, <str>"b"</>, <num>3</>, null]
  <key>meta</>: {<key>k</>: <str>v</>, <key>"q"</>: <num>1</>, <key>u</>: <str>http://x</>}
  <key>'single'</>: <str>'it''s'</>
  <key>empty</>: []
<key>lease</>:
  - <key>id</>: <anc>&a</> <str>abc</>
    <key>copy</>: <anc>*a</>
  - - <str>nested</>
  - !!str <num>42</>
  -
...
`
	if got := render(t, yamlF, vaultYAML); got != want {
		t.Errorf("YAML mismatch\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestYAMLEdges(t *testing.T) {
	cases := map[string]string{
		"a: 1\r\nb:\r\n":        "<key>a</>: <num>1</>\r\n<key>b</>:\r\n",
		"k #x: v\n":             "<str>k</> <com>#x: v</>\n",
		"--- a\n":               "--- a\n",
		"x: &anchor\n  y: 1\n":  "<key>x</>: <anc>&anchor</>\n  <key>y</>: <num>1</>\n",
		"x: !tag\n":             "<key>x</>: !tag\n",
		"x: [a] # c\n":          "<key>x</>: [<str>a</>] <com># c</>\n",
		"x: |\n  a\ny: 2\n":     "<key>x</>: |\n  <str>a</>\n<key>y</>: <num>2</>\n",
		"\"k\" : v\n":           "<key>\"k\"</> : <str>v</>\n",
		"x: \"a\\\"b\"\n":       "<key>x</>: <str>\"a\\\"b\"</>\n",
		"- [\"a\", \"b\": 1]\n": "- [<str>\"a\"</>, <key>\"b\"</>: <num>1</>]\n",
		"x: >\n":                "<key>x</>: >\n",
	}
	for in, want := range cases {
		if got := render(t, yamlF, in); got != want {
			t.Errorf("YAML(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestPassthrough(t *testing.T) {
	for _, f := range []func(*bytes.Buffer, []byte) error{jsonF, yamlF} {
		in := "{\"a\": [1,\n"
		var out bytes.Buffer
		if err := f(&out, []byte(in)); err != nil {
			t.Fatal(err)
		}
		if out.String() != in {
			t.Errorf("invalid input not passed through: %q", out.String())
		}
	}
	table := "Key      Value\n---      -----\npassword s3cret\n"
	var out bytes.Buffer
	_ = YAML(&out, []byte(table))
	if ansi.MatchString(out.String()) && ansi.ReplaceAllString(out.String(), "") != table {
		t.Errorf("table output mangled: %q", out.String())
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("boom") }

func TestWriteError(t *testing.T) {
	for _, in := range []string{"{}", "{", "a: 1", "a: ["} {
		if JSON(failWriter{}, []byte(in)) == nil || YAML(failWriter{}, []byte(in)) == nil {
			t.Errorf("write error swallowed for %q", in)
		}
	}
}

func TestEnabled(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm")
	if Enabled(Auto, f) {
		t.Error("Auto coloured a regular file")
	}
	if !Enabled(Always, f) || Enabled(Never, f) {
		t.Error("Always/Never not honoured")
	}
	tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		t.Skip("no controlling terminal")
	}
	defer func() { _ = tty.Close() }()
	if !Enabled(Auto, tty) {
		t.Error("Auto did not colour a terminal")
	}
	t.Setenv("NO_COLOR", "1")
	if Enabled(Auto, tty) || !Enabled(Always, tty) {
		t.Error("NO_COLOR not honoured, or overrode Always")
	}
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "dumb")
	if Enabled(Auto, tty) {
		t.Error("TERM=dumb coloured")
	}
}
