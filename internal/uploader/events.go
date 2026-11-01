package uploader

import (
	"fmt"
	"path/filepath"
	"time"
)

// Kind of progress event emitted by the Engine.
type Kind int

const (
	EvInfo           Kind = iota // Msg
	EvWarn                       // Err
	EvRetry                      // Msg=path, Index=attempt, Err
	EvScanFile                   // File, Index, Total, Done/Of bytes
	EvUploadStart                // File, Lines, Of=chunks
	EvUploadChunk                // File, Done=chunks sent, Of=chunks
	EvUploadDone                 // File, Lines, Events
	EvAlreadyDone                // File (server already had it)
	EvSkipped                    // File, Msg=reason ("no_session")
	EvInProgress                 // File (another client is uploading it)
	EvFailed                     // File, Err
	EvPassDone                   // Stats
	EvReset                      // Epoch
	EvLiveSent                   // Session, Lines, ToLine
	EvLiveResync                 // Session
	EvLiveComplete               // Session
	EvLiveNewSession             // Session, File
	EvLiveWaiting                // File (Game.log missing)
	EvHeartbeat                  //
	EvWeeklyLimit                // Until (weekly upload limit reached; backups pause until then)
)

// Event is a progress notification; the console UI renders it, the service logs it.
type Event struct {
	Kind    Kind
	File    string
	Session string
	Msg     string
	Err     error
	Index   int
	Total   int
	Done    int64
	Of      int64
	Lines   int
	ToLine  int
	Events  int
	Epoch   int
	Stats   *PassStats
	// Channel is set by engines that watch a test-server folder ("PTU"); empty for LIVE.
	Channel string
	Queued  bool // EvUploadDone: waiting in the server's processing queue
	Held    bool // EvUploadDone / EvLiveSent: waiting for the site to support this game version
	Until   time.Time
}

// PassStats summarises one backup pass.
type PassStats struct {
	Files      int // backup files seen
	Uploaded   int
	Already    int // complete on server / cached
	Skipped    int // no session uuid
	InProgress int
	Failed     int
	Pending    int // too fresh, retried later
	Lines      int // lines uploaded
}

// String renders an event as an English log line (service log).
func (e Event) String() string {
	s := e.str()
	if e.Channel != "" {
		return "[" + e.Channel + "] " + s
	}
	return s
}

func (e Event) str() string {
	base := filepath.Base(e.File)
	switch e.Kind {
	case EvInfo:
		return e.Msg
	case EvWarn:
		return fmt.Sprintf("warning: %v", e.Err)
	case EvRetry:
		return fmt.Sprintf("retry %d for %s: %v", e.Index, e.Msg, e.Err)
	case EvScanFile:
		return fmt.Sprintf("scanning %d/%d %s", e.Index, e.Total, base)
	case EvUploadStart:
		return fmt.Sprintf("uploading %s (%d lines, %d chunks)", base, e.Lines, e.Of)
	case EvUploadChunk:
		return fmt.Sprintf("uploading %s chunk %d/%d", base, e.Done, e.Of)
	case EvUploadDone:
		if e.Held {
			return fmt.Sprintf("uploaded %s: %d lines, waiting for the site to support this game version", base, e.Lines)
		}
		if e.Queued {
			return fmt.Sprintf("uploaded %s: %d lines, queued for processing", base, e.Lines)
		}
		return fmt.Sprintf("uploaded %s: %d lines, %d events", base, e.Lines, e.Events)
	case EvAlreadyDone:
		return fmt.Sprintf("already on server: %s", base)
	case EvSkipped:
		return fmt.Sprintf("skipped %s (%s)", base, e.Msg)
	case EvInProgress:
		return fmt.Sprintf("another client is uploading %s, will retry later", base)
	case EvFailed:
		return fmt.Sprintf("failed %s: %v", base, e.Err)
	case EvPassDone:
		s := e.Stats
		if s == nil {
			s = &PassStats{}
		}
		return fmt.Sprintf("pass done: %d files, %d uploaded, %d already, %d skipped, %d in progress, %d failed, %d pending",
			s.Files, s.Uploaded, s.Already, s.Skipped, s.InProgress, s.Failed, s.Pending)
	case EvReset:
		return fmt.Sprintf("server data was reset (epoch %d); re-sending everything", e.Epoch)
	case EvLiveSent:
		return fmt.Sprintf("live: sent %d lines of session %s (up to line %d)", e.Lines, e.Session, e.ToLine)
	case EvLiveResync:
		return fmt.Sprintf("live: server asked to resync session %s", e.Session)
	case EvLiveComplete:
		return fmt.Sprintf("live: session %s is complete on server", e.Session)
	case EvLiveNewSession:
		return fmt.Sprintf("live: following session %s", e.Session)
	case EvLiveWaiting:
		return fmt.Sprintf("live: waiting for %s", e.File)
	case EvHeartbeat:
		return "heartbeat ok"
	case EvWeeklyLimit:
		return fmt.Sprintf("weekly upload limit reached; backup uploads pause until %s (live data continues)", e.Until.Local().Format("2006-01-02 15:04"))
	}
	return fmt.Sprintf("event %d", e.Kind)
}
