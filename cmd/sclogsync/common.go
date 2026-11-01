package main

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/cihanenescolak/ScLogAgent/internal/api"
	"github.com/cihanenescolak/ScLogAgent/internal/state"
	"github.com/cihanenescolak/ScLogAgent/internal/ui"
	"github.com/cihanenescolak/ScLogAgent/internal/uploader"
)

func newClient(server, token string, report func(uploader.Event)) *api.Client {
	c := api.New(server, token, version)
	if report != nil {
		c.OnRetry = func(path string, attempt int, err error) {
			report(uploader.Event{Kind: uploader.EvRetry, Msg: path, Index: attempt, Err: err})
		}
	}
	return c
}

func formatMillis(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).Local().Format("2006-01-02 15:04:05")
}

func lastUploadText(u *ui.UI, st *state.State) string {
	if st == nil || st.LastUpload == nil || st.LastUpload.At == 0 {
		return u.T("status_never")
	}
	s := formatMillis(st.LastUpload.At)
	if st.LastUpload.File != "" {
		s += " (" + st.LastUpload.File + ")"
	}
	return s
}

// consoleRenderer turns engine events into console output.
func consoleRenderer(u *ui.UI, verbose bool) func(uploader.Event) {
	heldShown := map[string]bool{} // "paused: new game version" once per live session
	return func(e uploader.Event) {
		base := filepath.Base(e.File)
		ts := time.Now().Format("15:04:05")
		if e.Channel != "" {
			base = "[" + e.Channel + "] " + base
			ts = "[" + e.Channel + "] " + ts
		}
		switch e.Kind {
		case uploader.EvInfo:
			u.Println(e.Msg)
		case uploader.EvWarn:
			u.Warn("warn", e.Err)
		case uploader.EvRetry:
			u.Warn("retry", e.Err, e.Index)
		case uploader.EvScanFile:
			u.Progress(u.T("pass_scanning"), fmt.Sprintf("%d/%d %s", e.Index, e.Total, base), e.Done, e.Of)
		case uploader.EvUploadStart:
			u.Progress(u.T("pass_uploading"), base, 0, e.Of)
		case uploader.EvUploadChunk:
			u.Progress(u.T("pass_uploading"), base, e.Done, e.Of)
		case uploader.EvUploadDone:
			switch {
			case e.Held:
				u.Warn("pass_uploaded_held", base, e.Lines)
			case e.Queued:
				u.OK("pass_uploaded_queued", base, e.Lines)
			default:
				u.OK("pass_uploaded", base, e.Lines, e.Events)
			}
		case uploader.EvAlreadyDone:
			if verbose {
				u.Println(u.Dim(u.T("pass_already", base)))
			}
		case uploader.EvSkipped:
			if verbose {
				u.Println(u.Dim(u.T("pass_skipped", base)))
			}
		case uploader.EvInProgress:
			u.Warn("pass_in_progress", base)
		case uploader.EvFailed:
			u.Err("pass_failed", base, e.Err)
		case uploader.EvPassDone:
			u.EndProgress()
			if s := e.Stats; s != nil {
				u.Info("pass_summary", s.Files, s.Uploaded, s.Already, s.Skipped, s.Failed, s.Pending+s.InProgress)
			}
		case uploader.EvReset:
			u.Warn("reset", e.Epoch)
		case uploader.EvLiveSent:
			if e.Held {
				if !heldShown[e.Channel+e.Session] {
					heldShown[e.Channel+e.Session] = true
					u.Println(u.Yellow(u.T("live_held", ts)))
				}
			} else {
				u.Println(u.T("live_sent", u.Dim(ts), e.Lines, e.ToLine))
			}
		case uploader.EvLiveResync:
			u.Warn("live_resync")
		case uploader.EvLiveComplete:
			u.Info("live_complete", e.Session)
		case uploader.EvLiveNewSession:
			u.Info("live_session", u.Cyan(e.Session))
		case uploader.EvLiveWaiting:
			u.Println(u.Dim(u.T("live_waiting", e.File)))
		case uploader.EvWeeklyLimit:
			u.EndProgress()
			u.Warn("weekly_limit", e.Until.Local().Format("2006-01-02 15:04"))
		case uploader.EvHeartbeat:
			if verbose {
				u.Println(u.Dim(e.String()))
			}
		}
	}
}
