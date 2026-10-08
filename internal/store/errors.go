package store

import (
	"errors"
	"fmt"

	sqlite3 "modernc.org/sqlite/lib"
)

var (
	ErrStaleRun              = errors.New("stale worker run")
	ErrProtocolViolation     = errors.New("protocol violation")
	ErrRepoNameConflict      = errors.New("repo name conflict")
	ErrTaskFeatureConflict   = errors.New("task feature conflict")
	ErrPlanNameConflict      = errors.New("plan name conflict")
	ErrPlanPrerequisiteCycle = errors.New("plan prerequisite cycle")
)

type sqliteError interface {
	Code() int
}

type contractError struct {
	contract error
	message  string
}

func (e *contractError) Error() string {
	return e.message
}

func (e *contractError) Unwrap() error {
	return e.contract
}

func contractFailure(contract error, format string, args ...any) error {
	return &contractError{contract: contract, message: fmt.Sprintf(format, args...)}
}

func isSQLiteConstraint(err error, code int) bool {
	var sqliteErr sqliteError
	return errors.As(err, &sqliteErr) && sqliteErr.Code() == code
}

const (
	sqliteConstraintForeignKey = sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY
	sqliteConstraintTrigger    = sqlite3.SQLITE_CONSTRAINT_TRIGGER
	sqliteConstraintUnique     = sqlite3.SQLITE_CONSTRAINT_UNIQUE
)
