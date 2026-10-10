package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"shephrd/internal/version"
)

func agentCall(t *testing.T, header agentHeader, body string) agentResponse {
	t.Helper()
	command, _ := json.Marshal(header)
	var out bytes.Buffer
	Agent(context.Background(), HostEnv{Host: "laptop", DataDir: t.TempDir()}, string(command), strings.NewReader(body), &out)
	var resp agentResponse
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("agent response: %v\n%s", err, out.String())
	}
	return resp
}

func TestAgentRefusesAnotherRelease(t *testing.T) {
	resp := agentCall(t, agentHeader{V: version.Protocol, Version: "some-other-release", Op: "info"}, "{}")
	if resp.Error == nil || resp.Error.Kind != "version_mismatch" {
		t.Fatalf("mismatched release: %+v", resp)
	}
}

func TestAgentRunsHostOperations(t *testing.T) {
	resp := agentCall(t, agentHeader{V: version.Protocol, Version: version.String(), Op: "info"}, "{}")
	var info HostInfo
	if resp.Error != nil || json.Unmarshal(resp.Result, &info) != nil || info.Host != "laptop" {
		t.Fatalf("info: %+v", resp)
	}
	resp = agentCall(t, agentHeader{V: version.Protocol, Version: version.String(), Op: "format_disk"}, "{}")
	if resp.Error == nil || resp.Error.Kind != "usage" {
		t.Fatalf("unknown operation: %+v", resp)
	}
}
