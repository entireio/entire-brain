//go:build unix

package cli

import "golang.org/x/sys/unix"

func makeTestFIFO(path string) error { return unix.Mkfifo(path, 0o600) }
