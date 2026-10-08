package claudebridge

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	SchemaVersion     = 1
	MaxHookInputBytes = 1024 * 1024
	MaxFrameBytes     = 96 * 1024
	maxRequestBytes   = MaxHookInputBytes + 4096
)

const (
	SocketEnvironment        = "SHEPHRD_CLAUDE_BRIDGE_SOCKET"
	TokenEnvironment         = "SHEPHRD_CLAUDE_BRIDGE_TOKEN"
	AttemptEnvironment       = "SHEPHRD_CLAUDE_BRIDGE_ATTEMPT_ID"
	RunGenerationEnvironment = "SHEPHRD_CLAUDE_BRIDGE_RUN_GENERATION"
)

type request struct {
	SchemaVersion int             `json:"schema_version"`
	Token         string          `json:"token"`
	AttemptID     string          `json:"attempt_id"`
	RunGeneration int             `json:"run_generation"`
	Input         json.RawMessage `json:"input,omitempty"`
	Invalid       string          `json:"invalid,omitempty"`
}

type response struct {
	Terminate  bool   `json:"terminate"`
	StopReason string `json:"stop_reason,omitempty"`
	Decision   string `json:"decision,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

type Channel struct {
	SocketPath    string
	Token         string
	AttemptID     string
	RunGeneration int
	dir           string
	listener      *net.UnixListener
}

type Exchange struct {
	Input   json.RawMessage
	Invalid string
	conn    net.Conn
}

func Open(attemptID string, generation int) (*Channel, error) {
	if attemptID == "" || generation < 1 {
		return nil, fmt.Errorf("Claude bridge requires an attempt ID and positive run generation")
	}
	dir, err := os.MkdirTemp("", "shephrd-claude-bridge-")
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	path := filepath.Join(dir, "events.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		listener.Close()
		os.RemoveAll(dir)
		return nil, err
	}
	return &Channel{SocketPath: path, Token: hex.EncodeToString(tokenBytes), AttemptID: attemptID, RunGeneration: generation, dir: dir, listener: listener}, nil
}

func (c *Channel) Environment() map[string]string {
	return map[string]string{
		SocketEnvironment:        c.SocketPath,
		TokenEnvironment:         c.Token,
		AttemptEnvironment:       c.AttemptID,
		RunGenerationEnvironment: strconv.Itoa(c.RunGeneration),
	}
}

func (c *Channel) Accept() (*Exchange, error) {
	conn, err := c.listener.Accept()
	if err != nil {
		return nil, err
	}
	if err := conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		conn.Close()
		return nil, err
	}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4096), maxRequestBytes)
	if !scanner.Scan() {
		err := scanner.Err()
		conn.Close()
		if err != nil {
			return nil, fmt.Errorf("read Claude bridge request: %w", err)
		}
		return nil, fmt.Errorf("Claude bridge request is empty")
	}
	line := append([]byte(nil), scanner.Bytes()...)
	if len(line) >= maxRequestBytes {
		conn.Close()
		return nil, fmt.Errorf("Claude bridge request exceeds %d bytes", maxRequestBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	var request request
	if err := decoder.Decode(&request); err != nil {
		conn.Close()
		return nil, fmt.Errorf("invalid Claude bridge request: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		conn.Close()
		return nil, fmt.Errorf("invalid Claude bridge request: trailing JSON")
	}
	if request.SchemaVersion != SchemaVersion {
		conn.Close()
		return nil, fmt.Errorf("Claude bridge schema version is invalid")
	}
	if subtle.ConstantTimeCompare([]byte(request.Token), []byte(c.Token)) != 1 {
		conn.Close()
		return nil, fmt.Errorf("Claude bridge token is invalid")
	}
	if request.AttemptID != c.AttemptID || request.RunGeneration != c.RunGeneration {
		conn.Close()
		return nil, fmt.Errorf("Claude bridge run identity is stale")
	}
	if (len(request.Input) == 0) == (request.Invalid == "") {
		conn.Close()
		return nil, fmt.Errorf("Claude bridge request payload is invalid")
	}
	return &Exchange{Input: request.Input, Invalid: request.Invalid, conn: conn}, nil
}

func (e *Exchange) Respond(terminate bool, stopReason string) error {
	defer e.conn.Close()
	return json.NewEncoder(e.conn).Encode(response{Terminate: terminate, StopReason: stopReason})
}

func (e *Exchange) RespondBlock(reason string) error {
	defer e.conn.Close()
	return json.NewEncoder(e.conn).Encode(response{Decision: "block", Reason: reason})
}

func (c *Channel) Close() error {
	var first error
	if c.listener != nil {
		first = c.listener.Close()
	}
	if err := os.RemoveAll(c.dir); first == nil {
		first = err
	}
	return first
}
