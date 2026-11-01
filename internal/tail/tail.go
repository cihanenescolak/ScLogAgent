// Package tail follows a growing Game.log by polling. The file is opened only
// for the duration of each read so the game can still rename/move it.
package tail

import (
	"bytes"
	"errors"
	"io"
	"os"

	"github.com/cihanenescolak/ScLogAgent/internal/api"
	"github.com/cihanenescolak/ScLogAgent/internal/prefilter"
)

// headerProbeBytes is how much of the file start is re-read to detect a new
// session written over the same path.
const headerProbeBytes = 64 << 10

// maxReadPerPoll bounds memory when catching up on a huge file.
const maxReadPerPoll = 8 << 20

// Tailer tracks one file path. Not safe for concurrent use.
type Tailer struct {
	Path string
	// SessionOf extracts a session id from a line ("" if none). Used to detect
	// a different log written to the same path.
	SessionOf func(string) string

	offset  int64
	lineNo  int
	info    os.FileInfo
	session string
}

// Poll result.
type Poll struct {
	// Rotated is set when the file was replaced/truncated; Lines then start at
	// line 1 of the new file and any previous per-file state must be dropped.
	Rotated bool
	Lines   []api.Line
	// Missing is set when the file does not exist (game never started).
	Missing bool
}

func New(path string, sessionOf func(string) string) *Tailer {
	return &Tailer{Path: path, SessionOf: sessionOf}
}

// Reset makes the next Poll read the file again from line 1 (Rotated=true).
func (t *Tailer) Reset() {
	t.offset, t.lineNo, t.info, t.session = 0, 0, nil, ""
}

// LineNo is the number of complete lines consumed so far.
func (t *Tailer) LineNo() int { return t.lineNo }

// Session is the session uuid seen in the current file ("" until seen).
func (t *Tailer) Session() string { return t.session }

// Poll reads newly completed lines. A trailing line without "\n" is left for
// the next poll (the game may still be writing it). Call repeatedly until it
// returns no lines to fully catch up.
func (t *Tailer) Poll() (Poll, error) {
	var p Poll
	fi, err := os.Stat(t.Path)
	if errors.Is(err, os.ErrNotExist) {
		p.Missing = true
		if t.info != nil {
			t.Reset()
		}
		return p, nil
	}
	if err != nil {
		return p, err
	}
	// On Windows the file id behind os.SameFile is loaded lazily from the path;
	// force it now, otherwise the old FileInfo would later read the new file's id.
	_ = os.SameFile(fi, fi)
	if t.info == nil {
		p.Rotated = true
	} else if !os.SameFile(t.info, fi) || fi.Size() < t.offset {
		t.Reset()
		p.Rotated = true
	}
	if !p.Rotated && t.session != "" && t.SessionOf != nil && fi.Size() != t.info.Size() {
		if s := t.probeSession(); s != "" && s != t.session {
			t.Reset()
			p.Rotated = true
		}
	}
	t.info = fi
	if fi.Size() == t.offset {
		return p, nil
	}

	f, err := os.Open(t.Path)
	if err != nil {
		return p, err
	}
	defer f.Close()
	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return p, err
	}
	n := min(fi.Size()-t.offset, maxReadPerPoll)
	buf := make([]byte, n)
	got, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return p, err
	}
	buf = buf[:got]

	last := bytes.LastIndexByte(buf, '\n')
	if last < 0 {
		// A single line longer than maxReadPerPoll: consume it as one line
		// once complete; until then wait (but never stall forever).
		if int64(got) < maxReadPerPoll {
			return p, nil
		}
		last = len(buf) - 1
	}
	data := buf[:last+1]
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		var raw []byte
		if i < 0 {
			raw, data = data, nil
		} else {
			raw, data = data[:i], data[i+1:]
		}
		t.lineNo++
		text := prefilter.DecodeLine(raw, t.lineNo == 1)
		if t.session == "" && t.SessionOf != nil {
			t.session = t.SessionOf(text)
		}
		p.Lines = append(p.Lines, api.Line{No: t.lineNo, Text: text})
	}
	t.offset += int64(last + 1)
	return p, nil
}

func (t *Tailer) probeSession() string {
	f, err := os.Open(t.Path)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, headerProbeBytes)
	n, _ := io.ReadFull(f, buf)
	for i, raw := range bytes.Split(buf[:n], []byte{'\n'}) {
		if s := t.SessionOf(prefilter.DecodeLine(raw, i == 0)); s != "" {
			return s
		}
	}
	return ""
}
