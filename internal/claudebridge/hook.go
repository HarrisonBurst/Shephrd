package claudebridge

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

func RunHook(input io.Reader, output io.Writer) error {
	body, invalid, err := readHookInput(input)
	if err != nil {
		return err
	}
	generation, err := strconv.Atoi(os.Getenv(RunGenerationEnvironment))
	if err != nil || generation < 1 {
		return fmt.Errorf("Claude bridge run generation is invalid")
	}
	request := request{
		SchemaVersion: SchemaVersion,
		Token:         os.Getenv(TokenEnvironment),
		AttemptID:     os.Getenv(AttemptEnvironment),
		RunGeneration: generation,
		Input:         body,
		Invalid:       invalid,
	}
	if os.Getenv(SocketEnvironment) == "" || request.Token == "" || request.AttemptID == "" {
		return fmt.Errorf("Claude bridge environment is incomplete")
	}
	conn, err := net.DialTimeout("unix", os.Getenv(SocketEnvironment), 5*time.Second)
	if err != nil {
		return fmt.Errorf("connect Claude bridge: %w", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		return err
	}
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return fmt.Errorf("write Claude bridge request: %w", err)
	}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1024), 16*1024)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("read Claude bridge response: %w", err)
		}
		return fmt.Errorf("Claude bridge response is empty")
	}
	decoder := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
	decoder.DisallowUnknownFields()
	var response response
	if err := decoder.Decode(&response); err != nil {
		return fmt.Errorf("invalid Claude bridge response: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("invalid Claude bridge response: trailing JSON")
	}
	if response.Decision != "" {
		return json.NewEncoder(output).Encode(map[string]any{"decision": response.Decision, "reason": response.Reason})
	}
	if response.Terminate {
		return json.NewEncoder(output).Encode(map[string]any{"continue": false, "stopReason": response.StopReason})
	}
	return nil
}

func readHookInput(input io.Reader) (json.RawMessage, string, error) {
	body, err := io.ReadAll(io.LimitReader(input, MaxHookInputBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("read Claude hook input: %w", err)
	}
	if len(body) > MaxHookInputBytes {
		return nil, "Claude hook input is oversized", nil
	}
	if len(strings.TrimSpace(string(body))) == 0 || !json.Valid(body) {
		return nil, "Claude hook input is malformed", nil
	}
	return json.RawMessage(body), "", nil
}
