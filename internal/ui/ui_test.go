package ui

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestMessageKeysMatch(t *testing.T) {
	if len(messages) != len(Languages) {
		t.Fatalf("%d message tables for %d languages", len(messages), len(Languages))
	}
	for _, l := range Languages {
		m := messages[l]
		for k, v := range messages["en"] {
			s, ok := m[k]
			if !ok {
				t.Errorf("%s missing %q", l, k)
				continue
			}
			if verbs(s) != verbs(v) {
				t.Errorf("%s: format verbs differ for %q", l, k)
			}
		}
		for k := range m {
			if _, ok := messages["en"][k]; !ok {
				t.Errorf("%s has extra key %q", l, k)
			}
		}
	}
}

// verbs lists the printf verbs in order, e.g. "%s%d".
func verbs(s string) string {
	var b strings.Builder
	for i := 0; i < len(s)-1; i++ {
		if s[i] == '%' {
			b.WriteString(s[i : i+2])
			i++
		}
	}
	return b.String()
}

func TestNormalizeLang(t *testing.T) {
	for in, want := range map[string]string{"de": "de", "pt-BR": "pt", "ZH": "zh", "xx": "en", "": "en", "tr_TR": "tr"} {
		if got := NormalizeLang(in); got != want {
			t.Errorf("NormalizeLang(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPrompts(t *testing.T) {
	var out bytes.Buffer
	u := NewWith("tr", &out, strings.NewReader("x\nh\n\n7\n2\n"))
	ctx := context.Background()
	yes, err := u.Confirm(ctx, "?", true)
	if err != nil || yes {
		t.Fatalf("confirm %v %v", yes, err)
	}
	s, err := u.Ask(ctx, "srv", "def")
	if err != nil || s != "def" {
		t.Fatalf("ask %q %v", s, err)
	}
	n, err := u.Choose(ctx, 3, 1)
	if err != nil || n != 2 {
		t.Fatalf("choose %d %v", n, err)
	}
	if _, err := u.ReadLine(ctx, ""); err != ErrAborted {
		t.Fatalf("eof: %v", err)
	}
}
