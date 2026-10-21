package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"shephrd/internal/testkit"
	"shephrd/internal/version"
)

func agentCall(t *testing.T, binary string, header agentHeader, body string) agentResponse {
	t.Helper()
	command, _ := json.Marshal(header)
	var out bytes.Buffer
	Agent(context.Background(), HostEnv{Host: "laptop", DataDir: t.TempDir(), CLIPath: binary}, string(command), strings.NewReader(body), &out)
	var resp agentResponse
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("agent response: %v\n%s", err, out.String())
	}
	return resp
}

func TestAgentRefusesAnotherRelease(t *testing.T) {
	resp := agentCall(t, testkit.Binary(t), agentHeader{V: version.Protocol, Version: "some-other-release", Op: "info"}, "{}")
	if resp.Error == nil || resp.Error.Kind != "version_mismatch" {
		t.Fatalf("mismatched release: %+v", resp)
	}
}

func TestAgentRunsHostOperations(t *testing.T) {
	binary := testkit.Binary(t)
	out, err := exec.Command(binary, "version").Output()
	if err != nil {
		t.Fatal(err)
	}
	var installed struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(out, &installed); err != nil || installed.Version == "" {
		t.Fatalf("installed version: %s: %v", out, err)
	}
	release := version.Release
	version.Release = installed.Version
	defer func() { version.Release = release }()
	resp := agentCall(t, binary, agentHeader{V: version.Protocol, Version: version.String(), Op: "info"}, "{}")
	var info HostInfo
	if resp.Error != nil || json.Unmarshal(resp.Result, &info) != nil || info.Host != "laptop" {
		t.Fatalf("info: %+v", resp)
	}
	resp = agentCall(t, binary, agentHeader{V: version.Protocol, Version: version.String(), Op: "format_disk"}, "{}")
	if resp.Error == nil || resp.Error.Kind != "usage" {
		t.Fatalf("unknown operation: %+v", resp)
	}
}

func TestAgentChecksStampedInstalledCLIRelease(t *testing.T) {
	const release = "v0.0.0-20261018053737-4398cd95d064"
	binary := filepath.Join(t.TempDir(), "shephrd")
	cmd := exec.Command("go", "build", "-ldflags=-X shephrd/internal/version.Release="+release, "-o", binary, "./cmd/shephrd")
	cmd.Dir = filepath.Join("..", "..")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build stamped CLI: %v\n%s", err, out)
	}
	saved := version.Release
	version.Release = release
	defer func() { version.Release = saved }()
	matched := agentCall(t, binary, agentHeader{V: version.Protocol, Version: release, Op: "info"}, "{}")
	if matched.Error != nil {
		t.Fatalf("matching stamped CLI: %+v", matched)
	}
	version.Release = "different-release"
	refused := agentCall(t, binary, agentHeader{V: version.Protocol, Version: version.String(), Op: "info"}, "{}")
	if refused.Error == nil || refused.Error.Kind != "version_mismatch" {
		t.Fatalf("mismatched stamped CLI: %+v", refused)
	}
}
