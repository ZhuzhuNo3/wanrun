package cli

import "fmt"

type UsageError struct {
	message string
	cause   error
}

func (err *UsageError) Error() string {
	if err.message != "" {
		return err.message
	}
	return err.cause.Error()
}

func (err *UsageError) Unwrap() error {
	return err.cause
}

func usageErrorf(format string, values ...any) error {
	return &UsageError{cause: fmt.Errorf(format, values...)}
}

func usageError(context string, cause error) error {
	return &UsageError{cause: fmt.Errorf("%s: %w", context, cause)}
}

func usageErrorWithCause(message string, cause error) error {
	return &UsageError{message: message, cause: cause}
}
