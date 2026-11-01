// Package detect finds Star Citizen installation folders (the folder that
// contains Game.log and logbackups\).
package detect

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Environments are the per-channel subfolders of a StarCitizen install.
var Environments = []string{"LIVE", "PTU", "EPTU", "TECH-PREVIEW", "HOTFIX"}

// installBases are tried below every drive root.
var installBases = [][]string{
	{"Program Files", "Roberts Space Industries", "StarCitizen"},
	{"Roberts Space Industries", "StarCitizen"},
	{"StarCitizen"},
	{"Games", "StarCitizen"},
	{"Games", "Roberts Space Industries", "StarCitizen"},
	{"Program Files (x86)", "Roberts Space Industries", "StarCitizen"},
}

// Candidate is a detected game folder.
type Candidate struct {
	Path        string
	Environment string
	HasGameLog  bool
	Backups     int // number of *.log in logbackups
}

// Inspect describes dir; ok=false if it holds neither Game.log nor logbackups.
func Inspect(dir string) (Candidate, bool) {
	c := Candidate{Path: dir, Environment: strings.ToUpper(filepath.Base(dir))}
	if fi, err := os.Stat(filepath.Join(dir, "Game.log")); err == nil && !fi.IsDir() {
		c.HasGameLog = true
	}
	bdir := filepath.Join(dir, "logbackups")
	fi, err := os.Stat(bdir)
	hasBackups := err == nil && fi.IsDir()
	if hasBackups {
		c.Backups = len(BackupFiles(bdir))
	}
	return c, c.HasGameLog || hasBackups
}

// Normalize maps a path the user typed to a game folder: it accepts the
// folder itself, its logbackups subfolder, Game.log, or a StarCitizen root
// containing exactly one environment folder. Quotes are stripped.
func Normalize(p string) (string, bool) {
	p = strings.TrimSpace(p)
	p = strings.Trim(p, `"'`)
	if p == "" {
		return "", false
	}
	p = filepath.Clean(p)
	if strings.EqualFold(filepath.Base(p), "Game.log") {
		p = filepath.Dir(p)
	}
	if strings.EqualFold(filepath.Base(p), "logbackups") {
		p = filepath.Dir(p)
	}
	if _, ok := Inspect(p); ok {
		return p, true
	}
	var found []string
	for _, env := range Environments {
		sub := filepath.Join(p, env)
		if _, ok := Inspect(sub); ok {
			found = append(found, sub)
		}
	}
	if len(found) == 1 {
		return found[0], true
	}
	return p, false
}

// Scan lists game folders found below the given drive roots plus any extra
// install roots (e.g. from the RSI launcher). Results are de-duplicated and
// sorted: LIVE first, then by number of backups.
func Scan(roots []string, extraInstallRoots []string) []Candidate {
	seen := map[string]bool{}
	var out []Candidate
	add := func(dir string) {
		key := strings.ToLower(filepath.Clean(dir))
		if seen[key] {
			return
		}
		seen[key] = true
		if c, ok := Inspect(dir); ok {
			out = append(out, c)
		}
	}
	var installs []string
	for _, r := range roots {
		for _, b := range installBases {
			installs = append(installs, filepath.Join(append([]string{r}, b...)...))
		}
	}
	installs = append(installs, extraInstallRoots...)
	for _, inst := range installs {
		for _, env := range Environments {
			add(filepath.Join(inst, env))
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		li, lj := out[i].Environment == "LIVE", out[j].Environment == "LIVE"
		if li != lj {
			return li
		}
		return out[i].Backups > out[j].Backups
	})
	return out
}

// BackupFiles lists *.log files in a logbackups dir sorted by mtime (oldest first).
func BackupFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	type fe struct {
		path string
		mt   int64
	}
	var files []fe
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".log") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, fe{filepath.Join(dir, e.Name()), info.ModTime().UnixNano()})
	}
	sort.SliceStable(files, func(i, j int) bool {
		if files[i].mt != files[j].mt {
			return files[i].mt < files[j].mt
		}
		return files[i].path < files[j].path
	})
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.path
	}
	return out
}

// launcherPathRe finds "<X>:\...\StarCitizen" install roots in launcher
// logs/settings (JSON-escaped backslashes are normalised first).
var launcherPathRe = regexp.MustCompile(`(?i)([A-Z]:\\[^"'\r\n<>|*?]*?\\StarCitizen)(?:\\(?:LIVE|PTU|EPTU|TECH-PREVIEW|HOTFIX))?(?:[\\"'\s]|$)`)

// LauncherInstallRoots extracts StarCitizen install roots mentioned in text.
func LauncherInstallRoots(text string) []string {
	text = strings.ReplaceAll(text, `\\`, `\`)
	text = strings.ReplaceAll(text, "/", `\`)
	seen := map[string]bool{}
	var out []string
	for _, m := range launcherPathRe.FindAllStringSubmatch(text, -1) {
		p := m[1]
		k := strings.ToLower(p)
		if !seen[k] {
			seen[k] = true
			out = append(out, p)
		}
	}
	return out
}

// LauncherRoots reads the RSI launcher's files under appData (%APPDATA%) and
// returns install roots it mentions. Best effort; errors are ignored.
func LauncherRoots(appData string) []string {
	if appData == "" {
		return nil
	}
	base := filepath.Join(appData, "rsilauncher")
	var files []string
	for _, pat := range []string{"*.json", filepath.Join("logs", "*.log"), "*.log"} {
		m, _ := filepath.Glob(filepath.Join(base, pat))
		files = append(files, m...)
	}
	var out []string
	seen := map[string]bool{}
	for _, f := range files {
		fi, err := os.Stat(f)
		if err != nil || fi.Size() > 32<<20 {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, r := range LauncherInstallRoots(string(data)) {
			if k := strings.ToLower(r); !seen[k] {
				seen[k] = true
				out = append(out, r)
			}
		}
	}
	return out
}

// TestFolders returns the test-server folders (PTU, EPTU, TECH-PREVIEW, …) next to a LIVE
// game folder, e.g. …\StarCitizen\LIVE → …\StarCitizen\PTU, that contain logs.
func TestFolders(live string) []Candidate {
	if live == "" {
		return nil
	}
	parent := filepath.Dir(filepath.Clean(live))
	self := strings.ToUpper(filepath.Base(filepath.Clean(live)))
	var out []Candidate
	for _, env := range Environments {
		if env == "LIVE" || env == self {
			continue
		}
		if c, ok := Inspect(filepath.Join(parent, env)); ok {
			out = append(out, c)
		}
	}
	return out
}
