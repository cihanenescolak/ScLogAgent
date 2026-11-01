package uploader

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cihanenescolak/ScLogAgent/internal/api"
	"github.com/cihanenescolak/ScLogAgent/internal/state"
	"github.com/cihanenescolak/ScLogAgent/internal/tail"
)

func sessionID(n int) string { return fmt.Sprintf("%08d-0000-0000-0000-000000000000", n) }

// logText builds a log of `keep` kept lines interleaved with noise.
func logText(session string, keep int) string {
	var b strings.Builder
	b.WriteString("Log started\r\n")
	if session != "" {
		b.WriteString("<t> @session: '" + session + "'\r\n")
	}
	for i := 0; i < keep; i++ {
		fmt.Fprintf(&b, "noise %d\r\n<Keep> event %d\r\n", i, i)
	}
	return b.String()
}

type testEnv struct {
	srv    *fakeServer
	eng    *Engine
	folder string
	events []Event
	mu     sync.Mutex
}

func (te *testEnv) kinds(k Kind) int {
	te.mu.Lock()
	defer te.mu.Unlock()
	n := 0
	for _, e := range te.events {
		if e.Kind == k {
			n++
		}
	}
	return n
}

func newEnv(t *testing.T) *testEnv {
	te := &testEnv{srv: newFakeServer(t), folder: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(te.folder, "logbackups"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := api.New(te.srv.srv.URL, testToken, "test")
	c.MaxRetries = 0
	te.eng = &Engine{
		API: c, Store: state.Open(t.TempDir()), Folder: te.folder, Version: "test",
		Report: func(e Event) {
			te.mu.Lock()
			te.events = append(te.events, e)
			te.mu.Unlock()
		},
		StableAge: time.Millisecond, PollInterval: 20 * time.Millisecond, SendInterval: time.Nanosecond,
		HeartbeatInterval: time.Hour, BackupScanInterval: 50 * time.Millisecond,
	}
	if err := te.eng.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	return te
}

func (te *testEnv) backup(t *testing.T, name, content string) string {
	p := filepath.Join(te.folder, "logbackups", name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(p, old, old)
	return p
}

func expectedSha(t *testing.T, te *testEnv, content string) (string, int) {
	res, err := te.eng.Filter().FilterReader(strings.NewReader(content), nil)
	if err != nil {
		t.Fatal(err)
	}
	return res.Sha256, len(res.Lines)
}

func TestPassUploadsSkipsAndCaches(t *testing.T) {
	te := newEnv(t)
	c1, c2 := logText(sessionID(1), 4), logText(sessionID(2), 2)
	te.backup(t, "a.log", c1)
	te.backup(t, "b.log", c2)
	te.backup(t, "nosession.log", logText("", 3))
	sha2, _ := expectedSha(t, te, c2)
	te.srv.files[sessionID(2)] = &srvFile{sha: sha2, complete: true}

	st, err := te.eng.Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Uploaded != 1 || st.Already != 1 || st.Skipped != 1 || st.Failed != 0 {
		t.Fatalf("stats %+v", st)
	}
	sha1, n1 := expectedSha(t, te, c1)
	f := te.srv.file(sessionID(1))
	if f == nil || !f.complete || f.sha != sha1 || len(f.lines) != n1 || n1 != 5 {
		t.Fatalf("server file %+v (want %d lines)", f, n1)
	}
	// 5 lines in chunks of 3 → 2 chunks.
	if got := te.srv.count("/api/ingest/chunk"); got != 2 {
		t.Fatalf("chunks %d", got)
	}

	// Second pass: everything cached, no server calls for uploads or checks.
	checks := te.srv.count("/api/ingest/check")
	st, err = te.eng.Pass(context.Background())
	if err != nil || st.Uploaded != 0 || st.Already != 2 || st.Skipped != 1 {
		t.Fatalf("second pass %+v %v", st, err)
	}
	if te.srv.count("/api/ingest/check") != checks || te.srv.count("/api/ingest/begin") != 1 {
		t.Fatal("second pass hit the server")
	}
}

func TestInProgressRetriedNextPass(t *testing.T) {
	te := newEnv(t)
	te.backup(t, "a.log", logText(sessionID(1), 2))
	te.srv.on("/api/ingest/begin", reply(409, map[string]string{"error": "in_progress"}))
	st, err := te.eng.Pass(context.Background())
	if err != nil || st.InProgress != 1 || st.Uploaded != 0 {
		t.Fatalf("first %+v %v", st, err)
	}
	st, err = te.eng.Pass(context.Background())
	if err != nil || st.Uploaded != 1 {
		t.Fatalf("second %+v %v", st, err)
	}
}

func TestResetRestartsPassWithNewEpoch(t *testing.T) {
	te := newEnv(t)
	te.backup(t, "a.log", logText(sessionID(1), 2))
	te.srv.on("/api/ingest/check", func(w http.ResponseWriter, _ map[string]any) bool {
		te.srv.mu.Lock()
		te.srv.epoch = 7
		te.srv.mu.Unlock()
		writeJSON(w, 409, map[string]any{"error": "reset", "epoch": 7})
		return true
	})
	st, err := te.eng.Pass(context.Background())
	if err != nil || st.Uploaded != 1 {
		t.Fatalf("%+v %v", st, err)
	}
	if e := te.eng.Epoch(); e == nil || *e != 7 {
		t.Fatalf("epoch %v", e)
	}
	if te.kinds(EvReset) != 1 {
		t.Fatal("no reset event")
	}
	if ep := te.eng.Store.State().Epoch; ep == nil || *ep != 7 {
		t.Fatal("epoch not persisted")
	}
}

func TestResetClearsCompleteCache(t *testing.T) {
	te := newEnv(t)
	te.backup(t, "a.log", logText(sessionID(1), 2))
	if st, _ := te.eng.Pass(context.Background()); st.Uploaded != 1 {
		t.Fatal("initial upload")
	}
	// Website data deleted: server forgets files and bumps the epoch.
	te.srv.mu.Lock()
	te.srv.epoch = 2
	te.srv.files = map[string]*srvFile{}
	te.srv.mu.Unlock()
	// Cached as complete → no check at all; the next check-carrying call is
	// only reached after a reset seen elsewhere (heartbeat/live).
	te.eng.applyEpoch(func() *int { v := 2; return &v }())
	st, err := te.eng.Pass(context.Background())
	if err != nil || st.Uploaded != 1 {
		t.Fatalf("%+v %v", st, err)
	}
}

func TestExpiredLeaseRestartsFile(t *testing.T) {
	te := newEnv(t)
	te.backup(t, "a.log", logText(sessionID(1), 4))
	te.srv.on("/api/ingest/chunk", reply(410, map[string]string{"error": "expired"}))
	st, err := te.eng.Pass(context.Background())
	if err != nil || st.Uploaded != 1 {
		t.Fatalf("%+v %v", st, err)
	}
	if te.srv.count("/api/ingest/begin") != 2 {
		t.Fatalf("begin calls %d", te.srv.count("/api/ingest/begin"))
	}
	if f := te.srv.file(sessionID(1)); f == nil || !f.complete {
		t.Fatal("not complete")
	}
}

func TestBadSeqResumes(t *testing.T) {
	te := newEnv(t)
	te.backup(t, "a.log", logText(sessionID(1), 7)) // 8 lines → 3 chunks
	// Chunk 0 is handled normally; chunk 1 gets a bogus bad_seq (expected 2),
	// after which the real server answers bad_seq 1. The client must follow
	// expectedSeq both ways and still produce a verified upload.
	te.srv.on("/api/ingest/chunk", func(w http.ResponseWriter, body map[string]any) bool {
		return false
	})
	te.srv.on("/api/ingest/chunk", reply(409, map[string]any{"error": "bad_seq", "expectedSeq": 2}))
	st, err := te.eng.Pass(context.Background())
	if err != nil || st.Uploaded != 1 {
		t.Fatalf("%+v %v", st, err)
	}
}

func TestVerifyFailedMarksFailed(t *testing.T) {
	te := newEnv(t)
	p := te.backup(t, "a.log", logText(sessionID(1), 2))
	te.srv.on("/api/ingest/commit", reply(422, map[string]string{"error": "parse_failed"}))
	st, err := te.eng.Pass(context.Background())
	if err != nil || st.Failed != 1 || st.Uploaded != 0 {
		t.Fatalf("%+v %v", st, err)
	}
	if e := te.eng.Store.State().Files[p]; e == nil || e.Status != state.FileFailed {
		t.Fatalf("entry %+v", e)
	}
	// Not retried while unchanged.
	begins := te.srv.count("/api/ingest/begin")
	st, _ = te.eng.Pass(context.Background())
	if st.Failed != 1 || te.srv.count("/api/ingest/begin") != begins {
		t.Fatalf("retried: %+v", st)
	}
}

func TestUnauthorizedStopsPass(t *testing.T) {
	te := newEnv(t)
	te.backup(t, "a.log", logText(sessionID(1), 2))
	te.eng.API.Token = "wrong"
	if _, err := te.eng.Pass(context.Background()); !api.IsUnauthorized(err) {
		t.Fatalf("err %v", err)
	}
}

func TestSplitBatches(t *testing.T) {
	lines := []api.Line{{No: 1, Text: "aaaa"}, {No: 2, Text: "bbbb"}, {No: 3, Text: strings.Repeat("c", 100)}, {No: 4, Text: "d"}}
	got := SplitBatches(lines, 2, 1000)
	if fmt.Sprint(got) != "[2 4]" {
		t.Fatalf("by lines %v", got)
	}
	got = SplitBatches(lines, 10, 60)
	if fmt.Sprint(got) != "[2 3 4]" {
		t.Fatalf("by bytes %v", got)
	}
	if len(SplitBatches(nil, 2, 10)) != 0 {
		t.Fatal("empty")
	}
}

// --- live ---

func writeGameLog(t *testing.T, te *testEnv, s string, appendMode bool) {
	flag := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if appendMode {
		flag = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	f, err := os.OpenFile(te.eng.GameLog(), flag, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func TestLiveSendResyncComplete(t *testing.T) {
	te := newEnv(t)
	ctx := context.Background()
	tl := tail.New(te.eng.GameLog(), te.eng.Filter().Session)
	var ls liveSession
	ls.reset()
	sess := sessionID(9)

	if missing, err := te.eng.pollOnce(ctx, tl, &ls); !missing || err != nil {
		t.Fatalf("missing=%v err=%v", missing, err)
	}

	writeGameLog(t, te, logText(sess, 2), false) // 6 lines, 3 kept
	if _, err := te.eng.pollOnce(ctx, tl, &ls); err != nil {
		t.Fatal(err)
	}
	if len(te.srv.live) != 1 || te.srv.live[0].FromLine != 1 || te.srv.live[0].ToLine != 6 || len(te.srv.live[0].Lines) != 3 {
		t.Fatalf("first live %+v", te.srv.live)
	}

	writeGameLog(t, te, "noise\n<Keep> x\n", true)
	if _, err := te.eng.pollOnce(ctx, tl, &ls); err != nil {
		t.Fatal(err)
	}
	if r := te.srv.live[1]; r.FromLine != 7 || r.ToLine != 8 || r.Lines[0].No != 8 {
		t.Fatalf("second live %+v", r)
	}
	if f := te.srv.file(sess); f.lastLine != 8 || len(f.lines) != 4 {
		t.Fatalf("server %+v", f)
	}

	// Server lost state → 409 resync → whole file resent from line 1.
	te.srv.on("/api/ingest/live", reply(409, map[string]string{"error": "resync"}))
	writeGameLog(t, te, "<Keep> y\n", true)
	if _, err := te.eng.pollOnce(ctx, tl, &ls); err != nil {
		t.Fatal(err)
	}
	if te.kinds(EvLiveResync) != 1 {
		t.Fatal("no resync event")
	}
	if _, err := te.eng.pollOnce(ctx, tl, &ls); err != nil {
		t.Fatal(err)
	}
	// Resent from line 1, split into chunkMax(3)-line batches: 1-7 and 8-9.
	resent := te.srv.live[len(te.srv.live)-2:]
	if resent[0].FromLine != 1 || resent[0].ToLine != 7 || resent[1].FromLine != 8 || resent[1].ToLine != 9 {
		t.Fatalf("resend %+v", resent)
	}
	if f := te.srv.file(sess); f.lastLine != 9 || len(f.lines) != 5 {
		t.Fatalf("server after resync %+v", f)
	}

	// Session finalised by a backup upload → 409 complete → stop sending.
	te.srv.mu.Lock()
	te.srv.files[sess].complete = true
	te.srv.mu.Unlock()
	writeGameLog(t, te, "<Keep> z\n", true)
	_, _ = te.eng.pollOnce(ctx, tl, &ls)
	n := len(te.srv.live)
	writeGameLog(t, te, "<Keep> zz\n", true)
	_, _ = te.eng.pollOnce(ctx, tl, &ls)
	if len(te.srv.live) != n || !ls.complete || te.kinds(EvLiveComplete) != 1 {
		t.Fatalf("kept sending after complete (%d → %d)", n, len(te.srv.live))
	}
}

func TestLiveWaitsForSessionLine(t *testing.T) {
	te := newEnv(t)
	ctx := context.Background()
	tl := tail.New(te.eng.GameLog(), te.eng.Filter().Session)
	var ls liveSession
	ls.reset()
	writeGameLog(t, te, "Log started\n<Keep> before session\n", false)
	_, _ = te.eng.pollOnce(ctx, tl, &ls)
	if len(te.srv.live) != 0 {
		t.Fatal("sent without a session")
	}
	writeGameLog(t, te, "@session: '"+sessionID(3)+"'\n", true)
	_, _ = te.eng.pollOnce(ctx, tl, &ls)
	if len(te.srv.live) != 1 || te.srv.live[0].FromLine != 1 || len(te.srv.live[0].Lines) != 2 {
		t.Fatalf("live %+v", te.srv.live)
	}
}

func TestLiveRotationToBackup(t *testing.T) {
	te := newEnv(t)
	sess1, sess2 := sessionID(1), sessionID(2)
	content1 := logText(sess1, 3)
	writeGameLog(t, te, content1, false)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- te.eng.Live(ctx) }()

	waitFor(t, func() bool { f := te.srv.file(sess1); return f != nil && len(f.lines) == 4 })

	// Game closes: Game.log is moved to logbackups, a new session starts.
	if err := os.Rename(te.eng.GameLog(), filepath.Join(te.folder, "logbackups", "Game Build(1) 01.log")); err != nil {
		t.Fatal(err)
	}
	writeGameLog(t, te, logText(sess2, 1), false)

	sha1, _ := expectedSha(t, te, content1)
	waitFor(t, func() bool { f := te.srv.file(sess1); return f != nil && f.complete && f.sha == sha1 })
	waitFor(t, func() bool { f := te.srv.file(sess2); return f != nil && len(f.lines) == 2 })
	cancel()
	if err := <-done; err != context.DeadlineExceeded && err != context.Canceled {
		t.Fatalf("live returned %v", err)
	}
}

func TestLiveResetFromHeartbeat(t *testing.T) {
	te := newEnv(t)
	te.eng.HeartbeatInterval = 30 * time.Millisecond
	sess := sessionID(5)
	writeGameLog(t, te, logText(sess, 1), false)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- te.eng.Live(ctx) }()
	waitFor(t, func() bool { return te.srv.file(sess) != nil })

	te.srv.mu.Lock()
	te.srv.epoch = 3
	te.srv.files = map[string]*srvFile{}
	te.srv.mu.Unlock()
	waitFor(t, func() bool { f := te.srv.file(sess); return f != nil && len(f.lines) == 2 })
	cancel()
	<-done
	if e := te.eng.Epoch(); e == nil || *e != 3 {
		t.Fatalf("epoch %v", e)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func TestQueuedCommitAndBatchID(t *testing.T) {
	te := newEnv(t)
	te.eng.Channel = "PTU"
	te.backup(t, "a.log", logText(sessionID(1), 2))
	te.backup(t, "b.log", logText(sessionID(2), 2))
	var batches []string
	te.srv.inject["/api/ingest/begin"] = []func(http.ResponseWriter, map[string]any) bool{
		func(_ http.ResponseWriter, b map[string]any) bool {
			batches = append(batches, b["batchId"].(string))
			return false
		},
		func(_ http.ResponseWriter, b map[string]any) bool {
			batches = append(batches, b["batchId"].(string))
			return false
		},
	}
	te.srv.inject["/api/ingest/commit"] = []func(http.ResponseWriter, map[string]any) bool{
		func(w http.ResponseWriter, _ map[string]any) bool {
			writeJSON(w, 200, map[string]any{"ok": true, "queued": true, "held": true, "jobId": 7, "events": 0})
			return true
		},
	}
	if _, err := te.eng.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(batches) != 2 || batches[0] == "" || batches[0] != batches[1] {
		t.Fatalf("one batch id per pass, got %v", batches)
	}
	te.mu.Lock()
	defer te.mu.Unlock()
	held := 0
	for _, e := range te.events {
		if e.Kind == EvUploadDone && e.Held && e.Channel == "PTU" {
			held++
			if !strings.HasPrefix(e.String(), "[PTU] uploaded") {
				t.Fatalf("string %q", e.String())
			}
		}
	}
	if held != 1 {
		t.Fatalf("held uploads %d, events %+v", held, te.events)
	}
}

func TestWeeklyLimitPausesBackups(t *testing.T) {
	te := newEnv(t)
	te.backup(t, "a.log", logText(sessionID(1), 2))
	te.backup(t, "b.log", logText(sessionID(2), 2))
	reset := time.Now().Add(48 * time.Hour).Truncate(time.Millisecond)
	begins := 0
	limit := func(w http.ResponseWriter, _ map[string]any) bool {
		begins++
		w.Header().Set("Retry-After", "172800")
		writeJSON(w, 429, map[string]any{"error": "weekly_limit", "kind": "files", "resetAt": reset.UnixMilli()})
		return true
	}
	te.srv.inject["/api/ingest/begin"] = []func(http.ResponseWriter, map[string]any) bool{limit, limit, limit}

	stats, err := te.eng.Pass(context.Background())
	if err != nil {
		t.Fatalf("a weekly limit must not fail the pass: %v", err)
	}
	if begins != 1 {
		t.Fatalf("stops at the first refused file without retrying, begins=%d", begins)
	}
	if stats.Uploaded != 0 || stats.Pending == 0 {
		t.Fatalf("stats %+v", stats)
	}
	if te.kinds(EvWeeklyLimit) != 1 {
		t.Fatalf("one EvWeeklyLimit event, got %+v", te.events)
	}
	te.mu.Lock()
	for _, e := range te.events {
		if e.Kind == EvWeeklyLimit && !e.Until.Equal(reset) {
			t.Fatalf("until %v, want %v", e.Until, reset)
		}
	}
	te.mu.Unlock()

	// later passes before the reset don't contact the server for backups
	if _, err := te.eng.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if begins != 1 {
		t.Fatalf("paused pass still called begin (%d)", begins)
	}

	// after the reset the files are uploaded normally
	te.eng.pausedUntil = time.Now().Add(-time.Second)
	te.srv.inject["/api/ingest/begin"] = nil
	stats, err = te.eng.Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Uploaded != 2 {
		t.Fatalf("after reset: %+v", stats)
	}
}

func TestRetryAfterIsHonoured(t *testing.T) {
	te := newEnv(t)
	te.backup(t, "a.log", logText(sessionID(1), 2))
	var waits []time.Duration
	te.eng.API.MaxRetries = 2
	te.eng.API.Backoff = func(int) time.Duration { return time.Millisecond }
	te.eng.API.OnRetry = func(_ string, _ int, err error) {
		if ae, ok := api.AsError(err); ok {
			waits = append(waits, ae.RetryAfter)
		}
	}
	te.srv.inject["/api/ingest/check"] = []func(http.ResponseWriter, map[string]any) bool{
		func(w http.ResponseWriter, _ map[string]any) bool {
			w.Header().Set("Retry-After", "1")
			writeJSON(w, 429, map[string]any{"error": "rate_limited", "retryAfter": 1})
			return true
		},
	}
	start := time.Now()
	stats, err := te.eng.Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Uploaded != 1 || len(waits) != 1 || waits[0] != time.Second || time.Since(start) < time.Second {
		t.Fatalf("stats %+v waits %v elapsed %v", stats, waits, time.Since(start))
	}
}
