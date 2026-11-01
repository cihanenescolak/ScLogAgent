package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cihanenescolak/ScLogAgent/internal/state"
	"github.com/cihanenescolak/ScLogAgent/internal/svc"
	"github.com/cihanenescolak/ScLogAgent/internal/ui"
	"github.com/cihanenescolak/ScLogAgent/internal/winutil"
)

// cmdServiceControl implements start / stop / uninstall (self-elevating).
func cmdServiceControl(u *ui.UI, o *options) int {
	if !winutil.IsWindows {
		u.Err("svc_not_supported")
		return exitError
	}
	if !winutil.IsElevated() {
		args := []string{o.command, "--lang", u.Lang}
		if o.purge {
			args = append(args, "--purge")
		}
		if err := elevate(u, args); err != nil {
			u.Err("error", err)
			return exitError
		}
		switch o.command {
		case "start":
			u.OK("svc_started")
		case "stop":
			u.OK("svc_stopped_ok")
		case "uninstall":
			u.OK("svc_uninstalled")
		}
		return exitOK
	}

	var err error
	switch o.command {
	case "start":
		err = svc.Start()
	case "stop":
		err = svc.Stop(30 * time.Second)
	case "uninstall":
		err = uninstall(u, o.purge)
	}
	if err != nil {
		u.Err("svc_failed", err)
		pauseIfOwnConsole(u)
		return exitError
	}
	return exitOK
}

func uninstall(u *ui.UI, purge bool) error {
	st, err := svc.Status()
	if err != nil {
		return err
	}
	if st == svc.Running {
		if err := svc.Stop(30 * time.Second); err != nil {
			return err
		}
	}
	if st != svc.NotInstalled {
		if err := svc.Uninstall(); err != nil {
			return err
		}
	}
	if purge {
		store := state.Open("")
		if err := store.Purge(); err != nil {
			return err
		}
		u.Info("purged", store.Dir)
		// Best effort: the installed copy (cannot delete ourselves while running).
		if exe, err := os.Executable(); err == nil && !samePath(filepath.Dir(exe), winutil.InstallDir()) {
			_ = os.RemoveAll(winutil.InstallDir())
		}
	}
	return nil
}

// cmdInstallService (elevated): copy the exe to Program Files, save the
// settings passed by the interactive run, (re)install and start the service.
func cmdInstallService(u *ui.UI, o *options) int {
	if err := installService(o); err != nil {
		u.Err("svc_failed", err)
		pauseIfOwnConsole(u)
		return exitError
	}
	return exitOK
}

func installService(o *options) error {
	if !winutil.IsWindows {
		return errors.New("windows only")
	}
	store := state.Open("")
	cfg, err := store.LoadConfig()
	if err != nil {
		return err
	}
	if o.server != "" {
		cfg.Server = o.server
	}
	if o.folder != "" {
		cfg.Folder = o.folder
	}
	if o.noPTU || o.ptuFolder != "" {
		on := o.ptuFolder != ""
		cfg.PTU, cfg.PTUFolder = &on, o.ptuFolder
	}
	if o.lang != "" {
		cfg.Lang = o.lang
	}
	if cfg.Server == "" || cfg.Folder == "" || cfg.Token == "" {
		return errors.New("not configured (server, folder and login are required)")
	}
	if err := store.SaveConfig(cfg); err != nil {
		return err
	}

	st, err := svc.Status()
	if err != nil {
		return err
	}
	if st == svc.Running {
		if err := svc.Stop(30 * time.Second); err != nil {
			return err
		}
	}
	src, err := os.Executable()
	if err != nil {
		return err
	}
	dest := filepath.Join(winutil.InstallDir(), winutil.ExeName)
	if !samePath(src, dest) {
		if err := copyFile(src, dest); err != nil {
			return err
		}
	}
	if st == svc.NotInstalled {
		if err := svc.Install(dest); err != nil {
			return err
		}
	}
	return svc.Start()
}

func copyFile(src, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dest + ".new"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	// A running copy cannot be overwritten but can be renamed away on Windows.
	old := dest + ".old"
	_ = os.Remove(old)
	if _, err := os.Stat(dest); err == nil {
		if err := os.Rename(dest, old); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, dest); err != nil {
		return err
	}
	_ = os.Remove(old)
	return nil
}

func samePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

func pauseIfOwnConsole(u *ui.UI) {
	if winutil.OwnsConsole() {
		_, _ = u.ReadLine(context.Background(), "\n"+u.T("press_enter"))
	}
}

func cmdLogout(u *ui.UI) int {
	store := state.Open("")
	cfg, err := store.LoadConfig()
	if err != nil {
		u.Err("error", err)
		return exitError
	}
	cfg.Token, cfg.UserName = "", ""
	if err := store.SaveConfig(cfg); err != nil {
		u.Err("error", err)
		return exitError
	}
	u.OK("logged_out")
	return exitOK
}

func cmdStatus(ctx context.Context, u *ui.UI, o *options) int {
	store := state.Open("")
	if _, err := os.Stat(store.ConfigPath()); err != nil {
		u.Warn("not_configured")
		u.Info("status_data_dir", store.Dir)
		return exitNoConfig
	}
	printStatus(ctx, u, store)
	return exitOK
}

func printStatus(ctx context.Context, u *ui.UI, store *state.Store) {
	cfg, err := store.LoadConfig()
	if err != nil {
		u.Warn("warn", err)
	}
	st := store.ReadState()
	u.Println(u.Bold(u.T("status_title")))
	u.Info("status_version", version)
	if winutil.IsWindows {
		s, err := svc.Status()
		label := u.T(s.Key())
		if err != nil {
			label += " (" + err.Error() + ")"
		}
		u.Info("status_service", label)
	}
	u.Info("status_server", orDash(cfg.Server))

	user := cfg.UserName
	if tok, err := state.Token(cfg); err == nil && tok != "" && cfg.Server != "" {
		c := newClient(cfg.Server, tok, nil)
		c.MaxRetries = 0
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if me, err := c.Me(cctx); err == nil {
			user = me.User.Display()
		} else {
			user += " (" + err.Error() + ")"
		}
		cancel()
	} else if cfg.Token == "" {
		user = u.T("status_never")
	}
	u.Info("status_user", orDash(user))
	u.Info("status_folder", orDash(cfg.Folder))
	if cfg.PTU != nil && *cfg.PTU {
		u.Info("status_ptu_folder", orDash(cfg.PTUFolder))
	}
	u.Info("status_last", lastUploadText(u, st))
	hb := formatMillis(st.LastHeartbeat)
	if hb == "" {
		hb = u.T("status_never")
	}
	u.Info("status_heartbeat", hb)
	done := 0
	for _, f := range st.Files {
		if f.Status == state.FileComplete {
			done++
		}
	}
	u.Info("status_files", len(st.Files), done)
	if st.LastError != "" {
		u.Warn("warn", st.LastError)
	}
	u.Info("status_data_dir", store.Dir)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
