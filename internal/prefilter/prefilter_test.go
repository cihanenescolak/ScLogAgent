package prefilter

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/cihanenescolak/ScLogAgent/internal/api"
)

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestHashMatchesProtocol(t *testing.T) {
	got := HashLines([]api.Line{{No: 1, Text: "a"}, {No: 3, Text: "b"}})
	if want := sha("1\ta\n3\tb\n"); got != want {
		t.Fatalf("hash %s, want %s", got, want)
	}
	if HashLines(nil) != sha("") {
		t.Fatal("empty hash mismatch")
	}
}

var testCfg = api.AgentConfig{
	IncludePatterns: []string{`<Notice>`, `\[Bad`},
	HeaderPatterns:  []string{`^Log started`, `@session`},
	HeaderMaxLines:  3,
	SessionPattern:  DefaultSessionPattern,
}

func TestFilterReader(t *testing.T) {
	f, errs := Compile(testCfg)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	in := "\xEF\xBB\xBFLog started\r\n" +
		"x @session: '01234567-89ab-cdef-0123-456789abcdef'\r\n" +
		"noise\n" +
		"Log started again (outside header window)\n" +
		"<Notice> bad \xff byte\r\n" +
		"\n" +
		"tail without newline <Notice>"
	res, err := f.FilterReader(strings.NewReader(in), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []api.Line{
		{No: 1, Text: "Log started"},
		{No: 2, Text: "x @session: '01234567-89ab-cdef-0123-456789abcdef'"},
		{No: 5, Text: "<Notice> bad \uFFFD byte"},
		{No: 7, Text: "tail without newline <Notice>"},
	}
	if len(res.Lines) != len(want) {
		t.Fatalf("lines %v", res.Lines)
	}
	for i := range want {
		if res.Lines[i] != want[i] {
			t.Fatalf("line %d = %+v, want %+v", i, res.Lines[i], want[i])
		}
	}
	if res.SessionUUID != "01234567-89ab-cdef-0123-456789abcdef" {
		t.Fatalf("session %q", res.SessionUUID)
	}
	if res.TotalLines != 7 {
		t.Fatalf("total %d", res.TotalLines)
	}
	if res.Sha256 != HashLines(want) {
		t.Fatal("sha mismatch")
	}
}

func TestInvalidPatternsSkipped(t *testing.T) {
	cfg := testCfg
	cfg.IncludePatterns = []string{`(unclosed`, `ok`}
	f, errs := Compile(cfg)
	if len(errs) != 1 {
		t.Fatalf("errs %v", errs)
	}
	if !f.Keep(100, "this is ok") || f.Keep(100, "nope") {
		t.Fatal("keep wrong")
	}
}

func TestVeryLongLine(t *testing.T) {
	f, _ := Compile(testCfg)
	long := "<Notice> " + strings.Repeat("y", 3<<20)
	res, err := f.FilterReader(strings.NewReader("a\n"+long+"\nb\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Lines) != 1 || res.Lines[0].No != 2 {
		t.Fatalf("lines %d", len(res.Lines))
	}
	if n := len(utf16.Encode([]rune(res.Lines[0].Text))); n != MaxLineChars {
		t.Fatalf("not truncated: %d", n)
	}
}

func TestDecodeLineMaximalSubpart(t *testing.T) {
	cases := map[string]string{
		"a\xC3":        "a\uFFFD",            // truncated 2-byte seq
		"\xE2\x82x":    "\uFFFDx",            // truncated 3-byte seq = one U+FFFD
		"\xF0\x80\x80": "\uFFFD\uFFFD\uFFFD", // overlong lead: each byte replaced
		"\xED\xA0\x80": "\uFFFD\uFFFD\uFFFD", // surrogate
		"ok\r":         "ok",
		"é":            "é",
	}
	for in, want := range cases {
		if got := DecodeLine([]byte(in), false); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

func TestTruncateLine(t *testing.T) {
	short := "abc"
	if TruncateLine(short) != short {
		t.Fatal("short changed")
	}
	ascii := strings.Repeat("a", MaxLineChars+10)
	if got := TruncateLine(ascii); len(got) != MaxLineChars {
		t.Fatalf("ascii len %d", len(got))
	}
	// Multi-byte but within the unit limit stays intact.
	tr := strings.Repeat("ş", MaxLineChars)
	if TruncateLine(tr) != tr {
		t.Fatal("2-byte runes within limit were cut")
	}
	// Surrogate pair straddling the limit → U+FFFD (JS slice + UTF-8 encode).
	emoji := strings.Repeat("a", MaxLineChars-1) + "😀" + "zz"
	got := TruncateLine(emoji)
	if got != strings.Repeat("a", MaxLineChars-1)+"\uFFFD" {
		t.Fatalf("emoji cut wrong: tail %q", got[len(got)-6:])
	}
	if n := len(utf16.Encode([]rune(got))); n != MaxLineChars {
		t.Fatalf("units %d", n)
	}
}
