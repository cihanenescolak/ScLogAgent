package api

import (
	"encoding/json"
	"testing"
)

func TestLineJSON(t *testing.T) {
	b, err := json.Marshal([]Line{{1, "a\"b"}, {3, "ş"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `[[1,"a\"b"],[3,"ş"]]` {
		t.Fatalf("got %s", b)
	}
	var back []Line
	if err := json.Unmarshal(b, &back); err != nil || back[1].Text != "ş" || back[0].No != 1 {
		t.Fatalf("roundtrip %v %v", back, err)
	}
}

func TestVersionLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"1.0.0", "1.0.1", true},
		{"1.2.9", "1.2.10", true},
		{"v1.2", "1.2.0", false},
		{"1.3.0", "1.2.10", false},
		{"dev", "9.9.9", false},
		{"1.0.0-rc1", "1.0.1", true},
	}
	for _, c := range cases {
		if got := VersionLess(c.a, c.b); got != c.want {
			t.Errorf("VersionLess(%q,%q)=%v", c.a, c.b, got)
		}
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	cases := map[string]string{
		" sclog.example.com/ ": "https://sclog.example.com",
		"localhost:3000":       "http://localhost:3000",
		"http://x.y/":          "http://x.y",
	}
	for in, want := range cases {
		if got := NormalizeBaseURL(in); got != want {
			t.Errorf("%q → %q", in, got)
		}
	}
	if ValidateBaseURL("ftp://x") == nil || ValidateBaseURL("https://ok.example") != nil {
		t.Fatal("validate")
	}
}
