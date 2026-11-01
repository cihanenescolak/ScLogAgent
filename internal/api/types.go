// Package api is the wire protocol between the agent and an SC Log Viewer
// server (the server's ingest protocol, version ProtocolVersion).
package api

const ProtocolVersion = 1

// DefaultChunkMaxLines is used when the server does not say otherwise.
const DefaultChunkMaxLines = 2000

type AgentConfig struct {
	ProtocolVersion    int      `json:"protocolVersion"`
	MinAgentVersion    string   `json:"minAgentVersion"`
	LatestAgentVersion string   `json:"latestAgentVersion"`
	IncludePatterns    []string `json:"includePatterns"`
	HeaderPatterns     []string `json:"headerPatterns"`
	HeaderMaxLines     int      `json:"headerMaxLines"`
	SessionPattern     string   `json:"sessionPattern"`
	ChunkMaxLines      int      `json:"chunkMaxLines"`
	DataEpoch          *int     `json:"dataEpoch,omitempty"`
	// AgentUpdate describes the sclogsync.exe this server ships (nil when it has none).
	AgentUpdate *AgentUpdate `json:"agentUpdate,omitempty"`
}

// AgentUpdate is the client build offered for self-update.
type AgentUpdate struct {
	Version string `json:"version"`
	URL     string `json:"url"` // path on the server, e.g. /download/sclogsync.exe
	Sha256  string `json:"sha256"`
	Size    int64  `json:"size"`
}

type FileState string

const (
	StateComplete    FileState = "complete"
	StateMissing     FileState = "missing"
	StateInProgress  FileState = "in_progress"
	StateLivePartial FileState = "live_partial"
)

// Line is a FilteredLine: [original 1-based line number, text]. It marshals
// as a two-element JSON array.
type Line struct {
	No   int
	Text string
}

type CheckFile struct {
	SessionUUID string `json:"sessionUuid"`
	Sha256      string `json:"sha256"`
}

type CheckRequest struct {
	Epoch *int        `json:"epoch,omitempty"`
	Files []CheckFile `json:"files"`
}

type CheckResult struct {
	SessionUUID string    `json:"sessionUuid"`
	State       FileState `json:"state"`
}

type CheckResponse struct {
	Epoch   int           `json:"epoch"`
	Results []CheckResult `json:"results"`
}

type BeginRequest struct {
	Epoch       *int   `json:"epoch,omitempty"`
	SessionUUID string `json:"sessionUuid"`
	Sha256      string `json:"sha256"`
	LineCount   int    `json:"lineCount"`
	Filename    string `json:"filename,omitempty"`
	Source      string `json:"source"`
	// BatchID groups the files of one pass in the server's processing log.
	BatchID string `json:"batchId,omitempty"`
}

type BeginResponse struct {
	UploadID        string `json:"uploadId"`
	ChunkMaxLines   int    `json:"chunkMaxLines"`
	AlreadyComplete bool   `json:"alreadyComplete"`
}

type ChunkRequest struct {
	UploadID string `json:"uploadId"`
	Seq      int    `json:"seq"`
	Lines    []Line `json:"lines"`
}

type ChunkResponse struct {
	OK      bool `json:"ok"`
	NextSeq int  `json:"nextSeq"`
}

type CommitResponse struct {
	OK        bool           `json:"ok"`
	LogFileID int64          `json:"logFileId"`
	Events    int            `json:"events"`
	Counts    map[string]int `json:"counts"`
	// Newer servers process files in a queue: Queued=true, Held=true when the
	// game version is newer than the site supports (processed after an update).
	Queued   bool `json:"queued"`
	Held     bool `json:"held"`
	Position *int `json:"position,omitempty"`
}

type LiveRequest struct {
	Epoch       *int   `json:"epoch,omitempty"`
	SessionUUID string `json:"sessionUuid"`
	Filename    string `json:"filename,omitempty"`
	FromLine    int    `json:"fromLine"`
	ToLine      int    `json:"toLine"`
	Lines       []Line `json:"lines"`
}

type LiveResponse struct {
	OK       bool `json:"ok"`
	LastLine int  `json:"lastLine"`
	// Held: accepted but not stored (game version newer than the site supports).
	Held bool `json:"held"`
}

type DeviceStartRequest struct {
	DeviceName   string `json:"deviceName"`
	AgentVersion string `json:"agentVersion"`
}

type DeviceStartResponse struct {
	DeviceCode              string `json:"deviceCode"`
	UserCode                string `json:"userCode"`
	VerificationURI         string `json:"verificationUri"`
	VerificationURIComplete string `json:"verificationUriComplete"`
	Interval                int    `json:"interval"`
	ExpiresIn               int    `json:"expiresIn"`
}

type User struct {
	Name  *string `json:"name"`
	Email *string `json:"email"`
}

// Display returns the best human label for the user.
func (u User) Display() string {
	switch {
	case u.Name != nil && *u.Name != "" && u.Email != nil && *u.Email != "":
		return *u.Name + " <" + *u.Email + ">"
	case u.Email != nil && *u.Email != "":
		return *u.Email
	case u.Name != nil && *u.Name != "":
		return *u.Name
	}
	return "?"
}

type TokenResponse struct {
	Token     string `json:"token"`
	User      User   `json:"user"`
	DataEpoch int    `json:"dataEpoch"`
}

type MeResponse struct {
	User      User `json:"user"`
	DataEpoch int  `json:"dataEpoch"`
}

type HeartbeatRequest struct {
	AgentVersion string `json:"agentVersion"`
	Mode         string `json:"mode"`
	Folder       string `json:"folder"`
}

type HeartbeatResponse struct {
	OK        bool `json:"ok"`
	DataEpoch int  `json:"dataEpoch"`
}
