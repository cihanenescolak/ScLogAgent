package detect

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func mk(t *testing.T, p string, mt time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !mt.IsZero() {
		_ = os.Chtimes(p, mt, mt)
	}
}

func TestScanAndNormalize(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "Program Files", "Roberts Space Industries", "StarCitizen")
	live := filepath.Join(base, "LIVE")
	ptu := filepath.Join(base, "PTU")
	now := time.Now()
	mk(t, filepath.Join(live, "Game.log"), time.Time{})
	mk(t, filepath.Join(live, "logbackups", "b.log"), now.Add(-time.Hour))
	mk(t, filepath.Join(live, "logbackups", "a.log"), now.Add(-2*time.Hour))
	mk(t, filepath.Join(live, "logbackups", "ignore.txt"), time.Time{})
	mk(t, filepath.Join(ptu, "logbackups", "c.log"), time.Time{})
	mk(t, filepath.Join(ptu, "logbackups", "d.log"), time.Time{})
	mk(t, filepath.Join(ptu, "logbackups", "e.log"), time.Time{})

	got := Scan([]string{root}, nil)
	if len(got) != 2 || got[0].Path != live || !got[0].HasGameLog || got[0].Backups != 2 || got[1].Environment != "PTU" {
		t.Fatalf("scan %+v", got)
	}
	files := BackupFiles(filepath.Join(live, "logbackups"))
	want := []string{filepath.Join(live, "logbackups", "a.log"), filepath.Join(live, "logbackups", "b.log")}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("backups %v", files)
	}

	for _, in := range []string{live, `"` + live + `"`, filepath.Join(live, "Game.log"), filepath.Join(live, "logbackups")} {
		if p, ok := Normalize(in); !ok || p != live {
			t.Errorf("Normalize(%q) = %q %v", in, p, ok)
		}
	}
	if _, ok := Normalize(base); ok {
		t.Error("root with two environments must be ambiguous")
	}
	if _, ok := Normalize(filepath.Join(root, "nothing")); ok {
		t.Error("empty dir accepted")
	}
}

func TestLauncherInstallRoots(t *testing.T) {
	text := `{"library":{"defaultPath":"D:\\Games\\Roberts Space Industries\\StarCitizen\\LIVE","x":"C:/Program Files/Roberts Space Industries/StarCitizen"}}`
	got := LauncherInstallRoots(text)
	want := []string{`D:\Games\Roberts Space Industries\StarCitizen`, `C:\Program Files\Roberts Space Industries\StarCitizen`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
}

func TestTestFolders(t *testing.T) {
	root := t.TempDir()
	for _, env := range []string{"LIVE", "PTU", "EPTU"} {
		if err := os.MkdirAll(filepath.Join(root, env, "logbackups"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got := TestFolders(filepath.Join(root, "LIVE"))
	if len(got) != 2 || got[0].Environment != "PTU" || got[1].Environment != "EPTU" {
		t.Fatalf("got %+v", got)
	}
	if TestFolders("") != nil {
		t.Fatal("empty folder must give nil")
	}
}
