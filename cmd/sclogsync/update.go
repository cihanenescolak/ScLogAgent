package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/cihanenescolak/ScLogAgent/internal/api"
	"github.com/cihanenescolak/ScLogAgent/internal/logx"
	"github.com/cihanenescolak/ScLogAgent/internal/svc"
	"github.com/cihanenescolak/ScLogAgent/internal/update"
)

// envUpdated is set for the process started right after a self-update so it doesn't update again (guards
// against a loop when the server announces a version that doesn't match the exe it ships).
const envUpdated = "SCLOGSYNC_UPDATED"

// updateCheckEvery is how often the service looks for a newer build (also checked at every start).
const updateCheckEvery = 6 * time.Hour

var errRestart = errors.New("restart after update")

// selfUpdate installs a newer build when the server offers one. notify is called with the version before
// downloading. Returns the offered version and whether the exe was replaced.
func selfUpdate(ctx context.Context, cfg *api.AgentConfig, client *api.Client, notify func(version string)) (string, bool, error) {
	if os.Getenv(envUpdated) != "" {
		return "", false, nil
	}
	rel := update.Available(cfg, version)
	if rel == nil {
		return "", false, nil
	}
	if notify != nil {
		notify(rel.Version)
	}
	exe, err := update.Executable()
	if err != nil {
		return rel.Version, false, err
	}
	tmp, err := update.Download(ctx, nil, client.BaseURL, client.UserAgent, rel, filepath.Dir(exe))
	if err != nil {
		return rel.Version, false, err
	}
	if err := update.Install(tmp, exe); err != nil {
		_ = os.Remove(tmp)
		return rel.Version, false, err
	}
	return rel.Version, true, nil
}

// restartSelf runs the updated exe with the same arguments in this console and returns its exit code.
func restartSelf(args []string) int {
	exe, err := update.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitError
	}
	cmd := exec.Command(exe, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), envUpdated+"=1")
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		fmt.Fprintln(os.Stderr, err)
		return exitError
	}
	return exitOK
}

// headlessUpdates checks for a newer build now and then every updateCheckEvery. After installing one the
// process exits with exitUpdated: the Windows service restarts it (recovery action "restart") with the new exe.
func headlessUpdates(ctx context.Context, client *api.Client, lg *logx.File) {
	check := func() {
		cfg, err := client.Config(ctx)
		if err != nil {
			return
		}
		v, ok, err := selfUpdate(ctx, cfg, client, func(v string) { lg.Printf("updating to %s", v) })
		if err != nil {
			lg.Printf("update to %s failed: %v", v, err)
			return
		}
		if ok {
			if svc.Interactive() {
				lg.Printf("updated to %s; start sclogsync again to use it", v)
			} else {
				lg.Printf("updated to %s; the service restarts with the new version", v)
			}
			lg.Close()
			os.Exit(exitUpdated)
		}
	}
	check()
	t := time.NewTicker(updateCheckEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
		}
	}
}
