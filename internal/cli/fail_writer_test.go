package cli

import "fmt"

// evalCommandFailWriter forces command output writes to fail. Shared by the
// brain_harness eval regression tests and the capabilities report test, so it
// stays untagged (COR-2322).
type evalCommandFailWriter struct{}

func (evalCommandFailWriter) Write([]byte) (int, error) { return 0, fmt.Errorf("eval output failure") }
