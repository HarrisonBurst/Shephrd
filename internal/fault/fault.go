package fault

import (
	"errors"
	"fmt"
)

type Error struct {
	Kind    string     `json:"kind"`
	Message string     `json:"message"`
	Next    [][]string `json:"next,omitempty"`
	Unknown bool       `json:"-"`
}

func (e *Error) Error() string {
	return e.Kind + ": " + e.Message
}

func New(kind, format string, args ...any) *Error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, args...)}
}

func (e *Error) WithNext(argv ...string) *Error {
	e.Next = append(e.Next, append([]string{"shephrd"}, argv...))
	return e
}

func As(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Kind: "internal", Message: err.Error()}
}
