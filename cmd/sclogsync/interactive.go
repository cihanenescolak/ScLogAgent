package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/cihanenescolak/ScLogAgent/internal/api"
	"github.com/cihanenescolak/ScLogAgent/internal/detect"
	"github.com/cihanenescolak/ScLogAgent/internal/state"
	"github.com/cihanenescolak/ScLogAgent/internal/svc"
	"github.com/cihanenescolak/ScLogAgent/internal/ui"
	"github.com/cihanenescolak/ScLogAgent/internal/uploader"
	"github.com/cihanenescolak/ScLogAgent/internal/winutil"
)

var errExit = errors.New("exit requested")

// openBrowser is replaceable in tests.
var openBrowser = winutil.OpenBrowser

func cmdInteractive(ctx context.Context, u *ui.UI, o *options) int {
	u.Println(u.Bold(u.Cyan(u.T("banner", version))))
	u.Println("")
	err := interactive(ctx, u, o)
	if errors.Is(err, errExit) {
		return exitOK
	}
	if errors.Is(err, errRestart) {
		return exitRestart
	}
	return exitFor(u, err)
}

func interactive(ctx context.Context, u *ui.UI, o *options) error {
	store := state.Open("")
	cfg, err := store.LoadConfig()
	if err != nil {
		u.Warn("warn", err)
	}

	svcState := svc.NotInstalled
	reconfigure := false
	if winutil.IsWindows {
		svcState, _ = svc.Status()
		switch svcState {
		case svc.Running:
			if err := runningServiceMenu(ctx, u, store, cfg); err != nil {
				return err
			}
			reconfigure = true
		case svc.Stopped:
			var cont bool
			if cont, reconfigure, err = stoppedServiceMenu(ctx, u, o); err != nil || !cont {
				if err == nil {
					err = errExit
				}
				return err
			}
		}
		if svcState != svc.NotInstalled {
			svcState, _ = svc.Status()
		}
	}

	mu, err := winutil.AcquireMutex()
	if err != nil {
		return err
	}
	defer mu.Release()

	client, agentCfg, err := chooseServer(ctx, u, store, cfg, o, reconfigure)
	if err != nil {
		return err
	}
	if !o.noUpdate {
		v, ok, err := selfUpdate(ctx, agentCfg, client, func(v string) { u.Info("update_downloading", v) })
		if err != nil {
			u.Warn("update_failed", err)
		}
		if ok {
			u.OK("update_installed", v)
			return errRestart
		}
	}
	if agentCfg.LatestAgentVersion != "" && api.VersionLess(version, agentCfg.LatestAgentVersion) {
		u.Warn("agent_outdated", agentCfg.LatestAgentVersion, client.BaseURL)
	}
	if err := login(ctx, u, store, cfg, client); err != nil {
		return err
	}
	folder, err := chooseFolder(ctx, u, cfg, o, reconfigure)
	if err != nil {
		return err
	}
	cfg.Folder = folder
	if err := choosePTU(ctx, u, cfg, o, folder, reconfigure); err != nil {
		return err
	}
	if o.lang != "" {
		cfg.Lang = o.lang
	}
	if err := store.SaveConfig(cfg); err != nil {
		return err
	}
	u.OK("folder_selected", folder)
	u.Println("")

	report := consoleRenderer(u, o.verbose)
	client.OnRetry = func(path string, attempt int, err error) {
		report(uploader.Event{Kind: uploader.EvRetry, Msg: path, Index: attempt, Err: err})
	}
	eng := &uploader.Engine{API: client, Store: store, Folder: folder, Mode: "console", Version: version, Report: report}
	engines := []*uploader.Engine{eng}
	if p := ptuFolder(cfg, o); p != "" {
		engines = append(engines, &uploader.Engine{API: client, Store: store, Folder: p, Mode: "console", Version: version, Report: report, Channel: "PTU"})
	}
	for _, e := range engines {
		if err := e.Init(ctx); err != nil {
			return err
		}
		if e.Channel != "" {
			u.Info("ptu_pass_start", e.Channel, u.Cyan(e.Folder))
		}
		u.Info("pass_start", len(detect.BackupFiles(e.BackupDir())))
		if _, err := e.Pass(ctx); err != nil {
			u.EndProgress()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if api.IsUnauthorized(err) {
				u.Err("login_expired")
				return errExit
			}
			u.Err("error", err)
		}
	}
	if o.once {
		return nil
	}

	if winutil.IsWindows {
		q := "svc_install_q"
		if svcState != svc.NotInstalled {
			q = "svc_restart_q"
		}
		yes, err := u.Confirm(ctx, u.T(q), true)
		if err != nil {
			return err
		}
		if yes {
			// The service takes the single-instance lock itself.
			mu.Release()
			args := []string{"install-service", "--server", client.BaseURL, "--folder", folder, "--lang", u.Lang}
			if p := ptuFolder(cfg, o); p != "" {
				args = append(args, "--ptu-folder", p)
			} else {
				args = append(args, "--no-ptu")
			}
			if err := elevate(u, args); err != nil {
				return err
			}
			u.OK("svc_installed", svc.DisplayName)
			u.Info("svc_started")
			return nil
		}
	}

	u.Println("")
	for _, e := range engines {
		u.Info("live_start", u.Cyan(e.GameLog()))
	}
	err = liveAll(ctx, engines)
	if ctx.Err() != nil {
		u.Info("stopping")
		return nil
	}
	if api.IsUnauthorized(err) {
		u.Err("login_expired")
		return errExit
	}
	return err
}

// runningServiceMenu handles a service that is already running. Returns nil
// when the user stopped it to reconfigure.
func runningServiceMenu(ctx context.Context, u *ui.UI, store *state.Store, cfg *state.Config) error {
	for {
		st := store.ReadState()
		u.Warn("svc_running")
		folder := cfg.Folder
		if st.Folder != "" {
			folder = st.Folder
		}
		u.Info("svc_running_folder", folder)
		u.Info("svc_running_last", lastUploadText(u, st))
		u.Println(u.Dim(u.T("svc_running_note")))
		u.Println("")
		u.Println(u.T("svc_menu_running"))
		n, err := u.Choose(ctx, 4, 4)
		if err != nil {
			return err
		}
		switch n {
		case 1:
			u.Println("")
			printStatus(ctx, u, store)
			u.Println("")
		case 2:
			if err := elevate(u, []string{"stop", "--lang", u.Lang}); err != nil {
				return err
			}
			u.OK("svc_stopped_ok")
			return nil
		case 3:
			if err := elevate(u, []string{"uninstall", "--lang", u.Lang}); err != nil {
				return err
			}
			u.OK("svc_uninstalled")
			return errExit
		default:
			return errExit
		}
	}
}

// stoppedServiceMenu: cont=false means the program should exit.
func stoppedServiceMenu(ctx context.Context, u *ui.UI, o *options) (cont, reconfigure bool, err error) {
	u.Warn("svc_stopped")
	u.Println(u.T("svc_menu_stopped"))
	n, err := u.Choose(ctx, 4, 1)
	if err != nil {
		return false, false, err
	}
	switch n {
	case 1:
		if err := elevate(u, []string{"start", "--lang", u.Lang}); err != nil {
			return false, false, err
		}
		u.OK("svc_started")
		return false, false, nil
	case 2:
		return true, true, nil
	case 3:
		if err := elevate(u, []string{"uninstall", "--lang", u.Lang}); err != nil {
			return false, false, err
		}
		u.OK("svc_uninstalled")
		return true, false, nil
	}
	return true, false, nil
}

// elevate runs this exe with args as administrator (UAC prompt) and waits.
func elevate(u *ui.UI, args []string) error {
	if !winutil.IsWindows {
		return errors.New(u.T("svc_not_supported"))
	}
	if winutil.IsElevated() {
		if code := dispatchElevated(u, args); code != exitOK {
			return fmt.Errorf("%s: exit code %d", args[0], code)
		}
		return nil
	}
	u.Info("svc_needs_admin")
	code, err := winutil.RunElevated(args)
	if errors.Is(err, winutil.ErrElevationCancelled) {
		return errors.New(u.T("svc_elevation_denied"))
	}
	if err != nil {
		return err
	}
	if code != exitOK {
		return errors.New(u.T("svc_failed", fmt.Sprintf("%s: exit code %d", args[0], code)))
	}
	return nil
}

// dispatchElevated runs a service command in-process (already elevated).
func dispatchElevated(u *ui.UI, args []string) int {
	o, err := parseArgs(args, os.Stderr)
	if err != nil {
		return exitUsage
	}
	if o.command == "install-service" {
		return cmdInstallService(u, o)
	}
	return cmdServiceControl(u, o)
}

// switchServer saves the server address. A different server invalidates the
// saved login and the record of uploaded files: that server has none of them.
func switchServer(store *state.Store, cfg *state.Config, server string) error {
	if cfg.Server == server {
		return nil
	}
	if cfg.Server != "" {
		cfg.Token, cfg.UserName = "", ""
		if err := store.ResetSent(nil); err != nil {
			return err
		}
	}
	cfg.Server = server
	return store.SaveConfig(cfg)
}

// chooseServer resolves and validates the server URL.
func chooseServer(ctx context.Context, u *ui.UI, store *state.Store, cfg *state.Config, o *options, reconfigure bool) (*api.Client, *api.AgentConfig, error) {
	server := o.server
	if server == "" && !reconfigure {
		server = cfg.Server
	}
	ask := server == ""
	def := cfg.Server
	if def == "" {
		def = defaultServer
	}
	for {
		if ask {
			s, err := u.Ask(ctx, u.T("server_prompt"), def)
			if err != nil {
				return nil, nil, err
			}
			server = s
		}
		server = api.NormalizeBaseURL(server)
		if err := api.ValidateBaseURL(server); err != nil {
			u.Err("server_bad", err)
			ask, def = true, ""
			continue
		}
		c := newClient(server, "", nil)
		c.MaxRetries = 1
		stop := u.Spinner(u.T("server_checking", server))
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		ac, err := c.Config(cctx)
		cancel()
		stop()
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if err != nil {
			u.Err("server_bad", err)
			ask, def = true, server
			continue
		}
		u.OK("server_ok", ac.ProtocolVersion)
		if err := switchServer(store, cfg, server); err != nil {
			return nil, nil, err
		}
		return newClient(server, "", nil), ac, nil
	}
}

// login reuses the saved token or runs the device flow; sets client.Token.
func login(ctx context.Context, u *ui.UI, store *state.Store, cfg *state.Config, client *api.Client) error {
	tok, err := state.Token(cfg)
	if err != nil {
		u.Warn("warn", err)
		tok = ""
	}
	if tok != "" {
		client.Token = tok
		me, err := client.Me(ctx)
		switch {
		case err == nil:
			u.Info("login_as", u.Cyan(me.User.Display()))
			keep, err := u.Confirm(ctx, u.T("login_continue_q"), true)
			if err != nil {
				return err
			}
			if keep {
				if name := me.User.Display(); name != cfg.UserName {
					cfg.UserName = name
					_ = store.SaveConfig(cfg)
				}
				return nil
			}
		case api.IsUnauthorized(err), api.Is(err, http.StatusForbidden, ""):
			u.Warn("login_expired")
		default:
			return err
		}
		client.Token = ""
	}
	tr, err := deviceFlow(ctx, u, client)
	if err != nil {
		return err
	}
	if err := state.SetToken(cfg, tr.Token); err != nil {
		return err
	}
	cfg.UserName = tr.User.Display()
	cfg.DeviceName = deviceName()
	if err := store.SaveConfig(cfg); err != nil {
		return err
	}
	client.Token = tr.Token
	u.OK("login_ok", u.Cyan(cfg.UserName))
	return nil
}

func deviceName() string {
	n := winutil.ComputerName()
	if n == "" {
		return "SCLogSync"
	}
	return "SCLogSync on " + n
}

func deviceFlow(ctx context.Context, u *ui.UI, client *api.Client) (*api.TokenResponse, error) {
	ds, err := client.DeviceStart(ctx, api.DeviceStartRequest{DeviceName: deviceName(), AgentVersion: version})
	if err != nil {
		return nil, err
	}
	u.Println("")
	u.Info("login_start")
	u.Println("  " + u.T("login_code", u.Bold(u.Cyan(ds.UserCode))))
	link := ds.VerificationURIComplete
	if link == "" {
		link = ds.VerificationURI
	}
	u.Println("  " + u.T("login_url", link))
	u.Println("")
	_ = openBrowser(link)

	interval := time.Duration(max(ds.Interval, 1)) * time.Second
	expires := time.Duration(ds.ExpiresIn) * time.Second
	if expires <= 0 {
		expires = 15 * time.Minute
	}
	deadline := time.Now().Add(expires)
	stop := u.Spinner(u.T("login_waiting"))
	defer stop()
	for {
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
		tr, err := client.PollToken(ctx, ds.DeviceCode)
		if err == nil && tr.Token != "" {
			stop()
			return tr, nil
		}
		ae, isAPI := api.AsError(err)
		switch {
		case err == nil:
			// 200 without a token: keep polling
		case isAPI && ae.Status == http.StatusPreconditionRequired:
			// authorization_pending
		case isAPI && ae.Code == "slow_down":
			interval += 5 * time.Second
		case isAPI && ae.Status == http.StatusForbidden:
			stop()
			return nil, errors.New(u.T("login_denied"))
		case isAPI && ae.Status == http.StatusGone:
			stop()
			return nil, errors.New(u.T("login_expired_code"))
		case api.IsTemporary(err):
			// keep polling through short outages
		default:
			stop()
			return nil, err
		}
		if time.Now().After(deadline) {
			stop()
			return nil, errors.New(u.T("login_expired_code"))
		}
	}
}

// chooseFolder picks the game folder (flag, saved, detected or typed).
func chooseFolder(ctx context.Context, u *ui.UI, cfg *state.Config, o *options, reconfigure bool) (string, error) {
	if o.folder != "" {
		if p, ok := detect.Normalize(o.folder); ok {
			return p, nil
		}
		u.Err("folder_invalid", o.folder)
	} else if cfg.Folder != "" && !reconfigure {
		if _, ok := detect.Inspect(cfg.Folder); ok {
			return cfg.Folder, nil
		}
	}

	cands := detect.Scan(winutil.DriveRoots(), detect.LauncherRoots(os.Getenv("APPDATA")))
	if cfg.Folder != "" {
		if c, ok := detect.Inspect(cfg.Folder); ok && !containsPath(cands, c.Path) {
			cands = append([]detect.Candidate{c}, cands...)
		}
	}
	for {
		def := 0
		if len(cands) == 0 {
			u.Warn("folder_none")
		} else {
			u.Info("folder_title")
			for i, c := range cands {
				info := ""
				if c.HasGameLog {
					info = u.T("folder_has_gamelog")
				}
				label := u.T("folder_item_info", c.Backups, info)
				if c.Path == cfg.Folder {
					label += ", " + u.T("folder_current")
					def = i + 1
				}
				u.Println("  " + u.T("folder_item", i+1, u.Cyan(c.Path), label))
			}
			if def == 0 {
				def = 1
			}
		}
		u.Println("  " + u.T("folder_manual", len(cands)+1))
		if len(cands) == 0 {
			def = 1
		}
		n, err := u.Choose(ctx, len(cands)+1, def)
		if err != nil {
			return "", err
		}
		if n <= len(cands) {
			return cands[n-1].Path, nil
		}
		s, err := u.Ask(ctx, u.T("folder_prompt_path"), "")
		if err != nil {
			return "", err
		}
		if p, ok := detect.Normalize(s); ok {
			return p, nil
		}
		u.Err("folder_invalid", s)
	}
}

func containsPath(cs []detect.Candidate, p string) bool {
	for _, c := range cs {
		if c.Path == p {
			return true
		}
	}
	return false
}
