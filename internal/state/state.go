// Package state persists the agent's configuration (server, encrypted token,
// folder) and a local cache of per-file hashes / upload status. The cache is
// only an optimisation: the server is the source of truth.
package state

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cihanenescolak/ScLogAgent/internal/winutil"
)

// Config is %ProgramData%\SCLogSync\config.json.
type Config struct {
	Server  string   `json:"server,omitempty"`
	Token   string   `json:"token,omitempty"` // "<scheme>:<base64>" (dpapi on Windows)
	Folder  string   `json:"folder,omitempty"`
	Folders []string `json:"folders,omitempty"` // reserved: multiple game folders
	// PTU: nil = not asked yet; true = also upload PTUFolder (PTU / EPTU / TECH-PREVIEW logs).
	PTU        *bool  `json:"ptu,omitempty"`
	PTUFolder  string `json:"ptuFolder,omitempty"`
	Lang       string `json:"lang,omitempty"`
	UserName   string `json:"userName,omitempty"`
	DeviceName string `json:"deviceName,omitempty"`
}

// File status values kept in the cache.
const (
	FileComplete = "complete" // server confirmed
	FileSkipped  = "skipped"  // no session uuid: not uploadable
	FileFailed   = "failed"   // server rejected (422); retried only if content changes
	FileHashed   = "hashed"   // hash known, upload pending
)

// FileEntry caches the prefilter result of one backup file.
type FileEntry struct {
	Size        int64  `json:"size"`
	ModTime     int64  `json:"mtime"` // unix nanos
	Fingerprint string `json:"fp"`    // prefilter pattern set
	SessionUUID string `json:"sessionUuid,omitempty"`
	Sha256      string `json:"sha256,omitempty"`
	LineCount   int    `json:"lineCount"`
	Status      string `json:"status"`
	UpdatedAt   int64  `json:"updatedAt"`
}

// LastUpload is shown by `status` and the service menu.
type LastUpload struct {
	At      int64  `json:"at"`
	File    string `json:"file,omitempty"`
	Session string `json:"session,omitempty"`
	Kind    string `json:"kind,omitempty"` // backup | live
	Lines   int    `json:"lines,omitempty"`
}

// State is %ProgramData%\SCLogSync\state.json.
type State struct {
	Version       int                   `json:"version"`
	Epoch         *int                  `json:"epoch,omitempty"`
	Files         map[string]*FileEntry `json:"files"`
	LastUpload    *LastUpload           `json:"lastUpload,omitempty"`
	LastHeartbeat int64                 `json:"lastHeartbeat,omitempty"`
	LastError     string                `json:"lastError,omitempty"`
	Mode          string                `json:"mode,omitempty"`
	Folder        string                `json:"folder,omitempty"`
	PID           int                   `json:"pid,omitempty"`
}

// Store guards config + state files in Dir.
type Store struct {
	Dir string
	mu  sync.Mutex
	st  *State
}

// Open uses dir (defaults to winutil.DataDir()). The directory is created
// lazily on first save.
func Open(dir string) *Store {
	if dir == "" {
		dir = winutil.DataDir()
	}
	return &Store{Dir: dir}
}

func (s *Store) ConfigPath() string { return filepath.Join(s.Dir, "config.json") }
func (s *Store) StatePath() string  { return filepath.Join(s.Dir, "state.json") }
func (s *Store) LogPath() string    { return filepath.Join(s.Dir, "agent.log") }

// EnsureDir creates the data dir; a freshly created dir gets a restrictive ACL.
func (s *Store) EnsureDir() error {
	if _, err := os.Stat(s.Dir); err == nil {
		return nil
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	_ = winutil.RestrictDir(s.Dir)
	return nil
}

func (s *Store) LoadConfig() (*Config, error) {
	var c Config
	data, err := os.ReadFile(s.ConfigPath())
	if errors.Is(err, os.ErrNotExist) {
		return &c, nil
	}
	if err != nil {
		return &c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return &Config{}, fmt.Errorf("%s: %w", s.ConfigPath(), err)
	}
	return &c, nil
}

func (s *Store) SaveConfig(c *Config) error {
	if err := s.EnsureDir(); err != nil {
		return err
	}
	return writeJSON(s.ConfigPath(), c)
}

// SetToken encrypts and stores token in c (call SaveConfig afterwards).
func SetToken(c *Config, token string) error {
	if token == "" {
		c.Token = ""
		return nil
	}
	enc, err := winutil.Protect([]byte(token))
	if err != nil {
		return err
	}
	c.Token = winutil.ProtectScheme + ":" + base64.StdEncoding.EncodeToString(enc)
	return nil
}

// Token decrypts the stored token ("" when none).
func Token(c *Config) (string, error) {
	if c.Token == "" {
		return "", nil
	}
	scheme, b64, ok := strings.Cut(c.Token, ":")
	if !ok || scheme != winutil.ProtectScheme {
		return "", fmt.Errorf("token stored with unsupported scheme %q", scheme)
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", err
	}
	tok, err := winutil.Unprotect(raw)
	if err != nil {
		return "", err
	}
	return string(tok), nil
}

// State returns the cached state, loading it on first use. Never nil.
func (s *Store) State() *State {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st == nil {
		s.st = s.readState()
	}
	return s.st
}

// ReadState reads state.json from disk without caching (for `status`).
func (s *Store) ReadState() *State { return s.readState() }

func (s *Store) readState() *State {
	st := &State{Version: 1, Files: map[string]*FileEntry{}}
	data, err := os.ReadFile(s.StatePath())
	if err == nil {
		_ = json.Unmarshal(data, st)
	}
	if st.Files == nil {
		st.Files = map[string]*FileEntry{}
	}
	return st
}

// Update mutates the state under lock and persists it. Persistence errors are
// returned but the in-memory state is kept (the cache is best-effort).
func (s *Store) Update(fn func(st *State)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st == nil {
		s.st = s.readState()
	}
	fn(s.st)
	if err := s.EnsureDir(); err != nil {
		return err
	}
	return writeJSON(s.StatePath(), s.st)
}

// ResetSent forgets which files were uploaded (after a server-side reset).
// Hashes stay cached so the next pass does not re-read every file.
func (s *Store) ResetSent(epoch *int) error {
	return s.Update(func(st *State) {
		st.Epoch = epoch
		for _, f := range st.Files {
			if f.Status == FileComplete || f.Status == FileFailed {
				f.Status = FileHashed
			}
		}
		st.LastUpload = nil
	})
}

// Purge removes the whole data dir.
func (s *Store) Purge() error { return os.RemoveAll(s.Dir) }

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, time.Now().UnixNano())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
