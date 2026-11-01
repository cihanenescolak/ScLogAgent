package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cihanenescolak/ScLogAgent/internal/state"
	"github.com/cihanenescolak/ScLogAgent/internal/ui"
)

func TestInteractiveOnceEndToEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SCLOGSYNC_HOME", home)
	openBrowser = func(string) error { return nil }

	folder := filepath.Join(t.TempDir(), "LIVE")
	if err := os.MkdirAll(filepath.Join(folder, "logbackups"), 0o755); err != nil {
		t.Fatal(err)
	}
	log := "Log started\n<x> @session: '12345678-1234-1234-1234-123456789abc'\nnoise\n<Keep> hi\n"
	bp := filepath.Join(folder, "logbackups", "Game Build(1).log")
	if err := os.WriteFile(bp, []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(bp, old, old)

	var mu sync.Mutex
	polls, committed := 0, false
	var gotLines [][2]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		send := func(code int, v any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(v)
		}
		authed := r.Header.Get("Authorization") == "Bearer T1"
		switch r.URL.Path {
		case "/api/agent/config":
			cfg := map[string]any{"protocolVersion": 1, "includePatterns": []string{"<Keep>"}, "headerPatterns": []string{"@session"},
				"headerMaxLines": 10, "sessionPattern": `@session:\s+'([0-9a-fA-F-]{36})'`, "chunkMaxLines": 2000, "latestAgentVersion": "1.0.0"}
			if authed {
				cfg["dataEpoch"] = 4
			}
			send(200, cfg)
			return
		case "/api/agent/device":
			if body["deviceName"] == "" {
				send(400, map[string]string{"error": "invalid_request"})
				return
			}
			send(200, map[string]any{"deviceCode": "DC", "userCode": "ABCD-EFGH", "verificationUri": "http://x/device",
				"verificationUriComplete": "http://x/device?code=ABCD-EFGH", "interval": 1, "expiresIn": 60})
			return
		case "/api/agent/token":
			polls++
			if polls < 2 {
				send(428, map[string]string{"error": "authorization_pending"})
				return
			}
			send(200, map[string]any{"token": "T1", "user": map[string]any{"name": "Pilot", "email": "p@example.com"}, "dataEpoch": 4})
			return
		}
		if !authed {
			send(401, map[string]string{"error": "unauthorized"})
			return
		}
		switch r.URL.Path {
		case "/api/ingest/check":
			files := body["files"].([]any)
			f := files[0].(map[string]any)
			send(200, map[string]any{"epoch": 4, "results": []any{map[string]any{"sessionUuid": f["sessionUuid"], "state": "missing"}}})
		case "/api/ingest/begin":
			send(200, map[string]any{"uploadId": "U", "chunkMaxLines": 2000})
		case "/api/ingest/chunk":
			for _, l := range body["lines"].([]any) {
				p := l.([]any)
				gotLines = append(gotLines, [2]any{p[0], p[1]})
			}
			send(200, map[string]any{"ok": true, "nextSeq": 1})
		case "/api/ingest/commit":
			committed = true
			send(200, map[string]any{"ok": true, "logFileId": 1, "events": 1, "counts": map[string]int{}})
		default:
			send(404, map[string]string{"error": "not_found"})
		}
	}))
	defer srv.Close()

	var out bytes.Buffer
	u := ui.NewWith("en", &out, strings.NewReader(""))
	o := &options{server: srv.URL, folder: folder, once: true}
	if code := cmdInteractive(context.Background(), u, o); code != exitOK {
		t.Fatalf("exit %d\n%s", code, out.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if !committed || len(gotLines) != 2 || gotLines[1][1] != "<Keep> hi" {
		t.Fatalf("upload: committed=%v lines=%v\n%s", committed, gotLines, out.String())
	}
	for _, want := range []string{"ABCD-EFGH", "Signed in as Pilot <p@example.com>", "uploaded Game Build(1).log"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	cfg, err := state.Open(home).LoadConfig()
	if err != nil || cfg.Server != srv.URL || cfg.Folder != folder || cfg.Token == "" || cfg.Token == "T1" {
		t.Fatalf("config %+v %v", cfg, err)
	}
}
