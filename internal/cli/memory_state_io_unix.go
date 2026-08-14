//go:build !windows

package cli

import "golang.org/x/sys/unix"

func memoryStateReadOpenFlags() int { return unix.O_NONBLOCK }
