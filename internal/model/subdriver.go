package model

import (
	"encoding/json"
	"strings"
	"time"
)

type Subdriver struct {
	Endpoint   *TerminalEndpoint `json:"endpoint,omitempty"`
	ID         string            `json:"id"`
	RepoID     string            `json:"repo_id,omitempty"`
	Context    string            `json:"context"`
	Generation int               `json:"generation"`
	State      string            `json:"state"`
	SessionID  string            `json:"session_id,omitempty"`
	RunnerPID  int               `json:"runner_pid,omitempty"`
	HarnessPID int               `json:"harness_pid,omitempty"`
	Harness    string            `json:"harness,omitempty"`
	Model      string            `json:"model,omitempty"`
	Runtime    string            `json:"runtime,omitempty"`
	Checkpoint string            `json:"checkpoint,omitempty"`
	Failure    string            `json:"failure,omitempty"`
	UpdatedAt  time.Time         `json:"updated_at"`
}

func (c Subdriver) DriverID() string { return "coordinator:" + c.ID }

func IsSubdriverOwner(id string) bool {
	id = strings.TrimSpace(id)
	return strings.HasPrefix(id, "coordinator:") || strings.HasPrefix(id, "subdriver:")
}

type SubdriverRequest struct {
	ReadCommand    string    `json:"read_command,omitempty"`
	OriginDriverID string    `json:"origin_driver_id"`
	ID             string    `json:"id"`
	SubdriverID    string    `json:"subdriver_id"`
	DriverID       string    `json:"driver_id"`
	Key            string    `json:"key"`
	Original       string    `json:"original"`
	Context        string    `json:"context"`
	LeadRequestID  string    `json:"lead_request_id,omitempty"`
	State          string    `json:"state"`
	CreatedAt      time.Time `json:"created_at"`
}

type SubdriverEvent struct {
	ReadCommand string    `json:"read_command,omitempty"`
	ID          int64     `json:"id"`
	RequestID   string    `json:"request_id"`
	Key         string    `json:"key"`
	Kind        string    `json:"kind"`
	Payload     string    `json:"payload"`
	ReplyTo     int64     `json:"reply_to,omitempty"`
	Handled     bool      `json:"handled"`
	CreatedAt   time.Time `json:"created_at"`
}

type SubdriverWorker struct {
	RequestID string `json:"request_id"`
	Key       string `json:"key"`
	TaskID    string `json:"task_id"`
}

type SubdriverPage struct {
	Subdriver Subdriver          `json:"subdriver"`
	Requests  []SubdriverRequest `json:"requests"`
	Events    []SubdriverEvent   `json:"events"`
	Workers   []SubdriverWorker  `json:"workers"`
	Offset    int                `json:"offset"`
	Next      string             `json:"next,omitempty"`
}

func (r SubdriverRequest) MarshalJSON() ([]byte, error) {
	type plain SubdriverRequest
	return json.Marshal(struct {
		plain
		LegacyID string `json:"coordinator_id"`
	}{plain(r), r.SubdriverID})
}

func (p SubdriverPage) MarshalJSON() ([]byte, error) {
	type plain SubdriverPage
	return json.Marshal(struct {
		plain
		Legacy Subdriver `json:"coordinator"`
	}{plain(p), p.Subdriver})
}

func (n DriverNotification) MarshalJSON() ([]byte, error) {
	type plain DriverNotification
	return json.Marshal(struct {
		plain
		LegacyID       string `json:"coordinator_id,omitempty"`
		LegacyEventID  int64  `json:"coordinator_event_id,omitempty"`
		LegacyRepoName string `json:"coordinator_repo_name,omitempty"`
	}{plain(n), n.SubdriverID, n.SubdriverEventID, n.SubdriverRepoName})
}

type SubdriverFence struct {
	ID         string
	Generation int
	Token      string
}
