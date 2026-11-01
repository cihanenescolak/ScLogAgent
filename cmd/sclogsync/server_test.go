package main

import (
	"testing"

	"github.com/cihanenescolak/ScLogAgent/internal/state"
)

func TestSwitchServerForgetsUploads(t *testing.T) {
	store := state.Open(t.TempDir())
	epoch := 0
	if err := store.Update(func(s *state.State) {
		s.Epoch = &epoch
		s.Files["a.log"] = &state.FileEntry{Status: state.FileComplete}
		s.Files["b.log"] = &state.FileEntry{Status: state.FileSkipped}
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &state.Config{Server: "https://old.example", Token: "tok", UserName: "me"}

	// same address: nothing changes
	if err := switchServer(store, cfg, "https://old.example"); err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "tok" || store.State().Files["a.log"].Status != state.FileComplete {
		t.Fatalf("same server changed the state: %+v", cfg)
	}

	if err := switchServer(store, cfg, "https://new.example"); err != nil {
		t.Fatal(err)
	}
	st := store.State()
	if cfg.Server != "https://new.example" || cfg.Token != "" || cfg.UserName != "" {
		t.Fatalf("config after switch: %+v", cfg)
	}
	if st.Files["a.log"].Status != state.FileHashed {
		t.Fatalf("uploaded file must be sent again, status %v", st.Files["a.log"].Status)
	}
	if st.Files["b.log"].Status != state.FileSkipped {
		t.Fatalf("skipped file changed, status %v", st.Files["b.log"].Status)
	}
	if st.Epoch != nil {
		t.Fatalf("epoch must come from the new server, got %d", *st.Epoch)
	}
}
