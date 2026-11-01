// Package update replaces the running sclogsync.exe with the build the server offers.
//
// The server announces a version in /api/agent/config together with where to get it: a file on the server
// itself or a release asset of this project on GitHub. Nothing else is downloaded. Integrity: the download
// must have exactly the size and SHA-256 the server announced (it takes them from the release's SHA256SUMS).
// Releases are not code-signed yet; once they are, the signature check belongs in Download next to the hash check.
package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cihanenescolak/ScLogAgent/internal/api"
)

// MaxSize bounds a download (the exe is ~10 MB).
const MaxSize = 64 << 20

// Available returns the offered build when it is newer than current ("dev" builds never update).
func Available(cfg *api.AgentConfig, current string) *api.AgentUpdate {
	if cfg == nil || cfg.AgentUpdate == nil {
		return nil
	}
	u := cfg.AgentUpdate
	if u.URL == "" || len(u.Sha256) != 64 || u.Size <= 0 || u.Size > MaxSize {
		return nil
	}
	if !api.VersionLess(current, u.Version) {
		return nil
	}
	return u
}

// resolve turns the offered URL into an absolute one. Allowed: the configured server, or an HTTPS GitHub
// release asset (https://github.com/<owner>/<repo>/releases/download/<tag>/<file>).
func resolve(baseURL, ref string) (string, error) {
	base, err := url.Parse(baseURL + "/")
	if err != nil {
		return "", err
	}
	u, err := base.Parse(ref)
	if err != nil {
		return "", err
	}
	if u.Scheme == base.Scheme && u.Host == base.Host {
		return u.String(), nil
	}
	if u.Scheme == "https" && u.Host == "github.com" && githubAsset(u.Path) {
		return u.String(), nil
	}
	return "", fmt.Errorf("update URL %q is neither on %s nor a GitHub release asset", ref, base.Host)
}

// githubAsset: /<owner>/<repo>/releases/download/<tag>/<file>
func githubAsset(p string) bool {
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	return len(parts) == 6 && parts[2] == "releases" && parts[3] == "download" && parts[0] != "" && parts[1] != "" && parts[4] != "" && parts[5] != ""
}

// Download fetches the build into a temporary file in dir (the folder of the exe, so the later rename
// stays on one volume) and verifies size and SHA-256. Returns the temp file path.
func Download(ctx context.Context, hc *http.Client, baseURL, userAgent string, u *api.AgentUpdate, dir string) (path string, err error) {
	src, err := resolve(baseURL, u.URL)
	if err != nil {
		return "", err
	}
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return "", err
	}
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: HTTP %d", src, resp.StatusCode)
	}

	f, err := os.CreateTemp(dir, "sclogsync-*.exe.new")
	if err != nil {
		return "", err
	}
	defer func() {
		f.Close()
		if err != nil {
			os.Remove(f.Name())
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, u.Size+1))
	if err != nil {
		return "", err
	}
	if n != u.Size {
		return "", fmt.Errorf("download size %d, expected %d", n, u.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, u.Sha256) {
		return "", fmt.Errorf("download checksum %s, expected %s", got, u.Sha256)
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	return f.Name(), f.Close()
}

// Install swaps newPath in as exe. Windows can't overwrite a running executable but can rename it, so the
// current one is moved aside to "<exe>.old" (removed on the next start by Cleanup); on failure it is put back.
func Install(newPath, exe string) error {
	// the temp file is created 0600; give it the current exe's permissions (matters off Windows)
	if st, err := os.Stat(exe); err == nil {
		if err := os.Chmod(newPath, st.Mode().Perm()); err != nil {
			return err
		}
	}
	old := exe + ".old"
	if err := os.Remove(old); err != nil && !errors.Is(err, os.ErrNotExist) {
		// still locked by an earlier process that hasn't exited: use a fresh name
		old = fmt.Sprintf("%s.old-%d", exe, time.Now().UnixNano())
	}
	if err := os.Rename(exe, old); err != nil {
		return fmt.Errorf("move current executable aside: %w", err)
	}
	if err := os.Rename(newPath, exe); err != nil {
		if rerr := os.Rename(old, exe); rerr != nil {
			return fmt.Errorf("install new executable: %w (restoring the old one also failed: %v)", err, rerr)
		}
		return fmt.Errorf("install new executable: %w", err)
	}
	return nil
}

// Cleanup removes executables left behind by earlier updates (best effort).
func Cleanup(exe string) {
	matches, _ := filepath.Glob(exe + ".old*")
	for _, m := range matches {
		_ = os.Remove(m)
	}
	news, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), "sclogsync-*.exe.new"))
	for _, m := range news {
		if st, err := os.Stat(m); err == nil && time.Since(st.ModTime()) > time.Hour {
			_ = os.Remove(m)
		}
	}
}

// Executable is the path of the running exe with symlinks resolved.
func Executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}
