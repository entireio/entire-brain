//go:build !brain_harness

package cli

import "github.com/spf13/cobra"

// Release builds carry no developer harnesses (COR-2322); the brain_harness
// variants live in harness_commands.go.

func harnessRootCommands(Options) []*cobra.Command { return nil }

func harnessFactsCommands(Options) []*cobra.Command { return nil }
