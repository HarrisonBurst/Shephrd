package control

import (
	"strings"
	"testing"

	"shephrd/internal/model"
)

func TestRetiredWorkspaceBackendCannotReachNativeOperations(t *testing.T) {
	attempt := model.Attempt{ID: "attempt_legacy", WorkspaceBackend: model.WorkspaceBackendTreehouse}
	if err := requireNativeWorkspace(attempt); err == nil || !strings.Contains(err.Error(), "unsupported retired workspace backend") {
		t.Fatalf("retired backend validation = %v", err)
	}
}

func TestNativeWorkspaceBackendRemainsAccepted(t *testing.T) {
	attempt := model.Attempt{ID: "attempt_native", WorkspaceBackend: model.WorkspaceBackendNative}
	if err := requireNativeWorkspace(attempt); err != nil {
		t.Fatal(err)
	}
}
