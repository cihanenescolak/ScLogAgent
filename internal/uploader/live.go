package uploader

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/cihanenescolak/ScLogAgent/internal/api"
	"github.com/cihanenescolak/ScLogAgent/internal/prefilter"
	"github.com/cihanenescolak/ScLogAgent/internal/state"
	"github.com/cihanenescolak/ScLogAgent/internal/tail"
)

// liveSession is the send state of the Game.log currently being tailed.
type liveSession struct {
	session  string
	fromLine int        // first line the server has not acknowledged yet
	toLine   int        // last line read from disk
	pending  []api.Line // filtered lines in [fromLine, toLine] not yet sent
	complete bool       // server said the session is finalised; stop sending
	broken   bool       // server rejected it; wait for the backup upload
	lastSend time.Time
	warned   bool
}

func (l *liveSession) reset() { *l = liveSession{fromLine: 1} }

// Live tails Game.log and uploads new lines until ctx is cancelled. It also
// picks up new backups (the game moved the old Game.log to logbackups),
// sends heartbeats and re-sends everything after a server reset.
func (e *Engine) Live(ctx context.Context) error {
	e.defaults()
	t := tail.New(e.GameLog(), e.filter.Session)
	var ls liveSession
	ls.reset()

	poll := time.NewTicker(e.PollInterval)
	defer poll.Stop()
	hb := time.NewTicker(e.HeartbeatInterval)
	defer hb.Stop()
	scan := time.NewTicker(e.BackupScanInterval)
	defer scan.Stop()

	if err := e.heartbeat(ctx); err != nil && api.IsUnauthorized(err) {
		return err
	}
	waitingReported := false
	needPass := false

	for {
		if e.resetHit != nil {
			e.applyEpoch(e.resetHit)
			e.resetHit = nil
			t.Reset()
			ls.reset()
			needPass = true
		}
		if needPass {
			needPass = false
			if _, err := e.pass(ctx, true); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if api.IsUnauthorized(err) {
					return err
				}
				e.emit(Event{Kind: EvWarn, Err: fmt.Errorf("backup pass: %w", err)})
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-hb.C:
			if err := e.heartbeat(ctx); err != nil && api.IsUnauthorized(err) {
				return err
			}
		case <-scan.C:
			needPass = true
		case <-poll.C:
			missing, err := e.pollOnce(ctx, t, &ls)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if api.IsUnauthorized(err) {
					return err
				}
				e.emit(Event{Kind: EvWarn, Err: err})
			}
			if missing && !waitingReported {
				e.emit(Event{Kind: EvLiveWaiting, File: e.GameLog()})
			}
			waitingReported = missing
		}
	}
}

// pollOnce reads everything new from Game.log and sends it when due.
func (e *Engine) pollOnce(ctx context.Context, t *tail.Tailer, ls *liveSession) (missing bool, err error) {
	for range 64 { // bounded catch-up per tick
		p, err := t.Poll()
		if err != nil {
			return false, err
		}
		if p.Missing {
			return true, nil
		}
		if p.Rotated {
			ls.reset()
		}
		if len(p.Lines) == 0 {
			break
		}
		for _, l := range p.Lines {
			if e.filter.Keep(l.No, l.Text) {
				l.Text = prefilter.TruncateLine(l.Text)
				ls.pending = append(ls.pending, l)
			}
			ls.toLine = l.No
		}
		if ls.session == "" && t.Session() != "" {
			ls.session = t.Session()
			e.emit(Event{Kind: EvLiveNewSession, Session: ls.session, File: e.GameLog()})
		}
		if len(ls.pending) >= e.chunkMax {
			if err := e.sendLive(ctx, t, ls, true); err != nil {
				return false, err
			}
		}
	}
	return false, e.sendLive(ctx, t, ls, false)
}

// sendLive posts pending lines in batches of at most chunkMax lines.
func (e *Engine) sendLive(ctx context.Context, t *tail.Tailer, ls *liveSession, force bool) error {
	if ls.session == "" {
		return nil // header (with the session line) not read yet
	}
	if ls.complete || ls.broken {
		ls.pending = nil
		return nil
	}
	if len(ls.pending) == 0 || ls.toLine < ls.fromLine {
		return nil
	}
	if !force && time.Since(ls.lastSend) < e.SendInterval {
		return nil
	}
	for len(ls.pending) > 0 {
		n := SplitBatches(ls.pending, e.chunkMax, MaxBatchBytes)[0]
		batch := ls.pending[:n]
		to := ls.toLine
		if n < len(ls.pending) {
			to = ls.pending[n].No - 1 // the next batch starts at the next kept line
		}
		req := api.LiveRequest{
			Epoch: e.epoch, SessionUUID: ls.session, Filename: "Game.log",
			FromLine: ls.fromLine, ToLine: to, Lines: batch,
		}
		lr, err := e.API.Live(ctx, req)
		ls.lastSend = time.Now()
		if err != nil {
			ae, isAPI := api.AsError(err)
			switch {
			case api.IsReset(err):
				ne := e.epoch
				if ae.Epoch != nil {
					ne = ae.Epoch
				}
				e.resetHit = ne
				return nil
			case isAPI && ae.Status == http.StatusConflict && ae.Code == "resync":
				e.emit(Event{Kind: EvLiveResync, Session: ls.session})
				t.Reset()
				ls.reset()
				return nil
			case isAPI && ae.Status == http.StatusConflict && ae.Code == "complete":
				e.emit(Event{Kind: EvLiveComplete, Session: ls.session})
				ls.complete = true
				ls.pending = nil
				return nil
			case api.IsUnauthorized(err), api.IsTemporary(err), errors.Is(err, context.Canceled):
				if !ls.warned && api.IsTemporary(err) {
					ls.warned = true
					e.emit(Event{Kind: EvWarn, Err: fmt.Errorf("live upload delayed: %w", err)})
				}
				return err
			default:
				// Rejected request (4xx): stop live for this session; the
				// backup upload will deliver the full file later.
				ls.broken = true
				ls.pending = nil
				return fmt.Errorf("live upload rejected: %w", err)
			}
		}
		ls.warned = false
		ls.fromLine = to + 1
		ls.pending = ls.pending[n:]
		e.emit(Event{Kind: EvLiveSent, Session: ls.session, Lines: n, ToLine: to, Held: lr != nil && lr.Held})
		sent, sess := n, ls.session
		_ = e.Store.Update(func(s *state.State) {
			s.LastUpload = &state.LastUpload{At: time.Now().UnixMilli(), File: "Game.log", Session: sess, Kind: "live", Lines: sent}
			s.LastError = ""
		})
	}
	if len(ls.pending) == 0 {
		ls.pending = nil
	}
	return nil
}

func (e *Engine) heartbeat(ctx context.Context) error {
	if e.Channel != "" {
		return nil // reported by the primary engine
	}
	hctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c := *e.API
	c.MaxRetries = 0
	r, err := c.Heartbeat(hctx, api.HeartbeatRequest{AgentVersion: e.Version, Mode: e.Mode, Folder: e.Folder})
	if err != nil {
		if api.IsUnauthorized(err) {
			return err
		}
		return nil // transient; next tick
	}
	_ = e.Store.Update(func(s *state.State) { s.LastHeartbeat = time.Now().UnixMilli() })
	if e.epoch == nil || r.DataEpoch != *e.epoch {
		ne := r.DataEpoch
		e.resetHit = &ne
	}
	e.emit(Event{Kind: EvHeartbeat})
	return nil
}
