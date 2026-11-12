package sdk

import (
	"errors"
	"fmt"
)

// ErrIgnored tells the router to drop a command without any reply or reaction.
// Handlers return it when a group message attempts an administrative action:
// groups must not expose or acknowledge privileged commands.
var ErrIgnored = errors.New("sdk: command ignored")

// UserError is a command failure whose message is safe to send back to the
// user. Handlers should wrap validation and usage problems in Userf; internal
// failures stay plain errors and are only written to the route log.
type UserError struct {
	Message string
}

// Error implements error.
func (e *UserError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// UserMessage returns the user-facing text.
func (e *UserError) UserMessage() string { return e.Error() }

// Userf builds a UserError with a formatted message.
func Userf(format string, args ...any) error {
	return &UserError{Message: fmt.Sprintf(format, args...)}
}

// AsUserMessage returns the safe user-facing message carried by err, if any.
func AsUserMessage(err error) (string, bool) {
	var withMessage interface{ UserMessage() string }
	if errors.As(err, &withMessage) {
		message := withMessage.UserMessage()
		if message != "" {
			return message, true
		}
	}
	return "", false
}
