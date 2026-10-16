package cli

import "fmt"

func wrapTestError(prefix string, err error) error {
	return fmt.Errorf("%s: %w", prefix, err)
}

// The exact text git produces, as it reaches the worker through the CLI shim.
var errNotAGitRepoForTest = fmt.Errorf("not a git repository")

// The decoder's own wording, as it reaches a caller unwrapped by any remedy.
var errUnknownFieldForTest = fmt.Errorf(`json: unknown field "suppressed_warnings"`)
