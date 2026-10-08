package model

import "fmt"

type KindError struct {
	Kind     string
	Message  string
	Evidence map[string]string
}

func (e *KindError) Error() string {
	return e.Message
}

func (e *KindError) ErrorKind() string {
	return e.Kind
}

func (e *KindError) ErrorEvidence() map[string]string {
	return e.Evidence
}

func Failure(kind, format string, args ...any) error {
	return &KindError{Kind: kind, Message: fmt.Sprintf(format, args...)}
}

func EvidenceFailure(kind string, evidence map[string]string, format string, args ...any) error {
	return &KindError{Kind: kind, Message: fmt.Sprintf(format, args...), Evidence: evidence}
}
