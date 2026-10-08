package repository

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectContextPath(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".shephrd", "context.md")
	if got, err := ProjectContextPath(root); err != nil || got != "" {
		t.Fatalf("absent context = %q, err = %v", got, err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("discovery created project files: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("project overview"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ProjectContextPath(root); err != nil || got != ".shephrd/context.md" {
		t.Fatalf("context = %q, err = %v", got, err)
	}
}

func TestProjectContextRejectsDirectoriesAndEscapingLinks(t *testing.T) {
	for _, kind := range []string{"directory", "outside file", "outside directory"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			outside := t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, "context.md"), []byte("other repo"), 0o600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, ".shephrd", "context.md")
			var err error
			switch kind {
			case "directory":
				err = os.MkdirAll(path, 0o700)
			case "outside file":
				if err = os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
					err = os.Symlink(filepath.Join(outside, "context.md"), path)
				}
			case "outside directory":
				err = os.Symlink(outside, filepath.Dir(path))
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ProjectContextPath(root); err == nil || !strings.Contains(err.Error(), "project context") {
				t.Fatalf("accepted %s: %v", kind, err)
			}
		})
	}
}
