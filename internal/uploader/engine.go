// Package uploader implements the sync logic: staged upload of finished log
// backups (check → begin → chunk → commit) and live tailing of Game.log.
package uploader

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cihanenescolak/ScLogAgent/internal/api"
	"github.com/cihanenescolak/ScLogAgent/internal/detect"
	"github.com/cihanenescolak/ScLogAgent/internal/prefilter"
	"github.com/cihanenescolak/ScLogAgent/internal/state"
)

// CheckBatch is the number of files per /api/ingest/check call.
const CheckBatch = 50

// MaxBatchBytes keeps chunk / live request bodies well under the server's
// 4 MB limit (JSON escaping can grow the text).
const MaxBatchBytes = 3 << 20

// SplitBatches splits lines into consecutive batches of at most maxLines lines
// and roughly maxBytes of text each. It returns the exclusive end index of
// every batch (empty input → no batches).
func SplitBatches(lines []api.Line, maxLines, maxBytes int) []int {
	var ends []int
	n, size := 0, 0
	for i, l := range lines {
		w := len(l.Text) + 16
		if n > 0 && (n >= maxLines || size+w > maxBytes) {
			ends = append(ends, i)
			n, size = 0, 0
		}
		n++
		size += w
	}
	if n > 0 {
		ends = append(ends, len(lines))
	}
	return ends
}

// maxResetsPerPass guards against an endless reset loop.
const maxResetsPerPass = 5

var (
	errReset   = errors.New("server data reset")
	errChanged = errors.New("file changed while uploading")
	// errWeeklyLimit stops a pass when the server's weekly upload limit is used up.
	errWeeklyLimit = errors.New("weekly upload limit reached")
)

// Engine runs passes and the live loop. Not safe for concurrent use.
type Engine struct {
	API     *api.Client
	Store   *state.Store
	Folder  string // game folder (contains Game.log and logbackups)
	Mode    string // "console" | "service"
	Version string
	Report  func(Event)
	// Channel labels events of a secondary folder ("PTU"). Secondary engines also skip
	// heartbeats (the primary one reports the agent to the server).
	Channel string

	PollInterval       time.Duration // Game.log polling
	SendInterval       time.Duration // minimum gap between live posts
	HeartbeatInterval  time.Duration
	BackupScanInterval time.Duration // look for new backups while live
	StableAge          time.Duration // backups younger than this are left for later

	cfg      *api.AgentConfig
	filter   *prefilter.Filter
	epoch    *int
	chunkMax int
	resetHit *int // set when a reset was seen during live/heartbeat
	batchID  string
	// pausedUntil: backup passes are skipped until then (weekly upload limit reached).
	pausedUntil time.Time
}

func (e *Engine) defaults() {
	if e.PollInterval == 0 {
		e.PollInterval = 2 * time.Second
	}
	if e.SendInterval == 0 {
		e.SendInterval = 5 * time.Second
	}
	if e.HeartbeatInterval == 0 {
		e.HeartbeatInterval = 60 * time.Second
	}
	if e.BackupScanInterval == 0 {
		e.BackupScanInterval = 20 * time.Second
	}
	if e.StableAge == 0 {
		e.StableAge = 5 * time.Second
	}
	if e.Mode == "" {
		e.Mode = "console"
	}
}

func (e *Engine) emit(ev Event) {
	ev.Channel = e.Channel
	if e.Report != nil {
		e.Report(ev)
	}
}

// Filter exposes the compiled prefilter (after Init).
func (e *Engine) Filter() *prefilter.Filter { return e.filter }

// Epoch is the server data epoch in use (after Init).
func (e *Engine) Epoch() *int { return e.epoch }

// Init fetches the prefilter config and the data epoch.
func (e *Engine) Init(ctx context.Context) error {
	e.defaults()
	cfg, err := e.API.Config(ctx)
	if err != nil {
		return fmt.Errorf("fetch agent config: %w", err)
	}
	if cfg.ProtocolVersion != 0 && cfg.ProtocolVersion != api.ProtocolVersion {
		e.emit(Event{Kind: EvWarn, Err: fmt.Errorf("server protocol version %d, agent speaks %d", cfg.ProtocolVersion, api.ProtocolVersion)})
	}
	f, errs := prefilter.Compile(*cfg)
	for _, err := range errs {
		e.emit(Event{Kind: EvWarn, Err: err})
	}
	if len(cfg.IncludePatterns) == 0 {
		e.emit(Event{Kind: EvWarn, Err: errors.New("server sent no include patterns; only header lines will be uploaded")})
	}
	e.cfg, e.filter = cfg, f
	e.chunkMax = cfg.ChunkMaxLines
	if e.chunkMax <= 0 {
		e.chunkMax = api.DefaultChunkMaxLines
	}
	epoch := cfg.DataEpoch
	if epoch == nil {
		me, err := e.API.Me(ctx)
		if err != nil {
			return fmt.Errorf("fetch account: %w", err)
		}
		epoch = &me.DataEpoch
	}
	st := e.Store.State()
	if st.Epoch != nil && *st.Epoch != *epoch {
		_ = e.Store.ResetSent(epoch)
	} else if st.Epoch == nil {
		_ = e.Store.Update(func(s *state.State) { s.Epoch = epoch })
	}
	e.epoch = epoch
	if e.Channel == "" {
		_ = e.Store.Update(func(s *state.State) { s.Mode, s.Folder, s.PID = e.Mode, e.Folder, os.Getpid() })
	}
	return nil
}

// BackupDir is <folder>\logbackups.
func (e *Engine) BackupDir() string { return filepath.Join(e.Folder, "logbackups") }

// GameLog is <folder>\Game.log.
func (e *Engine) GameLog() string { return filepath.Join(e.Folder, "Game.log") }

func (e *Engine) handleReset(err error) {
	ne := e.epoch
	if ae, ok := api.AsError(err); ok && ae.Epoch != nil {
		ne = ae.Epoch
	}
	e.applyEpoch(ne)
}

func (e *Engine) applyEpoch(ne *int) {
	e.epoch = ne
	_ = e.Store.ResetSent(ne)
	v := 0
	if ne != nil {
		v = *ne
	}
	e.emit(Event{Kind: EvReset, Epoch: v})
}

// Pass uploads every finished backup the server does not have yet. On a
// server reset it starts over with the new epoch.
func (e *Engine) Pass(ctx context.Context) (*PassStats, error) { return e.pass(ctx, false) }

// pass with quiet=true reports EvPassDone only when something happened
// (periodic re-scans during live mode).
func (e *Engine) pass(ctx context.Context, quiet bool) (*PassStats, error) {
	for i := 0; ; i++ {
		stats, err := e.passOnce(ctx)
		if errors.Is(err, errReset) && i < maxResetsPerPass {
			continue
		}
		if errors.Is(err, errWeeklyLimit) {
			err = nil // reported once via EvWeeklyLimit; the pass resumes after the reset
		}
		if err == nil && (!quiet || stats.Uploaded+stats.Failed+stats.InProgress > 0) {
			e.emit(Event{Kind: EvPassDone, Stats: stats})
		}
		return stats, err
	}
}

type pending struct {
	path  string
	entry *state.FileEntry
	lines []api.Line
}

func (e *Engine) passOnce(ctx context.Context) (*PassStats, error) {
	stats := &PassStats{}
	if time.Now().Before(e.pausedUntil) {
		return stats, nil
	}
	// one processing-log batch per pass on the server
	e.batchID = fmt.Sprintf("agent-%s-%d", strings.ToLower(e.Channel), time.Now().UnixNano())
	files := detect.BackupFiles(e.BackupDir())
	stats.Files = len(files)
	st := e.Store.State()
	now := time.Now()

	var batch []*pending
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := e.checkAndUpload(ctx, batch, stats)
		batch = batch[:0]
		return err
	}

	for i, path := range files {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		if now.Sub(fi.ModTime()) < e.StableAge {
			stats.Pending++
			continue
		}
		ent := st.Files[path]
		fresh := ent != nil && ent.Size == fi.Size() && ent.ModTime == fi.ModTime().UnixNano() && ent.Fingerprint == e.filter.Fingerprint
		if fresh {
			switch ent.Status {
			case state.FileComplete:
				stats.Already++
				continue
			case state.FileSkipped:
				stats.Skipped++
				continue
			case state.FileFailed:
				stats.Failed++
				continue
			}
			batch = append(batch, &pending{path: path, entry: ent})
		} else {
			p, err := e.hashFile(path, fi, i+1, len(files))
			if err != nil {
				e.emit(Event{Kind: EvFailed, File: path, Err: err})
				stats.Failed++
				continue
			}
			if p.entry.Status == state.FileSkipped {
				e.emit(Event{Kind: EvSkipped, File: path, Msg: "no_session"})
				stats.Skipped++
				continue
			}
			batch = append(batch, p)
		}
		if len(batch) >= CheckBatch {
			if err := flush(); err != nil {
				return stats, err
			}
		}
	}
	return stats, flush()
}

func (e *Engine) hashFile(path string, fi os.FileInfo, idx, total int) (*pending, error) {
	res, err := e.filterFile(path, func(done int64) {
		e.emit(Event{Kind: EvScanFile, File: path, Index: idx, Total: total, Done: done, Of: fi.Size()})
	})
	if err != nil {
		return nil, err
	}
	ent := &state.FileEntry{
		Size: fi.Size(), ModTime: fi.ModTime().UnixNano(), Fingerprint: e.filter.Fingerprint,
		SessionUUID: res.SessionUUID, Sha256: res.Sha256, LineCount: len(res.Lines),
		Status: state.FileHashed, UpdatedAt: time.Now().UnixMilli(),
	}
	if res.SessionUUID == "" {
		ent.Status = state.FileSkipped
		res.Lines = nil
	}
	e.saveEntry(path, ent)
	return &pending{path: path, entry: ent, lines: res.Lines}, nil
}

func (e *Engine) filterFile(path string, progress func(int64)) (*prefilter.Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return e.filter.FilterReader(f, progress)
}

func (e *Engine) saveEntry(path string, ent *state.FileEntry) {
	ent.UpdatedAt = time.Now().UnixMilli()
	if err := e.Store.Update(func(s *state.State) { s.Files[path] = ent }); err != nil {
		e.emit(Event{Kind: EvWarn, Err: fmt.Errorf("save state: %w", err)})
	}
}

func (e *Engine) checkAndUpload(ctx context.Context, batch []*pending, stats *PassStats) error {
	req := api.CheckRequest{Epoch: e.epoch}
	seen := map[string]bool{}
	for _, p := range batch {
		if !seen[p.entry.SessionUUID] {
			seen[p.entry.SessionUUID] = true
			req.Files = append(req.Files, api.CheckFile{SessionUUID: p.entry.SessionUUID, Sha256: p.entry.Sha256})
		}
	}
	resp, err := e.API.Check(ctx, req)
	if err != nil {
		if api.IsReset(err) {
			e.handleReset(err)
			return errReset
		}
		return err
	}
	states := map[string]api.FileState{}
	for _, r := range resp.Results {
		states[r.SessionUUID] = r.State
	}
	for _, p := range batch {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch states[p.entry.SessionUUID] {
		case api.StateComplete:
			p.entry.Status = state.FileComplete
			e.saveEntry(p.path, p.entry)
			stats.Already++
		case api.StateInProgress:
			e.emit(Event{Kind: EvInProgress, File: p.path})
			stats.InProgress++
		case api.StateMissing, api.StateLivePartial:
			err := e.uploadFile(ctx, p, stats)
			switch {
			case err == nil:
			case errors.Is(err, errChanged):
				stats.Pending++
			case errors.Is(err, errReset):
				return err
			case api.IsWeeklyLimit(err):
				e.pauseForWeeklyLimit(err)
				stats.Pending++
				return errWeeklyLimit
			case api.IsUnauthorized(err), api.IsTemporary(err), errors.Is(err, context.Canceled):
				return err
			default:
				e.emit(Event{Kind: EvFailed, File: p.path, Err: err})
				stats.Failed++
			}
		default:
			e.emit(Event{Kind: EvWarn, Err: fmt.Errorf("check: no usable state for %s", filepath.Base(p.path))})
		}
	}
	return nil
}

// pauseForWeeklyLimit stops backup uploads until the server's weekly limit resets
// (an hour from now if the answer had no reset time). Live data is not affected.
func (e *Engine) pauseForWeeklyLimit(err error) {
	until := time.Now().Add(time.Hour)
	if ae, ok := api.AsError(err); ok && !ae.ResetAt.IsZero() {
		until = ae.ResetAt
	}
	e.pausedUntil = until
	e.emit(Event{Kind: EvWeeklyLimit, Until: until})
}

func isGone(err error) bool {
	return api.Is(err, http.StatusGone, "") || api.Is(err, http.StatusNotFound, "")
}

// uploadFile runs begin/chunk/commit for one backup. A 410 (lease expired)
// restarts the file; a 422 marks it failed until its content changes.
func (e *Engine) uploadFile(ctx context.Context, p *pending, stats *PassStats) error {
	lines := p.lines
	if lines == nil {
		res, err := e.filterFile(p.path, nil)
		if err != nil {
			return err
		}
		if res.Sha256 != p.entry.Sha256 || res.SessionUUID != p.entry.SessionUUID {
			// Content changed since it was hashed: refresh the cache and let the next pass re-check it.
			if fi, err := os.Stat(p.path); err == nil {
				p.entry.Size, p.entry.ModTime = fi.Size(), fi.ModTime().UnixNano()
			}
			p.entry.Sha256, p.entry.SessionUUID, p.entry.LineCount = res.Sha256, res.SessionUUID, len(res.Lines)
			p.entry.Status = state.FileHashed
			e.saveEntry(p.path, p.entry)
			return errChanged
		}
		lines = res.Lines
	}
	p.lines = nil // free memory once uploaded

	name := filepath.Base(p.path)
	for attempt := 0; attempt < 3; attempt++ {
		br, err := e.API.Begin(ctx, api.BeginRequest{
			Epoch: e.epoch, SessionUUID: p.entry.SessionUUID, Sha256: p.entry.Sha256,
			LineCount: len(lines), Filename: name, Source: "agent", BatchID: e.batchID,
		})
		if err != nil {
			switch {
			case api.IsReset(err):
				e.handleReset(err)
				return errReset
			case api.Is(err, http.StatusConflict, "in_progress"):
				e.emit(Event{Kind: EvInProgress, File: p.path})
				stats.InProgress++
				return nil
			}
			return err
		}
		if br.AlreadyComplete {
			p.entry.Status = state.FileComplete
			e.saveEntry(p.path, p.entry)
			e.emit(Event{Kind: EvAlreadyDone, File: p.path})
			stats.Already++
			return nil
		}
		size := br.ChunkMaxLines
		if size <= 0 {
			size = e.chunkMax
		}
		bounds := SplitBatches(lines, size, MaxBatchBytes)
		chunks := len(bounds)
		e.emit(Event{Kind: EvUploadStart, File: p.path, Lines: len(lines), Of: int64(chunks)})

		restart, err := e.sendChunks(ctx, p.path, br.UploadID, lines, bounds)
		if err != nil {
			if errors.Is(err, errReset) || !api.IsTemporary(err) {
				e.abort(br.UploadID)
			}
			return err
		}
		if restart {
			continue
		}
		cr, err := e.API.Commit(ctx, br.UploadID)
		if err != nil {
			switch {
			case isGone(err):
				continue
			case api.IsReset(err):
				e.handleReset(err)
				return errReset
			case api.Is(err, http.StatusUnprocessableEntity, ""):
				p.entry.Status = state.FileFailed
				e.saveEntry(p.path, p.entry)
				return err
			}
			return err
		}
		p.entry.Status = state.FileComplete
		e.saveEntry(p.path, p.entry)
		_ = e.Store.Update(func(s *state.State) {
			s.LastUpload = &state.LastUpload{At: time.Now().UnixMilli(), File: name, Session: p.entry.SessionUUID, Kind: "backup", Lines: len(lines)}
			s.LastError = ""
		})
		stats.Uploaded++
		stats.Lines += len(lines)
		e.emit(Event{Kind: EvUploadDone, File: p.path, Lines: len(lines), Events: cr.Events, Queued: cr.Queued, Held: cr.Held})
		return nil
	}
	return fmt.Errorf("%s: upload lease kept expiring", name)
}

// sendChunks posts all chunks; restart=true means the lease expired.
func (e *Engine) sendChunks(ctx context.Context, path, uploadID string, lines []api.Line, bounds []int) (bool, error) {
	badSeq := 0
	chunks := len(bounds)
	for seq := 0; seq < chunks; {
		lo := 0
		if seq > 0 {
			lo = bounds[seq-1]
		}
		hi := bounds[seq]
		_, err := e.API.Chunk(ctx, api.ChunkRequest{UploadID: uploadID, Seq: seq, Lines: lines[lo:hi]})
		if err != nil {
			if ae, ok := api.AsError(err); ok && ae.Status == http.StatusConflict && ae.Code == "bad_seq" &&
				ae.ExpectedSeq != nil && *ae.ExpectedSeq >= 0 && *ae.ExpectedSeq <= chunks && badSeq < 3 {
				// e.g. a retried request that the server had already stored
				badSeq++
				seq = *ae.ExpectedSeq
				continue
			}
			if isGone(err) {
				return true, nil
			}
			if api.IsReset(err) {
				e.handleReset(err)
				return false, errReset
			}
			return false, err
		}
		badSeq = 0
		seq++
		e.emit(Event{Kind: EvUploadChunk, File: path, Done: int64(seq), Of: int64(chunks)})
	}
	return false, nil
}

func (e *Engine) abort(uploadID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := *e.API
	c.MaxRetries = 0
	_ = c.Abort(ctx, uploadID)
}
