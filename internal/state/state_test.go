package state

import "testing"

func TestConfigTokenAndState(t *testing.T) {
	s := Open(t.TempDir())
	c, err := s.LoadConfig()
	if err != nil || c.Server != "" {
		t.Fatalf("empty load %+v %v", c, err)
	}
	c.Server, c.Folder = "https://x", `C:\SC\LIVE`
	if err := SetToken(c, "secret"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveConfig(c); err != nil {
		t.Fatal(err)
	}
	c2, _ := s.LoadConfig()
	if tok, err := Token(c2); err != nil || tok != "secret" || c2.Token == "secret" {
		t.Fatalf("token %q %v (stored %q)", tok, err, c2.Token)
	}
	c2.Token = "bogus:xx"
	if _, err := Token(c2); err == nil {
		t.Fatal("unsupported scheme accepted")
	}

	one := 1
	_ = s.Update(func(st *State) {
		st.Epoch = &one
		st.Files["a"] = &FileEntry{Status: FileComplete}
		st.Files["b"] = &FileEntry{Status: FileSkipped}
		st.LastUpload = &LastUpload{At: 1}
	})
	two := 2
	if err := s.ResetSent(&two); err != nil {
		t.Fatal(err)
	}
	st := Open(s.Dir).State()
	if *st.Epoch != 2 || st.Files["a"].Status != FileHashed || st.Files["b"].Status != FileSkipped || st.LastUpload != nil {
		t.Fatalf("after reset %+v", st)
	}
}
