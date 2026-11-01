package tail

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

var sessRe = regexp.MustCompile(`@session:\s+'([0-9a-f-]{36})'`)

func sessionOf(s string) string {
	if m := sessRe.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

const (
	s1 = "11111111-1111-1111-1111-111111111111"
	s2 = "22222222-2222-2222-2222-222222222222"
)

func appendFile(t *testing.T, p, s string) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func TestTailFollowAndRotate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "Game.log")
	tl := New(p, sessionOf)

	if r, _ := tl.Poll(); !r.Missing {
		t.Fatal("expected missing")
	}

	appendFile(t, p, "head\r\n@session: '"+s1+"'\npartial")
	r, err := tl.Poll()
	if err != nil {
		t.Fatal(err)
	}
	if !r.Rotated || len(r.Lines) != 2 || r.Lines[0].Text != "head" || tl.Session() != s1 {
		t.Fatalf("first poll %+v", r)
	}
	appendFile(t, p, " line\nnext\n")
	r, _ = tl.Poll()
	if r.Rotated || len(r.Lines) != 2 || r.Lines[0].No != 3 || r.Lines[0].Text != "partial line" {
		t.Fatalf("append poll %+v", r)
	}
	if r, _ = tl.Poll(); len(r.Lines) != 0 || r.Rotated {
		t.Fatalf("idle poll %+v", r)
	}

	// Truncation (smaller file) → rotated, from line 1.
	if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, _ = tl.Poll()
	if !r.Rotated || len(r.Lines) != 1 || r.Lines[0].No != 1 {
		t.Fatalf("truncate poll %+v", r)
	}

	// Same-size-or-larger rewrite with a different session line → rotated.
	content := "@session: '" + s1 + "'\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	tl.Reset()
	if r, _ = tl.Poll(); tl.Session() != s1 {
		t.Fatalf("session %q", tl.Session())
	}
	if err := os.WriteFile(p+".tmp", []byte("@session: '"+s2+"'\nmore lines here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(p+".tmp", p); err != nil {
		t.Fatal(err)
	}
	r, _ = tl.Poll()
	if !r.Rotated || r.Lines[0].No != 1 || tl.Session() != s2 {
		t.Fatalf("replace poll %+v session %s", r, tl.Session())
	}

	// File moved away (game closed) → missing, then a new file starts at line 1.
	if err := os.Rename(p, filepath.Join(dir, "backup.log")); err != nil {
		t.Fatal(err)
	}
	if r, _ = tl.Poll(); !r.Missing {
		t.Fatal("expected missing after move")
	}
	appendFile(t, p, "new\n")
	if r, _ = tl.Poll(); !r.Rotated || r.Lines[0].Text != "new" || r.Lines[0].No != 1 {
		t.Fatalf("new file poll %+v", r)
	}
}

func TestSameSessionRewriteDetectedByProbe(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "Game.log")
	appendFile(t, p, "@session: '"+s1+"'\naaaa\n")
	tl := New(p, sessionOf)
	if _, err := tl.Poll(); err != nil {
		t.Fatal(err)
	}
	// Overwrite in place (same inode) with a longer file of another session.
	if err := os.WriteFile(p, []byte("@session: '"+s2+"'\nbbbbbbbbbbbbbbbbbbbb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, _ := tl.Poll()
	if !r.Rotated || tl.Session() != s2 || len(r.Lines) != 2 {
		t.Fatalf("probe rotation %+v", r)
	}
}
