package scanner

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	extensionhost "shephrd/internal/extension"
	"shephrd/internal/repository/discovery"
)

func TestManifestDeclaresOnlyRepositoryDiscovery(t *testing.T) {
	var output bytes.Buffer
	if err := Run([]string{"describe"}, bytes.NewReader(nil), &output); err != nil {
		t.Fatal(err)
	}
	var manifest extensionhost.Manifest
	if err := extensionhost.StrictDecode(bytes.TrimSpace(output.Bytes()), &manifest); err != nil {
		t.Fatal(err)
	}
	if err := extensionhost.ValidateManifest(manifest, discovery.ExtensionID, []extensionhost.Capability{discovery.Capability()}); err != nil {
		t.Fatal(err)
	}
}

func TestScanRecursesAndSkipsDependencyAndHiddenDirectories(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wanted := filepath.Join(root, "group", "wanted")
	for _, path := range []string{
		wanted,
		filepath.Join(root, "node_modules", "skipped"),
		filepath.Join(root, "vendor", "skipped"),
		filepath.Join(root, ".hidden", "skipped"),
	} {
		command := exec.Command("git", "init", "-q", "-b", "main", path)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git init %s: %s: %v", path, output, err)
		}
	}
	candidates, err := scan(context.Background(), []string{root, filepath.Join(root, "missing")})
	if err != nil || len(candidates) != 1 || candidates[0].Path != wanted {
		t.Fatalf("candidates = %+v, err = %v", candidates, err)
	}
}
