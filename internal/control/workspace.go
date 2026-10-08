package control

import (
	"fmt"

	"shephrd/internal/model"
	"shephrd/internal/worktree"
)

type workspaceClassification struct {
	Exact         bool
	PathExists    bool
	Registered    bool
	Identity      worktree.NativeIdentity
	ProcessPIDs   []int
	FailureReason string
}

type workspaceReconcileResult struct {
	Entry    ReconcileEntry
	Verified bool
	Unknown  bool
}

func (s Service) workspaceBackend() *nativeWorkspaceBackend {
	return &nativeWorkspaceBackend{service: s}
}

func requireNativeWorkspace(attempt model.Attempt) error {
	if attempt.WorkspaceBackend != model.WorkspaceBackendNative {
		return fmt.Errorf("attempt %s has unsupported retired workspace backend %q", attempt.ID, attempt.WorkspaceBackend)
	}
	return nil
}
