// Package apperr says what kind of failure an error is. Domain packages
// return kinded errors for what a caller got wrong or asked for in vain;
// the HTTP layer maps kinds to statuses in one place (internal/web,
// handleErr). An error with no kind, a cancelled context or a database
// that went away, is the server's fault and becomes a 500.
package apperr

import "fmt"

// Kind is what went wrong, from the caller's side.
type Kind int

const (
	_          Kind = iota
	BadRequest      // the request is malformed: a missing name, an unknown scope
	NotFound        // nothing by that name is the caller's
	Invalid         // well formed, but the domain refuses it (a draft that cannot publish)
	Conflict        // it clashes with what exists: a duplicate, a limit reached
)

// Kinded is an error that knows its kind. *Error is one; studio.Error is
// another, carrying its publish problems.
type Kinded interface {
	error
	ErrorKind() Kind
}

// Error is a kinded error with a message for the caller.
type Error struct {
	Kind Kind
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

// ErrorKind implements Kinded.
func (e *Error) ErrorKind() Kind { return e.Kind }

// New returns a kinded error. Sentinels made with it still match errors.Is.
func New(k Kind, msg string) *Error { return &Error{Kind: k, Msg: msg} }

// Errorf returns a kinded error with a formatted message.
func Errorf(k Kind, format string, a ...any) error {
	return &Error{Kind: k, Msg: fmt.Sprintf(format, a...)}
}
