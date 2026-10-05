//go:build !unix

package cli

import "errors"

func makeTestFIFO(string) error { return errors.New("FIFOs are not available on this platform") }
