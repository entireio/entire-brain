//go:build !brain_harness

package cli

// Release builds carry no harness commands (COR-2322), so there are no
// harness documentation examples to execute.
func harnessDocumentedCommands() []documentedCommand { return nil }
