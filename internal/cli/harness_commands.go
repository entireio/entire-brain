//go:build brain_harness

package cli

import (
	"github.com/entireio/entire-brain/internal/cli/command/bench"
	"github.com/spf13/cobra"
)

// Developer measurement harnesses (COR-2322): ~4k LOC of eval/bench tooling
// compiled only under the brain_harness build tag, so release binaries do not
// carry it. Run from a dev checkout with
// `go run -tags brain_harness ./cmd/entire-brain <command>`.

func harnessRootCommands(opts Options) []*cobra.Command {
	return []*cobra.Command{
		bench.New(benchConfig(opts)),
		newHistoryEvalGenCommand(opts),
		newHistoryEvalCommand(opts),
	}
}

func harnessFactsCommands(opts Options) []*cobra.Command {
	return []*cobra.Command{
		newFactsEvalCommand(opts),
		newFactsEvalGenCommand(opts),
		newFactsEvalCompareCommand(opts),
	}
}
