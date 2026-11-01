package uploader

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/cihanenescolak/ScLogAgent/internal/api"
	"github.com/cihanenescolak/ScLogAgent/internal/prefilter"
)

// fakeServer simulates the server's ingest protocol (in memory).
type fakeServer struct {
	t     *testing.T
	mu    sync.Mutex
	epoch int
	chunk int
	files map[string]*srvFile
	ups   map[string]*srvUpload
	nextU int
	// inject: path → queue of handlers run instead of the normal logic
	// (return false to fall through to the normal logic).
	inject map[string][]func(w http.ResponseWriter, body map[string]any) bool
	calls  map[string]int
	live   []api.LiveRequest
	srv    *httptest.Server
}

type srvFile struct {
	sha      string
	complete bool
	lines    []api.Line
	lastLine int
}

type srvUpload struct {
	session, sha, filename string
	count, nextSeq         int
	lines                  []api.Line
}

const testToken = "tok"

func newFakeServer(t *testing.T) *fakeServer {
	s := &fakeServer{
		t: t, epoch: 1, chunk: 3,
		files:  map[string]*srvFile{},
		ups:    map[string]*srvUpload{},
		inject: map[string][]func(http.ResponseWriter, map[string]any) bool{},
		calls:  map[string]int{},
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *fakeServer) on(path string, fn func(w http.ResponseWriter, body map[string]any) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inject[path] = append(s.inject[path], fn)
}

func (s *fakeServer) count(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[path]
}

func (s *fakeServer) file(session string) *srvFile {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.files[session]
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func reply(status int, v any) func(http.ResponseWriter, map[string]any) bool {
	return func(w http.ResponseWriter, _ map[string]any) bool {
		writeJSON(w, status, v)
		return true
	}
}

func (s *fakeServer) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := r.URL.Path
	s.calls[path]++
	if path != "/api/agent/config" && r.Header.Get("Authorization") != "Bearer "+testToken {
		writeJSON(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	if q := s.inject[path]; len(q) > 0 {
		fn := q[0]
		s.inject[path] = q[1:]
		s.mu.Unlock()
		handled := fn(w, body)
		s.mu.Lock()
		if handled {
			return
		}
	}
	if ep, ok := body["epoch"].(float64); ok && int(ep) != s.epoch {
		writeJSON(w, 409, map[string]any{"error": "reset", "epoch": s.epoch})
		return
	}
	switch path {
	case "/api/agent/config":
		ep := s.epoch
		writeJSON(w, 200, api.AgentConfig{
			ProtocolVersion: 1, IncludePatterns: []string{`<Keep>`}, HeaderPatterns: []string{`@session`},
			HeaderMaxLines: 5, SessionPattern: prefilter.DefaultSessionPattern, ChunkMaxLines: s.chunk, DataEpoch: &ep,
		})
	case "/api/agent/me":
		writeJSON(w, 200, map[string]any{"user": map[string]any{"name": "Tester", "email": nil}, "dataEpoch": s.epoch})
	case "/api/agent/heartbeat":
		writeJSON(w, 200, map[string]any{"ok": true, "dataEpoch": s.epoch})
	case "/api/ingest/check":
		var req api.CheckRequest
		_ = json.Unmarshal(raw, &req)
		resp := api.CheckResponse{Epoch: s.epoch}
		for _, f := range req.Files {
			st := api.StateMissing
			if sf := s.files[f.SessionUUID]; sf != nil {
				switch {
				case sf.complete && sf.sha == f.Sha256:
					st = api.StateComplete
				case !sf.complete:
					st = api.StateLivePartial
				}
			}
			resp.Results = append(resp.Results, api.CheckResult{SessionUUID: f.SessionUUID, State: st})
		}
		writeJSON(w, 200, resp)
	case "/api/ingest/begin":
		var req api.BeginRequest
		_ = json.Unmarshal(raw, &req)
		if sf := s.files[req.SessionUUID]; sf != nil && sf.complete && sf.sha == req.Sha256 {
			writeJSON(w, 200, map[string]any{"alreadyComplete": true})
			return
		}
		s.nextU++
		id := fmt.Sprintf("u%d", s.nextU)
		s.ups[id] = &srvUpload{session: req.SessionUUID, sha: req.Sha256, count: req.LineCount, filename: req.Filename}
		writeJSON(w, 200, map[string]any{"uploadId": id, "chunkMaxLines": s.chunk})
	case "/api/ingest/chunk":
		var req api.ChunkRequest
		_ = json.Unmarshal(raw, &req)
		up := s.ups[req.UploadID]
		if up == nil {
			writeJSON(w, 410, map[string]string{"error": "expired"})
			return
		}
		if req.Seq != up.nextSeq {
			writeJSON(w, 409, map[string]any{"error": "bad_seq", "expectedSeq": up.nextSeq})
			return
		}
		if len(req.Lines) > s.chunk {
			writeJSON(w, 400, map[string]string{"error": "invalid_request"})
			return
		}
		up.lines = append(up.lines, req.Lines...)
		up.nextSeq++
		writeJSON(w, 200, map[string]any{"ok": true, "nextSeq": up.nextSeq})
	case "/api/ingest/commit":
		id, _ := body["uploadId"].(string)
		up := s.ups[id]
		if up == nil {
			writeJSON(w, 410, map[string]string{"error": "expired"})
			return
		}
		delete(s.ups, id)
		if len(up.lines) != up.count || prefilter.HashLines(up.lines) != up.sha {
			writeJSON(w, 422, map[string]string{"error": "verify_failed", "detail": "hash"})
			return
		}
		s.files[up.session] = &srvFile{sha: up.sha, complete: true, lines: up.lines}
		writeJSON(w, 200, map[string]any{"ok": true, "logFileId": 1, "events": len(up.lines), "counts": map[string]int{}})
	case "/api/ingest/abort":
		id, _ := body["uploadId"].(string)
		delete(s.ups, id)
		writeJSON(w, 200, map[string]any{"ok": true})
	case "/api/ingest/live":
		var req api.LiveRequest
		_ = json.Unmarshal(raw, &req)
		s.live = append(s.live, req)
		sf := s.files[req.SessionUUID]
		if sf != nil && sf.complete {
			writeJSON(w, 409, map[string]string{"error": "complete"})
			return
		}
		if req.FromLine == 1 {
			sf = &srvFile{}
			s.files[req.SessionUUID] = sf
		} else if sf == nil || sf.lastLine+1 != req.FromLine {
			delete(s.files, req.SessionUUID)
			writeJSON(w, 409, map[string]any{"error": "resync", "lastLine": 0})
			return
		}
		sf.lines = append(sf.lines, req.Lines...)
		sf.lastLine = req.ToLine
		writeJSON(w, 200, map[string]any{"ok": true, "lastLine": req.ToLine})
	default:
		writeJSON(w, 404, map[string]string{"error": "not_found"})
	}
}
