// Package prefilter selects the log lines worth sending (patterns come from
// GET /api/agent/config) and computes the payload hash of the server's ingest
// protocol: sha256(hex) of "<lineNo>\t<text>\n" for every kept line.
package prefilter

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/cihanenescolak/ScLogAgent/internal/api"
)

// DefaultSessionPattern mirrors the server's session pattern (used when the config has none).
const DefaultSessionPattern = `@session:\s+'([0-9a-fA-F-]{36})'`

// Filter is a compiled prefilter configuration.
type Filter struct {
	include        *regexp.Regexp // alternation of all include patterns (nil = none)
	header         *regexp.Regexp
	headerMaxLines int
	session        *regexp.Regexp
	// Fingerprint identifies the pattern set (part of the local hash cache key).
	Fingerprint string
}

// Compile builds a Filter. Invalid patterns are skipped and reported in the
// returned error slice; Compile itself never fails.
func Compile(cfg api.AgentConfig) (*Filter, []error) {
	var errs []error
	f := &Filter{headerMaxLines: cfg.HeaderMaxLines}
	f.include, errs = combine(cfg.IncludePatterns, errs)
	f.header, errs = combine(cfg.HeaderPatterns, errs)
	sp := cfg.SessionPattern
	if sp == "" {
		sp = DefaultSessionPattern
	}
	re, err := regexp.Compile(sp)
	if err != nil {
		errs = append(errs, fmt.Errorf("session pattern %q: %w", sp, err))
		re = regexp.MustCompile(DefaultSessionPattern)
	}
	f.session = re

	h := sha256.New()
	for _, p := range cfg.IncludePatterns {
		io.WriteString(h, "i\x00"+p+"\x00")
	}
	for _, p := range cfg.HeaderPatterns {
		io.WriteString(h, "h\x00"+p+"\x00")
	}
	io.WriteString(h, "s\x00"+sp+"\x00m"+strconv.Itoa(cfg.HeaderMaxLines))
	f.Fingerprint = hex.EncodeToString(h.Sum(nil))[:16]
	return f, errs
}

func combine(patterns []string, errs []error) (*regexp.Regexp, []error) {
	var ok []string
	for _, p := range patterns {
		if p == "" {
			continue
		}
		if _, err := regexp.Compile(p); err != nil {
			errs = append(errs, fmt.Errorf("pattern %q: %w", p, err))
			continue
		}
		ok = append(ok, "(?:"+p+")")
	}
	if len(ok) == 0 {
		return nil, errs
	}
	re, err := regexp.Compile(strings.Join(ok, "|"))
	if err != nil {
		return nil, append(errs, err)
	}
	return re, errs
}

// Keep reports whether the line with 1-based number lineNo is sent.
func (f *Filter) Keep(lineNo int, text string) bool {
	if f.header != nil && lineNo <= f.headerMaxLines && f.header.MatchString(text) {
		return true
	}
	return f.include != nil && f.include.MatchString(text)
}

// Session returns the session uuid if text is the session line, else "".
func (f *Filter) Session(text string) string {
	m := f.session.FindStringSubmatch(text)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// HeaderMaxLines is the configured header window.
func (f *Filter) HeaderMaxLines() int { return f.headerMaxLines }

// Hasher accumulates the payload hash.
type Hasher struct {
	h   hash.Hash
	buf []byte
	n   int
}

func NewHasher() *Hasher { return &Hasher{h: sha256.New()} }

func (h *Hasher) Add(lineNo int, text string) {
	h.buf = strconv.AppendInt(h.buf[:0], int64(lineNo), 10)
	h.buf = append(h.buf, '\t')
	h.buf = append(h.buf, text...)
	h.buf = append(h.buf, '\n')
	h.h.Write(h.buf)
	h.n++
}

func (h *Hasher) Count() int  { return h.n }
func (h *Hasher) Sum() string { return hex.EncodeToString(h.h.Sum(nil)) }
func HashLines(lines []api.Line) string {
	h := NewHasher()
	for _, l := range lines {
		h.Add(l.No, l.Text)
	}
	return h.Sum()
}

// Result of filtering a whole file.
type Result struct {
	SessionUUID string
	Lines       []api.Line
	Sha256      string
	TotalLines  int
}

// FilterReader streams r line by line. progress (optional) receives the number
// of bytes consumed so far.
func (f *Filter) FilterReader(r io.Reader, progress func(bytes int64)) (*Result, error) {
	lr := NewLineReader(r)
	res := &Result{}
	h := NewHasher()
	var lastReport int64
	for {
		text, ok, err := lr.Next()
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		n := lr.LineNo()
		if res.SessionUUID == "" {
			res.SessionUUID = f.Session(text)
		}
		if f.Keep(n, text) {
			text = TruncateLine(text)
			res.Lines = append(res.Lines, api.Line{No: n, Text: text})
			h.Add(n, text)
		}
		if progress != nil && lr.Offset()-lastReport >= 1<<20 {
			lastReport = lr.Offset()
			progress(lastReport)
		}
	}
	if progress != nil {
		progress(lr.Offset())
	}
	res.TotalLines = lr.LineNo()
	res.Sha256 = h.Sum()
	return res, nil
}
