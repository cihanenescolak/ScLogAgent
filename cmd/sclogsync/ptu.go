package main

import (
	"context"
	"errors"
	"sync"

	"github.com/cihanenescolak/ScLogAgent/internal/detect"
	"github.com/cihanenescolak/ScLogAgent/internal/state"
	"github.com/cihanenescolak/ScLogAgent/internal/ui"
	"github.com/cihanenescolak/ScLogAgent/internal/uploader"
)

// ptuFolder is the configured test-server folder, honouring --ptu-folder / --no-ptu ("" = off).
func ptuFolder(cfg *state.Config, o *options) string {
	switch {
	case o.noPTU:
		return ""
	case o.ptuFolder != "":
		if p, ok := detect.Normalize(o.ptuFolder); ok {
			return p
		}
		return ""
	case cfg.PTU != nil && *cfg.PTU:
		return cfg.PTUFolder
	}
	return ""
}

// choosePTU asks (once, or again when reconfiguring) whether PTU logs should be uploaded too.
// The server processes PTU files right away and only flags versions it was not updated for.
func choosePTU(ctx context.Context, u *ui.UI, cfg *state.Config, o *options, live string, reconfigure bool) error {
	off := false
	on := true
	switch {
	case o.noPTU:
		cfg.PTU, cfg.PTUFolder = &off, ""
		return nil
	case o.ptuFolder != "":
		p, ok := detect.Normalize(o.ptuFolder)
		if !ok {
			u.Err("folder_invalid", o.ptuFolder)
			return nil
		}
		cfg.PTU, cfg.PTUFolder = &on, p
		return nil
	case cfg.PTU != nil && !reconfigure:
		return nil
	case o.folder != "" && cfg.PTU == nil:
		// scripted setup (--folder given): don't prompt; PTU stays off unless --ptu-folder is passed
		cfg.PTU, cfg.PTUFolder = &off, ""
		return nil
	}
	found := detect.TestFolders(live)
	if len(found) == 0 {
		yes, err := u.Confirm(ctx, u.T("ptu_q_none"), false)
		if err != nil || !yes {
			cfg.PTU, cfg.PTUFolder = &off, ""
			return err
		}
		for {
			s, err := u.Ask(ctx, u.T("ptu_prompt_path"), "")
			if err != nil {
				return err
			}
			if s == "" {
				cfg.PTU, cfg.PTUFolder = &off, ""
				return nil
			}
			if p, ok := detect.Normalize(s); ok {
				cfg.PTU, cfg.PTUFolder = &on, p
				u.OK("ptu_selected", p)
				return nil
			}
			u.Err("folder_invalid", s)
		}
	}
	c := found[0]
	yes, err := u.Confirm(ctx, u.T("ptu_q_found", c.Environment, u.Cyan(c.Path)), cfg.PTU != nil && *cfg.PTU)
	if err != nil {
		return err
	}
	if !yes {
		cfg.PTU, cfg.PTUFolder = &off, ""
		u.Info("ptu_disabled")
		return nil
	}
	cfg.PTU, cfg.PTUFolder = &on, c.Path
	u.OK("ptu_selected", c.Path)
	return nil
}

// liveAll tails every engine's Game.log until ctx ends or one of them fails fatally.
func liveAll(ctx context.Context, engines []*uploader.Engine) error {
	if len(engines) == 1 {
		return engines[0].Live(ctx)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	errs := make([]error, len(engines))
	for i, e := range engines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = e.Live(ctx)
			cancel() // one engine stopping (e.g. login revoked) stops all
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
	}
	return ctx.Err()
}
