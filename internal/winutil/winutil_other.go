//go:build !windows

package winutil

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

// ProtectScheme names the encoding stored in config.json. Off Windows there is
// no DPAPI: the token is only obfuscated and the file is chmod 0600.
const ProtectScheme = "plain"

func Protect(data []byte) ([]byte, error) {
	return []byte(base64.StdEncoding.EncodeToString(data)), nil
}

func Unprotect(data []byte) ([]byte, error) {
	return base64.StdEncoding.DecodeString(string(data))
}

// Mutex is a held single-instance lock (an flock'ed file).
type Mutex struct{ f *os.File }

func AcquireMutex() (*Mutex, error) {
	dir := DataDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "instance.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrAlreadyRunning
		}
		return nil, err
	}
	return &Mutex{f: f}, nil
}

func (m *Mutex) Release() {
	if m == nil || m.f == nil {
		return
	}
	_ = syscall.Flock(int(m.f.Fd()), syscall.LOCK_UN)
	m.f.Close()
	m.f = nil
}

func IsElevated() bool { return os.Geteuid() == 0 }

func RunElevated(args []string) (int, error) {
	return -1, fmt.Errorf("elevation is only supported on Windows (%s)", runtime.GOOS)
}

func OpenBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}

func UILanguage() string {
	for _, k := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := os.Getenv(k); v != "" {
			if strings.HasPrefix(strings.ToLower(v), "tr") {
				return "tr"
			}
			return "en"
		}
	}
	return "en"
}

func EnableConsole() bool {
	return os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
}

func OwnsConsole() bool { return false }

// DataDir is $SCLOGSYNC_HOME or <user config dir>/SCLogSync.
func DataDir() string {
	if d := os.Getenv("SCLOGSYNC_HOME"); d != "" {
		return d
	}
	base, err := os.UserConfigDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "SCLogSync")
}

func InstallDir() string { return filepath.Join(DataDir(), "bin") }

const ExeName = "sclogsync"

func RestrictDir(dir string) error { return os.Chmod(dir, 0o700) }

func DriveRoots() []string { return []string{"/"} }

func ComputerName() string {
	h, _ := os.Hostname()
	return strings.TrimSuffix(h, ".local")
}

const IsWindows = false
