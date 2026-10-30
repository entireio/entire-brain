package cmdutil

import (
	"regexp"
	"strings"
)

var commandErrorCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// RenderedCommandError marks an error whose message was already written to the
// command's output, so the top-level error printer must not repeat it. The
// wrapped error stays reachable for errors.Is/As.
type RenderedCommandError struct {
	Err error
}

func (e RenderedCommandError) Error() string {
	if e.Err == nil {
		return "command error rendered"
	}
	return e.Err.Error()
}

func (e RenderedCommandError) Unwrap() error {
	return e.Err
}

func (e RenderedCommandError) Is(target error) bool {
	_, ok := target.(RenderedCommandError)
	return ok
}

func CommandErrorCode(err error) string {
	message := err.Error()
	code, _, ok := strings.Cut(message, ":")
	if ok {
		code = strings.TrimSpace(code)
		if commandErrorCodePattern.MatchString(code) {
			return code
		}
	}
	return "command_failed"
}
