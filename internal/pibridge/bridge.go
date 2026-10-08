package pibridge

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"shephrd/internal/adapter"
)

const (
	SchemaVersion             = 2
	MaxFrameBytes             = 96 * 1024
	ResponseTimeout           = 5 * time.Second
	ResultAcceptedNonterminal = "accepted_nonterminal"
	ResultAcceptedTerminal    = "accepted_terminal"
	ResultRepair              = "repair"
	ResultFatal               = "fatal"
)

//go:embed shephrd-herdr-bridge.ts
var extensionSource []byte

type Frame struct {
	SchemaVersion int    `json:"schema_version"`
	Token         string `json:"token"`
	AttemptID     string `json:"attempt_id"`
	RunGeneration int    `json:"run_generation"`
	Sequence      uint64 `json:"seq"`
	Kind          string `json:"kind"`
	SessionID     string `json:"session_id,omitempty"`
	Envelope      string `json:"envelope,omitempty"`
}

type Response struct {
	SchemaVersion int                 `json:"schema_version"`
	Token         string              `json:"token"`
	AttemptID     string              `json:"attempt_id"`
	RunGeneration int                 `json:"run_generation"`
	AckSequence   uint64              `json:"ack_seq"`
	Result        string              `json:"result"`
	RepairID      string              `json:"repair_id,omitempty"`
	Diagnostic    *adapter.Diagnostic `json:"diagnostic,omitempty"`
}

type Channel struct {
	SocketPath    string
	Token         string
	AttemptID     string
	RunGeneration int
	dir           string
	listener      *net.UnixListener
}

type Connection struct {
	channel   *Channel
	conn      net.Conn
	scanner   *bufio.Scanner
	expected  uint64
	last      uint64
	responded bool
}

func WriteExtension(dir, attemptID string, generation int) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("pi-herdr-bridge-%s-run-%d.ts", attemptID, generation))
	if err := os.WriteFile(path, extensionSource, 0o600); err != nil {
		return "", err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func Open(attemptID string, generation int) (*Channel, error) {
	if attemptID == "" || generation < 1 {
		return nil, fmt.Errorf("Pi bridge requires an attempt ID and positive run generation")
	}
	dir, err := os.MkdirTemp("", "shephrd-pi-bridge-")
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

func (c *Channel) Environment() []string {
	return []string{
		"SHEPHRD_BRIDGE_SOCKET=" + c.SocketPath,
		"SHEPHRD_BRIDGE_TOKEN=" + c.Token,
		"SHEPHRD_BRIDGE_ATTEMPT_ID=" + c.AttemptID,
		fmt.Sprintf("SHEPHRD_BRIDGE_RUN_GENERATION=%d", c.RunGeneration),
	}
}

func (c *Channel) Accept(ctx context.Context) (*Connection, error) {
	if deadline, ok := ctx.Deadline(); ok {
		if err := c.listener.SetDeadline(deadline); err != nil {
			return nil, err
		}
	}
	conn, err := c.listener.Accept()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4096), MaxFrameBytes)
	return &Connection{channel: c, conn: conn, scanner: scanner, expected: 1}, nil
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

func (c *Connection) Next() (Frame, error) {
	if !c.scanner.Scan() {
		if err := c.scanner.Err(); err != nil {
			return Frame{}, fmt.Errorf("read Pi bridge frame: %w", err)
		}
		return Frame{}, fmt.Errorf("Pi bridge closed before a terminal envelope")
	}
	line := append([]byte(nil), c.scanner.Bytes()...)
	if c.last != 0 && !c.responded {
		return Frame{}, fmt.Errorf("Pi bridge frame %d has no correlated response", c.last)
	}
	if len(line) >= MaxFrameBytes {
		return Frame{}, fmt.Errorf("Pi bridge frame exceeds %d bytes", MaxFrameBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	var frame Frame
	if err := decoder.Decode(&frame); err != nil {
		return Frame{}, fmt.Errorf("invalid Pi bridge frame: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Frame{}, fmt.Errorf("invalid Pi bridge frame: trailing JSON")
	}
	if err := c.validate(frame); err != nil {
		return Frame{}, err
	}
	c.expected++
	c.last = frame.Sequence
	c.responded = false
	return frame, nil
}

func (c *Connection) Respond(sequence uint64, result, repairID string, diagnostic *adapter.Diagnostic) error {
	if sequence != c.last || c.responded {
		return fmt.Errorf("Pi bridge response sequence is invalid: got %d, want unacknowledged %d", sequence, c.last)
	}
	switch result {
	case ResultAcceptedNonterminal, ResultAcceptedTerminal, ResultRepair, ResultFatal:
	default:
		return fmt.Errorf("Pi bridge response result is invalid")
	}
	if result == ResultRepair {
		if repairID == "" || diagnostic == nil || !diagnostic.Repairable() {
			return fmt.Errorf("Pi bridge repair response is incomplete")
		}
	} else if repairID != "" || diagnostic != nil {
		return fmt.Errorf("Pi bridge non-repair response contains repair fields")
	}
	response := Response{SchemaVersion: SchemaVersion, Token: c.channel.Token, AttemptID: c.channel.AttemptID,
		RunGeneration: c.channel.RunGeneration, AckSequence: sequence, Result: result, RepairID: repairID, Diagnostic: diagnostic}
	encoded, err := json.Marshal(response)
	if err != nil {
		return err
	}
	if len(encoded)+1 >= MaxFrameBytes {
		return fmt.Errorf("Pi bridge response exceeds %d bytes", MaxFrameBytes)
	}
	if err := c.conn.SetWriteDeadline(time.Now().Add(ResponseTimeout)); err != nil {
		return err
	}
	c.responded = true
	_, err = c.conn.Write(append(encoded, '\n'))
	_ = c.conn.SetWriteDeadline(time.Time{})
	if err != nil {
		return fmt.Errorf("write Pi bridge response: %w", err)
	}
	return nil
}

func (c *Connection) Close() error {
	return c.conn.Close()
}

func (c *Connection) validate(frame Frame) error {
	if frame.SchemaVersion != SchemaVersion {
		return fmt.Errorf("Pi bridge schema version is invalid")
	}
	if subtle.ConstantTimeCompare([]byte(frame.Token), []byte(c.channel.Token)) != 1 {
		return fmt.Errorf("Pi bridge token is invalid")
	}
	if frame.AttemptID != c.channel.AttemptID || frame.RunGeneration != c.channel.RunGeneration {
		return fmt.Errorf("Pi bridge run identity is stale")
	}
	if frame.Sequence != c.expected {
		return fmt.Errorf("Pi bridge sequence is invalid: got %d, want %d", frame.Sequence, c.expected)
	}
	switch frame.Kind {
	case "session":
		if frame.SessionID == "" || frame.Envelope != "" {
			return fmt.Errorf("Pi bridge session frame is invalid")
		}
	case "event_candidate":
		if frame.SessionID != "" || frame.Envelope == "" {
			return fmt.Errorf("Pi bridge event candidate frame is invalid")
		}
	case "settled", "invalid", "repair_exhausted":
		if frame.SessionID != "" || frame.Envelope != "" {
			return fmt.Errorf("Pi bridge control frame is invalid")
		}
	default:
		return fmt.Errorf("Pi bridge frame kind is invalid")
	}
	return nil
}

func AcceptTimeout() time.Duration {
	return 10 * time.Second
}
