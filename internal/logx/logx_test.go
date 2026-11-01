package logx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotation(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "agent.log")
	l, err := Open(p, 200)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for i := 0; i < 20; i++ {
		l.Printf("line %02d %s", i, strings.Repeat("x", 20))
	}
	cur, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cur) > 200 {
		t.Fatalf("current log too big: %d", len(cur))
	}
	old, err := os.ReadFile(p + ".1")
	if err != nil {
		t.Fatalf("no rotated file: %v", err)
	}
	if !strings.Contains(string(cur), "line 19") || len(old) == 0 {
		t.Fatalf("unexpected content: %q / %q", cur, old)
	}
}
