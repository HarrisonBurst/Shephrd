package runner

import (
	"errors"
	"os/exec"

	"shephrd/internal/adapter"
	"shephrd/internal/model"
)

type RepairRequest struct {
	ID            string
	CandidateHash string
}

type CandidateIngestion struct {
	Diagnostic      *adapter.Diagnostic
	PersistedCursor int64
}

type State struct {
	SetRunner              func(int) error
	SetSession             func(string) error
	UpdateCursor           func(int64) error
	Ingest                 func(model.Event, int64) error
	IngestCandidate        func([]model.Event, int64, string, RepairRequest, bool) (CandidateIngestion, error)
	RecordAdapterRejection func(int64, RepairRequest, adapter.Diagnostic) (*adapter.Diagnostic, error)
	RecordControlFailure   func(string) error
	RecordRunnerDeath      func(string) error
	Finish                 func(int, string) error
}

type Result struct {
	WaitError        error
	ExitCode         int
	Failure          string
	TerminalAccepted bool
}

type Lifecycle struct {
	Report  func(string, string) error
	Release func() error
}

func result(waitErr error, failure string, terminal bool) Result {
	exitCode := 0
	if waitErr != nil {
		exitCode = -1
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
	}
	return Result{WaitError: waitErr, ExitCode: exitCode, Failure: failure, TerminalAccepted: terminal}
}

func finishSetupFailure(state State, reason string) (Result, error) {
	_ = state.RecordControlFailure(reason)
	_ = state.Finish(-1, reason)
	return Result{ExitCode: -1, Failure: reason}, errors.New(reason)
}
