// Package svc manages the "SCLogSync" Windows service (kardianos/service).
package svc

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/kardianos/service"
)

const (
	Name        = "SCLogSync"
	DisplayName = "SC Log Sync"
	Description = "Uploads Star Citizen Game.log files to SC Log Viewer in the background."
)

// State of the service.
type State int

const (
	NotInstalled State = iota
	Stopped
	Running
	Unknown
)

func (s State) Key() string {
	switch s {
	case Running:
		return "svc_state_running"
	case Stopped:
		return "svc_state_stopped"
	case NotInstalled:
		return "svc_state_missing"
	}
	return "svc_state_unknown"
}

// program adapts a blocking run(ctx) func to service.Interface.
type program struct {
	run    func(ctx context.Context) error
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
}

func (p *program) Start(s service.Service) error {
	ctx, cancel := context.WithCancel(context.Background())
	p.mu.Lock()
	p.cancel, p.done = cancel, make(chan struct{})
	p.mu.Unlock()
	go func() {
		defer close(p.done)
		_ = p.run(ctx)
	}()
	return nil
}

func (p *program) Stop(s service.Service) error {
	p.mu.Lock()
	cancel, done := p.cancel, p.done
	p.mu.Unlock()
	if cancel != nil {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
		}
	}
	return nil
}

func config(executable string) *service.Config {
	return &service.Config{
		Name:        Name,
		DisplayName: DisplayName,
		Description: Description,
		Arguments:   []string{"run"},
		Executable:  executable,
		Option: service.KeyValue{
			"StartType":              "automatic",
			"DelayedAutoStart":       true,
			"OnFailure":              "restart",
			"OnFailureDelayDuration": "30s",
			"OnFailureResetPeriod":   86400,
		},
	}
}

func get(executable string, run func(ctx context.Context) error) (service.Service, error) {
	if run == nil {
		run = func(context.Context) error { return nil }
	}
	return service.New(&program{run: run}, config(executable))
}

// Status queries the service state.
func Status() (State, error) {
	s, err := get("", nil)
	if err != nil {
		return Unknown, err
	}
	st, err := s.Status()
	if errors.Is(err, service.ErrNotInstalled) {
		return NotInstalled, nil
	}
	if err != nil {
		return Unknown, err
	}
	switch st {
	case service.StatusRunning:
		return Running, nil
	case service.StatusStopped:
		return Stopped, nil
	}
	return Unknown, nil
}

// Install registers the service for executable (absolute path).
func Install(executable string) error {
	s, err := get(executable, nil)
	if err != nil {
		return err
	}
	return s.Install()
}

func Uninstall() error {
	s, err := get("", nil)
	if err != nil {
		return err
	}
	return s.Uninstall()
}

func Start() error {
	s, err := get("", nil)
	if err != nil {
		return err
	}
	return s.Start()
}

// Stop stops the service and waits (up to timeout) until it reports stopped.
func Stop(timeout time.Duration) error {
	s, err := get("", nil)
	if err != nil {
		return err
	}
	if err := s.Stop(); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		st, err := s.Status()
		if err != nil || st == service.StatusStopped {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("service did not stop within %s", timeout)
}

// Run executes run under the service manager (or in the foreground when
// started from a console). Blocks until stopped.
func Run(run func(ctx context.Context) error) error {
	s, err := get("", run)
	if err != nil {
		return err
	}
	return s.Run()
}

// Interactive reports whether we were started from a console (not by the SCM).
func Interactive() bool { return service.Interactive() }
