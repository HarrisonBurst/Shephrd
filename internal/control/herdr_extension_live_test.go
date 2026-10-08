package control

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func liveHerdrExtensionConfig(t *testing.T, repositoryRoot, sandbox string) string {
	t.Helper()
	if os.Getenv("SHEPHRD_REAL_HERDR_EXTENSION_E2E") != "1" {
		return ""
	}
	executable := filepath.Join(sandbox, "shephrd-terminal-herdr")
	build := exec.Command("go", "build", "-o", executable, "./cmd/shephrd-terminal-herdr")
	build.Dir = repositoryRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build E2E Herdr extension: %s: %v", output, err)
	}
	body, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	return fmt.Sprintf("\n[terminal_extensions.herdr]\ncommand = [%q]\nsha256 = %q\n", executable, hex.EncodeToString(digest[:]))
}
