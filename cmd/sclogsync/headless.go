package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/cihanenescolak/ScLogAgent/internal/api"
	"github.com/cihanenescolak/ScLogAgent/internal/logx"
	"github.com/cihanenescolak/ScLogAgent/internal/state"
	"github.com/cihanenescolak/ScLogAgent/internal/svc"
	"github.com/cihanenescolak/ScLogAgent/internal/uploader"
	"github.com/cihanenescolak/ScLogAgent/internal/winutil"
)

var errNotConfigured = errors.New("not configured: run sclogsync.exe interactively first")

// cmdRun is the headless mode used by the Windows service (also usable from a
// console: it then runs in the foreground until Ctrl+C).
func cmdRun(o *options) int {
	store := state.Open("")
	if err := store.EnsureDir(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitError
	}
	lg, err := logx.Open(store.LogPath(), logx.DefaultMaxBytes)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitError
	}
	defer lg.Close()
	if svc.Interactive() {
		lg.Echo = func(line string) { fmt.Println(line) }
	}
	lg.Printf("SCLogSync %s starting (pid %d)", version, os.Getpid())
	err = svc.Run(func(ctx context.Context) error {
		headless(ctx, store, o, lg)
		return nil
	})
	if err != nil {
		lg.Printf("service error: %v", err)
		return exitError
	}
	lg.Printf("stopped")
	return exitOK
}

func headless(ctx context.Context, store *state.Store, o *options, lg *logx.File) {
	var mu *winutil.Mutex
	for {
		m, err := winutil.AcquireMutex()
		if err == nil {
			mu = m
			break
		}
		lg.Printf("waiting: %v", err)
		if !sleepCtx(ctx, 30*time.Second) {
			return
		}
	}
	defer mu.Release()

	failures := 0
	for {
		started := time.Now()
		err := headlessOnce(ctx, store, o, lg)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > 10*time.Minute {
			failures = 0
		}
		failures++
		wait := time.Duration(min(failures, 6)) * 30 * time.Second
		if errors.Is(err, errNotConfigured) || api.IsUnauthorized(err) {
			wait = 5 * time.Minute
		}
		msg := "stopped unexpectedly"
		if err != nil {
			msg = err.Error()
		}
		if api.IsUnauthorized(err) {
			msg = "login rejected by server (token revoked?); run sclogsync.exe to sign in again"
		}
		lg.Printf("error: %s; retrying in %s", msg, wait)
		_ = store.Update(func(s *state.State) { s.LastError = msg })
		if !sleepCtx(ctx, wait) {
			return
		}
	}
}

func headlessOnce(ctx context.Context, store *state.Store, o *options, lg *logx.File) error {
	cfg, err := store.LoadConfig()
	if err != nil {
		return err
	}
	server, folder := cfg.Server, cfg.Folder
	if o.server != "" {
		server = api.NormalizeBaseURL(o.server)
	}
	if o.folder != "" {
		folder = o.folder
	}
	tok, err := state.Token(cfg)
	if err != nil {
		return fmt.Errorf("read token: %w", err)
	}
	if server == "" || folder == "" || tok == "" {
		return errNotConfigured
	}
	report := func(e uploader.Event) {
		switch e.Kind {
		case uploader.EvScanFile, uploader.EvUploadChunk:
			return // too chatty for the log file
		case uploader.EvHeartbeat, uploader.EvAlreadyDone, uploader.EvLiveSent:
			if !o.verbose {
				return
			}
		}
		lg.Printf("%s", e.String())
	}
	client := newClient(server, tok, report)
	if !o.noUpdate {
		uctx, cancel := context.WithCancel(ctx)
		defer cancel()
		go headlessUpdates(uctx, client, lg)
	}
	engines := []*uploader.Engine{{API: client, Store: store, Folder: folder, Mode: "service", Version: version, Report: report}}
	if p := ptuFolder(cfg, o); p != "" {
		engines = append(engines, &uploader.Engine{API: client, Store: store, Folder: p, Mode: "service", Version: version, Report: report, Channel: "PTU"})
	}
	for _, eng := range engines {
		if err := eng.Init(ctx); err != nil {
			return err
		}
	}
	lg.Printf("server %s, folder %s", server, folder)
	if len(engines) > 1 {
		lg.Printf("PTU folder %s", engines[1].Folder)
	}
	for _, eng := range engines {
		if _, err := eng.Pass(ctx); err != nil {
			if ctx.Err() != nil || api.IsUnauthorized(err) {
				return err
			}
			lg.Printf("backup pass: %v", err)
		}
	}
	return liveAll(ctx, engines)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
