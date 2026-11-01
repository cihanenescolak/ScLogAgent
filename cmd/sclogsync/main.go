// Command sclogsync uploads Star Citizen Game.log files to an SC Log Viewer
// server: backups once, Game.log live. Runs interactively (double-click),
// headless ("run", used by the Windows service) or as a small service manager.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/cihanenescolak/ScLogAgent/internal/ui"
	"github.com/cihanenescolak/ScLogAgent/internal/update"
	"github.com/cihanenescolak/ScLogAgent/internal/winutil"
)

// Set at build time: -ldflags "-X main.version=1.2.3 -X main.defaultServer=https://..."
var (
	version       = "dev"
	defaultServer = ""
)

// Exit codes.
const (
	exitOK       = 0
	exitError    = 1
	exitUsage    = 2
	exitRunning  = 3
	exitAborted  = 130
	exitNoConfig = 4
	// exitUpdated: the exe was replaced by a newer build; the service manager restarts it.
	exitUpdated = 5
	// exitRestart (internal): run the freshly installed exe with the same arguments.
	exitRestart = 6
)

type options struct {
	command string
	server  string
	folder  string
	lang    string
	verbose bool
	once    bool
	purge   bool
	// PTU: --ptu-folder PATH also uploads that test-server folder; --no-ptu turns it off.
	ptuFolder string
	noPTU     bool
	// noUpdate turns off the automatic update from the server.
	noUpdate bool
}

func parseArgs(args []string, stderr io.Writer) (*options, error) {
	o := &options{}
	fs := flag.NewFlagSet("sclogsync", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.server, "server", "", "")
	fs.StringVar(&o.folder, "folder", "", "")
	fs.StringVar(&o.lang, "lang", "", "")
	fs.BoolVar(&o.verbose, "verbose", false, "")
	fs.BoolVar(&o.verbose, "v", false, "")
	fs.BoolVar(&o.once, "once", false, "")
	fs.BoolVar(&o.purge, "purge", false, "")
	fs.StringVar(&o.ptuFolder, "ptu-folder", "", "")
	fs.BoolVar(&o.noPTU, "no-ptu", false, "")
	fs.BoolVar(&o.noUpdate, "no-update", false, "")
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return nil, err
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		if o.command != "" {
			return nil, fmt.Errorf("unexpected argument %q", rest[0])
		}
		o.command = strings.ToLower(rest[0])
		rest = rest[1:]
	}
	if o.lang != "" {
		if !ui.IsLanguage(o.lang) {
			return nil, fmt.Errorf("--lang must be one of %s", strings.Join(ui.Languages, ", "))
		}
		o.lang = ui.NormalizeLang(o.lang)
	}
	if o.noPTU && o.ptuFolder != "" {
		return nil, errors.New("--ptu-folder and --no-ptu exclude each other")
	}
	switch o.command {
	case "", "run", "status", "start", "stop", "uninstall", "logout", "install-service", "version", "help":
	default:
		return nil, fmt.Errorf("unknown command %q", o.command)
	}
	return o, nil
}

func main() {
	os.Exit(realMain(os.Args[1:]))
}

func realMain(args []string) (code int) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "internal error: %v\n", r)
			code = exitError
		}
	}()

	if exe, err := update.Executable(); err == nil {
		update.Cleanup(exe) // leftovers of an earlier self-update
	}
	o, err := parseArgs(args, os.Stderr)
	if err != nil {
		u := ui.New(winutil.UILanguage(), false)
		fmt.Fprintln(os.Stderr, err)
		fmt.Fprintln(os.Stderr, u.T("usage"))
		return exitUsage
	}
	lang := o.lang
	if lang == "" {
		lang = winutil.UILanguage()
	}

	switch o.command {
	case "version":
		fmt.Println("sclogsync " + version)
		return exitOK
	case "help":
		fmt.Println(ui.New(lang, false).T("usage"))
		return exitOK
	case "run":
		return cmdRun(o)
	}

	color := winutil.EnableConsole()
	u := ui.New(lang, color)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch o.command {
	case "status":
		return cmdStatus(ctx, u, o)
	case "start", "stop", "uninstall":
		return cmdServiceControl(u, o)
	case "install-service":
		return cmdInstallService(u, o)
	case "logout":
		return cmdLogout(u)
	}
	code = cmdInteractive(ctx, u, o)
	if code == exitRestart {
		stop() // the new process handles Ctrl+C itself
		code = restartSelf(args)
	}
	if winutil.OwnsConsole() {
		// Double-clicked: keep the window open so the user can read the result.
		_, _ = u.ReadLine(context.Background(), "\n"+u.T("press_enter"))
	}
	return code
}

// exitFor maps an error from the interactive flow to an exit code.
func exitFor(u *ui.UI, err error) int {
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, ui.ErrAborted), errors.Is(err, context.Canceled):
		u.Info("aborted")
		return exitAborted
	case errors.Is(err, winutil.ErrAlreadyRunning):
		u.Err("another_instance")
		return exitRunning
	}
	u.Err("error", err)
	return exitError
}
