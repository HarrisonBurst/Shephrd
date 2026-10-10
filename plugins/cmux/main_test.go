package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCloseRechecksWorkspaceSurface(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(dir, "cmux"), []byte("#!/bin/sh\ncase \"$6\" in\n  tree) cat \"$CMUX_TREE\" ;;\n  workspace) touch \"$CMUX_CLOSED\" ;;\nesac\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(dir, "tree")
	closed := filepath.Join(dir, "closed")
	t.Setenv("CMUX_TREE", tree)
	t.Setenv("CMUX_CLOSED", closed)
	socket, window, password = "test", "window", ""
	req := request{}
	req.Body.Operation = "probe"
	req.Body.Endpoint = `{"window_id":"window","workspace_id":"workspace","surface_id":"surface"}`
	original := []byte(`{"windows":[{"id":"window","workspaces":[{"id":"workspace","panes":[{"surfaces":[{"id":"surface"}]}]}]}]}`)
	if err := os.WriteFile(tree, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if shown, err := handle(req); err != nil || shown["state"] != "present" {
		t.Fatalf("initial probe: %v %v", shown, err)
	}
	if err := os.WriteFile(tree, []byte(`{"windows":[{"id":"window","workspaces":[{"id":"other","panes":[{"surfaces":[{"id":"surface"}]}]},{"id":"workspace","panes":[{"surfaces":[{"id":"replacement"}]}]}]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	req.Body.Operation = "close"
	if _, err := handle(req); err == nil || !strings.Contains(err.Error(), "ownership is uncertain") {
		t.Fatalf("close with a replacement surface: %v", err)
	}
	if _, err := os.Stat(closed); !os.IsNotExist(err) {
		t.Fatalf("closed a replacement workspace: %v", err)
	}
	if err := os.WriteFile(tree, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := handle(req); err != nil {
		t.Fatalf("close with the original surface: %v", err)
	}
	if _, err := os.Stat(closed); err != nil {
		t.Fatalf("original workspace not closed: %v", err)
	}
}
