package terminal

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func liveHerdrExtensionClient(t *testing.T) (RuntimeClient, Parent) {
	t.Helper()
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Clean(filepath.Join(workingDirectory, "..", ".."))
	executable := filepath.Join(t.TempDir(), "shephrd-terminal-herdr")
	build := exec.Command("go", "build", "-o", executable, "./cmd/shephrd-terminal-herdr")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build E2E Herdr extension: %s: %v", output, err)
	}
	if err := os.Chmod(executable, 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	provider := NewHerdrExtensionProvider([]string{executable}, hex.EncodeToString(digest[:]), os.Environ())
	context := ParentContext{Values: map[string]string{
		"HERDR_ENV":          os.Getenv("HERDR_ENV"),
		"HERDR_SOCKET_PATH":  os.Getenv("HERDR_SOCKET_PATH"),
		"HERDR_WORKSPACE_ID": os.Getenv("HERDR_WORKSPACE_ID"),
		"HERDR_PANE_ID":      os.Getenv("HERDR_PANE_ID"),
	}, InvalidKeys: map[string]bool{}}
	detection := provider.Detect(context)
	if detection.State != DetectionMatched {
		t.Fatalf("Herdr extension detection = %+v", detection)
	}
	return provider.WithSocket(detection.Parent.SocketPath), detection.Parent
}
