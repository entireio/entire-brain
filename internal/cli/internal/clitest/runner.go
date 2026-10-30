package clitest

import "context"

// CommandRunnerFunc adapts a function to the base.CommandRunner interface for
// test fakes.
type CommandRunnerFunc func(context.Context, string, string, ...string) ([]byte, []byte, error)

func (f CommandRunnerFunc) Run(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	return f(ctx, dir, name, args...)
}
