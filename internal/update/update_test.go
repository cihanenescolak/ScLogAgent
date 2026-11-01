package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cihanenescolak/ScLogAgent/internal/api"
)

func offer(body []byte) *api.AgentUpdate {
	sum := sha256.Sum256(body)
	return &api.AgentUpdate{Version: "1.3.0", URL: "/download/sclogsync.exe", Sha256: hex.EncodeToString(sum[:]), Size: int64(len(body))}
}

func TestAvailable(t *testing.T) {
	u := offer([]byte("exe"))
	cfg := &api.AgentConfig{AgentUpdate: u}
	if Available(cfg, "1.2.0") != u {
		t.Fatal("1.2.0 should update to 1.3.0")
	}
	for _, cur := range []string{"1.3.0", "1.4.0", "dev"} {
		if Available(cfg, cur) != nil {
			t.Fatalf("%s must not update", cur)
		}
	}
	if Available(&api.AgentConfig{}, "1.0.0") != nil || Available(nil, "1.0.0") != nil {
		t.Fatal("no offer → no update")
	}
	bad := *u
	bad.Sha256 = "abc"
	if Available(&api.AgentConfig{AgentUpdate: &bad}, "1.0.0") != nil {
		t.Fatal("malformed offer must be ignored")
	}
}

func serve(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/download/sclogsync.exe" {
			http.NotFound(w, r)
			return
		}
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDownloadVerifies(t *testing.T) {
	body := []byte(strings.Repeat("MZ new build ", 100))
	srv := serve(t, body)
	dir := t.TempDir()

	path, err := Download(context.Background(), nil, srv.URL, "test", offer(body), dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != string(body) {
		t.Fatal("downloaded content differs")
	}
	os.Remove(path)

	wrongSum := offer(body)
	wrongSum.Sha256 = strings.Repeat("0", 64)
	if _, err := Download(context.Background(), nil, srv.URL, "", wrongSum, dir); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("checksum mismatch not detected: %v", err)
	}
	wrongSize := offer(body)
	wrongSize.Size--
	if _, err := Download(context.Background(), nil, srv.URL, "", wrongSize, dir); err == nil || !strings.Contains(err.Error(), "size") {
		t.Fatalf("size mismatch not detected: %v", err)
	}
	foreign := offer(body)
	foreign.URL = "https://evil.example/sclogsync.exe"
	if _, err := Download(context.Background(), nil, srv.URL, "", foreign, dir); err == nil {
		t.Fatal("download from another host must be refused")
	}
	notAsset := offer(body)
	notAsset.URL = "https://github.com/someone/repo/raw/main/sclogsync.exe"
	if _, err := Download(context.Background(), nil, srv.URL, "", notAsset, dir); err == nil {
		t.Fatal("only GitHub release assets are allowed")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*.new")); len(left) != 0 {
		t.Fatalf("failed downloads left temp files: %v", left)
	}
}

func TestResolve(t *testing.T) {
	ok := map[string]string{
		"/download/sclogsync.exe": "https://sclog.example.com/download/sclogsync.exe",
		"https://github.com/cihanenescolak/ScLogAgent/releases/download/v1.0.1/sclogsync.exe": "https://github.com/cihanenescolak/ScLogAgent/releases/download/v1.0.1/sclogsync.exe",
	}
	for ref, want := range ok {
		got, err := resolve("https://sclog.example.com", ref)
		if err != nil || got != want {
			t.Fatalf("resolve(%q) = %q, %v", ref, got, err)
		}
	}
	for _, ref := range []string{
		"http://github.com/a/b/releases/download/v1/sclogsync.exe",
		"https://github.com/a/b/releases/download/sclogsync.exe",
		"https://objects.githubusercontent.com/x/sclogsync.exe",
		"https://evil.example/download/sclogsync.exe",
	} {
		if _, err := resolve("https://sclog.example.com", ref); err == nil {
			t.Fatalf("resolve(%q) should be refused", ref)
		}
	}
}

func TestInstallAndCleanup(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "sclogsync.exe")
	os.WriteFile(exe, []byte("old"), 0o755)
	newPath := filepath.Join(dir, "sclogsync-1.exe.new")
	os.WriteFile(newPath, []byte("new"), 0o600) // like os.CreateTemp

	if err := Install(newPath, exe); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "new" {
		t.Fatalf("exe = %q", got)
	}
	if st, _ := os.Stat(exe); st.Mode().Perm() != 0o755 {
		t.Fatalf("new exe mode %v, want the old exe's 0755", st.Mode().Perm())
	}
	if got, _ := os.ReadFile(exe + ".old"); string(got) != "old" {
		t.Fatalf("old copy = %q", got)
	}
	Cleanup(exe)
	if _, err := os.Stat(exe + ".old"); !os.IsNotExist(err) {
		t.Fatal("cleanup should remove the old copy")
	}

	// a missing new file puts the current exe back
	if err := Install(filepath.Join(dir, "missing.exe.new"), exe); err == nil {
		t.Fatal("expected an error")
	}
	if got, _ := os.ReadFile(exe); string(got) != "new" {
		t.Fatalf("exe not restored, got %q", got)
	}
}
