// Package ui is the small console toolkit: translated strings, colors,
// prompts that respect Ctrl+C, progress bars and a spinner.
package ui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ErrAborted is returned by prompts when input ends or Ctrl+C is pressed.
var ErrAborted = errors.New("aborted")

// Languages supported by the console.
var Languages = []string{"en", "tr", "de", "fr", "es", "it", "pt", "pl", "ru", "zh", "ja", "ko"}

// IsLanguage reports whether l (e.g. "de", "pt-BR") maps to a supported language.
func IsLanguage(l string) bool {
	p := langPrefix(l)
	for _, x := range Languages {
		if x == p {
			return true
		}
	}
	return false
}

// NormalizeLang maps a language tag to a supported language, falling back to "en".
func NormalizeLang(l string) string {
	if IsLanguage(l) {
		return langPrefix(l)
	}
	return "en"
}

func langPrefix(l string) string {
	l = strings.ToLower(strings.TrimSpace(l))
	if len(l) > 2 {
		l = l[:2]
	}
	return l
}

// UI writes to Out and reads lines from In.
type UI struct {
	Lang  string
	Color bool
	Out   io.Writer

	mu       sync.Mutex
	lines    chan string
	inOnce   sync.Once
	in       io.Reader
	progress bool // a progress line is currently drawn
}

func New(lang string, color bool) *UI {
	return &UI{Lang: NormalizeLang(lang), Color: color, Out: os.Stdout, in: os.Stdin}
}

// NewWith is New with explicit streams (tests).
func NewWith(lang string, out io.Writer, in io.Reader) *UI {
	return &UI{Lang: NormalizeLang(lang), Out: out, in: in}
}

// T translates key and formats args.
func (u *UI) T(key string, args ...any) string {
	m := messages[u.Lang]
	s, ok := m[key]
	if !ok {
		s, ok = messages["en"][key]
	}
	if !ok {
		s = key
	}
	if len(args) == 0 {
		return s
	}
	return fmt.Sprintf(s, args...)
}

const (
	cReset  = "\x1b[0m"
	cBold   = "\x1b[1m"
	cDim    = "\x1b[2m"
	cRed    = "\x1b[31m"
	cGreen  = "\x1b[32m"
	cYellow = "\x1b[33m"
	cCyan   = "\x1b[36m"
)

func (u *UI) paint(code, s string) string {
	if !u.Color {
		return s
	}
	return code + s + cReset
}

func (u *UI) Cyan(s string) string   { return u.paint(cCyan, s) }
func (u *UI) Green(s string) string  { return u.paint(cGreen, s) }
func (u *UI) Yellow(s string) string { return u.paint(cYellow, s) }
func (u *UI) Red(s string) string    { return u.paint(cRed, s) }
func (u *UI) Dim(s string) string    { return u.paint(cDim, s) }
func (u *UI) Bold(s string) string   { return u.paint(cBold, s) }

// Println prints a line, clearing any progress bar first.
func (u *UI) Println(s string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.clearProgressLocked()
	fmt.Fprintln(u.Out, s)
}

func (u *UI) Info(key string, args ...any) { u.Println(u.T(key, args...)) }
func (u *UI) OK(key string, args ...any)   { u.Println(u.Green("✓ ") + u.T(key, args...)) }
func (u *UI) Warn(key string, args ...any) { u.Println(u.Yellow("! " + u.T(key, args...))) }
func (u *UI) Err(key string, args ...any)  { u.Println(u.Red("✗ " + u.T(key, args...))) }

func (u *UI) clearProgressLocked() {
	if u.progress {
		fmt.Fprint(u.Out, "\r\x1b[2K")
		if !u.Color {
			fmt.Fprint(u.Out, "\r"+strings.Repeat(" ", 79)+"\r")
		}
		u.progress = false
	}
}

// Progress draws a single-line progress bar (overwritten on each call).
func (u *UI) Progress(label, name string, done, total int64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	const width = 24
	frac := 0.0
	if total > 0 {
		frac = float64(done) / float64(total)
	}
	frac = max(0, min(frac, 1))
	filled := int(frac * width)
	bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
	name = shorten(name, 38)
	line := fmt.Sprintf("  %-12s %s %3d%%  %s", label, u.Cyan(bar), int(frac*100), u.Dim(name))
	if u.Color {
		fmt.Fprint(u.Out, "\r\x1b[2K"+line)
	} else {
		fmt.Fprint(u.Out, "\r"+line)
	}
	u.progress = true
}

// EndProgress clears the progress line.
func (u *UI) EndProgress() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.clearProgressLocked()
}

func shorten(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return "…" + string(r[len(r)-n+1:])
}

// Spinner shows "label ⠋" until stop is called.
func (u *UI) Spinner(label string) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
		start := time.Now()
		t := time.NewTicker(120 * time.Millisecond)
		defer t.Stop()
		for i := 0; ; i++ {
			u.mu.Lock()
			fmt.Fprintf(u.Out, "\r  %s %s %s", u.Cyan(frames[i%len(frames)]), label, u.Dim(fmt.Sprintf("(%ds)", int(time.Since(start).Seconds()))))
			u.progress = true
			u.mu.Unlock()
			select {
			case <-done:
				return
			case <-t.C:
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			wg.Wait()
			u.EndProgress()
		})
	}
}

func (u *UI) startReader() {
	u.inOnce.Do(func() {
		u.lines = make(chan string)
		go func() {
			defer close(u.lines)
			br := bufio.NewReader(u.in)
			for {
				s, err := br.ReadString('\n')
				if s != "" || err == nil {
					u.lines <- strings.TrimRight(s, "\r\n")
				}
				if err != nil {
					return
				}
			}
		}()
	})
}

// ReadLine prints prompt and waits for a line (or ctx cancel).
func (u *UI) ReadLine(ctx context.Context, prompt string) (string, error) {
	u.mu.Lock()
	u.clearProgressLocked()
	fmt.Fprint(u.Out, prompt)
	u.mu.Unlock()
	u.startReader()
	select {
	case <-ctx.Done():
		fmt.Fprintln(u.Out)
		return "", ErrAborted
	case s, ok := <-u.lines:
		if !ok {
			fmt.Fprintln(u.Out)
			return "", ErrAborted
		}
		return strings.TrimSpace(s), nil
	}
}

// Ask prompts with a default shown in brackets; empty input returns def.
func (u *UI) Ask(ctx context.Context, label, def string) (string, error) {
	p := u.Bold(label)
	if def != "" {
		p += " " + u.Dim("["+def+"]")
	}
	s, err := u.ReadLine(ctx, p+": ")
	if err != nil {
		return "", err
	}
	if s == "" {
		return def, nil
	}
	return s, nil
}

// Confirm asks a yes/no question in the UI language.
func (u *UI) Confirm(ctx context.Context, question string, def bool) (bool, error) {
	hint := u.T("yes_no_default_no")
	if def {
		hint = u.T("yes_no_default_yes")
	}
	for {
		s, err := u.ReadLine(ctx, u.Bold(question)+" "+hint+" ")
		if err != nil {
			return false, err
		}
		s = strings.ToLower(s)
		if s == "" {
			return def, nil
		}
		if inList(s, u.T("yes_chars")) {
			return true, nil
		}
		if inList(s, u.T("no_chars")) {
			return false, nil
		}
		u.Warn("invalid_choice")
	}
}

func inList(s, csv string) bool {
	for _, x := range strings.Split(csv, ",") {
		if s == x {
			return true
		}
	}
	return false
}

// Choose asks for a number in [1, n]. def (1-based, 0 = none) is used on empty input.
func (u *UI) Choose(ctx context.Context, n, def int) (int, error) {
	label := u.T("choose")
	d := ""
	if def > 0 {
		d = strconv.Itoa(def)
	}
	for {
		s, err := u.Ask(ctx, label, d)
		if err != nil {
			return 0, err
		}
		if v, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && v >= 1 && v <= n {
			return v, nil
		}
		u.Warn("invalid_choice")
	}
}
