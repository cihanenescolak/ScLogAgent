package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// MarshalJSON encodes a Line as [no, "text"].
func (l Line) MarshalJSON() ([]byte, error) {
	t, err := json.Marshal(l.Text)
	if err != nil {
		return nil, err
	}
	b := make([]byte, 0, len(t)+16)
	b = append(b, '[')
	b = strconv.AppendInt(b, int64(l.No), 10)
	b = append(b, ',')
	b = append(b, t...)
	b = append(b, ']')
	return b, nil
}

func (l *Line) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if len(raw) != 2 {
		return fmt.Errorf("line: expected 2 elements, got %d", len(raw))
	}
	if err := json.Unmarshal(raw[0], &l.No); err != nil {
		return err
	}
	return json.Unmarshal(raw[1], &l.Text)
}

// Error is a non-2xx answer from the server. Code is the `error` field of the
// JSON body ("reset", "in_progress", "resync", ...).
type Error struct {
	Status      int
	Code        string
	Detail      string
	Epoch       *int
	ExpectedSeq *int
	Path        string
	// RetryAfter is the server's Retry-After (429 answers); 0 when absent.
	RetryAfter time.Duration
	// ResetAt is when a weekly upload limit resets (429 {error:"weekly_limit"}); zero otherwise.
	ResetAt time.Time
}

func (e *Error) Error() string {
	s := fmt.Sprintf("%s: HTTP %d", e.Path, e.Status)
	if e.Code != "" {
		s += " " + e.Code
	}
	if e.Detail != "" {
		s += " (" + e.Detail + ")"
	}
	return s
}

// Is reports whether err is an *Error with the given status and code ("" = any code).
func Is(err error, status int, code string) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	return e.Status == status && (code == "" || e.Code == code)
}

// AsError unwraps err into *Error.
func AsError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

// IsUnauthorized reports a 401 (token missing/revoked).
func IsUnauthorized(err error) bool { return Is(err, http.StatusUnauthorized, "") }

// IsReset reports the 409 {error:"reset"} answer (user's data epoch changed).
func IsReset(err error) bool { return Is(err, http.StatusConflict, "reset") }

// IsWeeklyLimit reports the 429 {error:"weekly_limit"} answer: the user's weekly
// upload limit is used up until ResetAt (not worth retrying before then).
func IsWeeklyLimit(err error) bool { return Is(err, http.StatusTooManyRequests, "weekly_limit") }

// IsTemporary reports errors worth retrying later (network, 5xx, 429 rate limits).
func IsTemporary(err error) bool {
	if err == nil {
		return false
	}
	if e, ok := AsError(err); ok {
		if e.Code == "weekly_limit" {
			return false
		}
		return e.Status >= 500 || e.Status == http.StatusTooManyRequests || e.Status == http.StatusRequestTimeout
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return true
	}
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)
}

// Client talks to the SC Log Viewer server.
type Client struct {
	BaseURL   string
	Token     string
	UserAgent string
	HTTP      *http.Client
	// Retries for temporary errors (network, 5xx, 429). 0 = no retry.
	MaxRetries int
	// Backoff before retry n (0-based). Defaults to exponential 1s..30s.
	Backoff func(n int) time.Duration
	// OnRetry is called before sleeping for a retry (for logging).
	OnRetry func(path string, attempt int, err error)
}

// New creates a client with sane defaults.
func New(baseURL, token, version string) *Client {
	return &Client{
		BaseURL:    NormalizeBaseURL(baseURL),
		Token:      token,
		UserAgent:  fmt.Sprintf("SCLogSync/%s (%s; %s)", version, runtime.GOOS, runtime.GOARCH),
		HTTP:       &http.Client{Timeout: 120 * time.Second},
		MaxRetries: 4,
	}
}

// NormalizeBaseURL trims spaces/trailing slashes and adds https:// when no scheme is given.
func NormalizeBaseURL(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, "/")
	if s == "" {
		return s
	}
	if !strings.Contains(s, "://") {
		if strings.HasPrefix(s, "localhost") || strings.HasPrefix(s, "127.0.0.1") {
			s = "http://" + s
		} else {
			s = "https://" + s
		}
	}
	return s
}

// ValidateBaseURL checks that s is an absolute http(s) URL.
func ValidateBaseURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("invalid server URL %q", s)
	}
	return nil
}

// maxRetryAfter caps how long a single request waits for a server-requested Retry-After.
const maxRetryAfter = 2 * time.Minute

func defaultBackoff(n int) time.Duration {
	d := time.Second << uint(min(n, 5))
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	backoff := c.Backoff
	if backoff == nil {
		backoff = defaultBackoff
	}
	for attempt := 0; ; attempt++ {
		err := c.once(ctx, method, path, body, out)
		if err == nil || !IsTemporary(err) || attempt >= c.MaxRetries || ctx.Err() != nil {
			return err
		}
		if c.OnRetry != nil {
			c.OnRetry(path, attempt+1, err)
		}
		wait := backoff(attempt)
		if e, ok := AsError(err); ok && e.RetryAfter > wait {
			wait = min(e.RetryAfter, maxRetryAfter)
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (c *Client) once(ctx context.Context, method, path string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		e := &Error{Status: resp.StatusCode, Path: path}
		if s, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && s > 0 {
			e.RetryAfter = time.Duration(s) * time.Second
		}
		var eb struct {
			Error       string `json:"error"`
			Detail      any    `json:"detail"`
			Epoch       *int   `json:"epoch"`
			ExpectedSeq *int   `json:"expectedSeq"`
			ResetAt     int64  `json:"resetAt"`
		}
		if json.Unmarshal(data, &eb) == nil {
			e.Code, e.Epoch, e.ExpectedSeq = eb.Error, eb.Epoch, eb.ExpectedSeq
			if eb.ResetAt > 0 {
				e.ResetAt = time.UnixMilli(eb.ResetAt)
			}
			if eb.Detail != nil {
				e.Detail = fmt.Sprint(eb.Detail)
			}
		} else if len(data) > 0 && len(data) < 300 {
			e.Detail = strings.TrimSpace(string(data))
		}
		return e
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s: bad response: %w", path, err)
	}
	return nil
}

func (c *Client) Config(ctx context.Context) (*AgentConfig, error) {
	var r AgentConfig
	return &r, c.do(ctx, http.MethodGet, "/api/agent/config", nil, &r)
}

func (c *Client) DeviceStart(ctx context.Context, req DeviceStartRequest) (*DeviceStartResponse, error) {
	var r DeviceStartResponse
	return &r, c.do(ctx, http.MethodPost, "/api/agent/device", req, &r)
}

// PollToken performs one poll. Returns (nil, *Error{428 authorization_pending}) while pending.
func (c *Client) PollToken(ctx context.Context, deviceCode string) (*TokenResponse, error) {
	var r TokenResponse
	return &r, c.do(ctx, http.MethodPost, "/api/agent/token", map[string]string{"deviceCode": deviceCode}, &r)
}

func (c *Client) Me(ctx context.Context) (*MeResponse, error) {
	var r MeResponse
	return &r, c.do(ctx, http.MethodGet, "/api/agent/me", nil, &r)
}

func (c *Client) Heartbeat(ctx context.Context, req HeartbeatRequest) (*HeartbeatResponse, error) {
	var r HeartbeatResponse
	return &r, c.do(ctx, http.MethodPost, "/api/agent/heartbeat", req, &r)
}

func (c *Client) Check(ctx context.Context, req CheckRequest) (*CheckResponse, error) {
	var r CheckResponse
	return &r, c.do(ctx, http.MethodPost, "/api/ingest/check", req, &r)
}

func (c *Client) Begin(ctx context.Context, req BeginRequest) (*BeginResponse, error) {
	var r BeginResponse
	return &r, c.do(ctx, http.MethodPost, "/api/ingest/begin", req, &r)
}

func (c *Client) Chunk(ctx context.Context, req ChunkRequest) (*ChunkResponse, error) {
	var r ChunkResponse
	return &r, c.do(ctx, http.MethodPost, "/api/ingest/chunk", req, &r)
}

func (c *Client) Commit(ctx context.Context, uploadID string) (*CommitResponse, error) {
	var r CommitResponse
	return &r, c.do(ctx, http.MethodPost, "/api/ingest/commit", map[string]string{"uploadId": uploadID}, &r)
}

func (c *Client) Abort(ctx context.Context, uploadID string) error {
	return c.do(ctx, http.MethodPost, "/api/ingest/abort", map[string]string{"uploadId": uploadID}, nil)
}

func (c *Client) Live(ctx context.Context, req LiveRequest) (*LiveResponse, error) {
	var r LiveResponse
	return &r, c.do(ctx, http.MethodPost, "/api/ingest/live", req, &r)
}
