// Package watch hosts the `entire brain watch` daemon loop as it moves out of
// package cli (COR-2268): the command constructor, the flag set shared with
// `workspace watch`, the tick's gating/cursor logic, the binary-staleness
// probe, and the attributed log writer. The per-repo side effects — the
// deterministic refresh, gated seed/distill, fingerprint, and hosted fact
// sync — are the refresh/seed/distill engine, which stays in cli and is
// injected whole through Deps; the daemon registration/re-registration arm
// stays in cli with `setup`.
package watch

import (
	"context"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/spf13/cobra"
)

// Deps are the cli-bound seams the watch command still needs. root.go wires
// them from package cli; tests may supply fakes.
type Deps struct {
	// Storage resolves where the repo's persisted watch cursor lives and the
	// repo's storage key (the identity the daemon's log lines carry).
	Storage func(ctx context.Context, opts base.Options, repoDir string) (cursorPath, repoKey string, err error)
	// StepsForRepo builds the per-repo side effects of one tick, bound to the
	// cli-side refresh/seed/distill engine.
	StepsForRepo func(cmd *cobra.Command, opts base.Options, w Flags, repoDir string, now func() time.Time) Steps
}

// Config carries the CLI options plus the injected cli-bound seams.
type Config struct {
	Options base.Options
	Deps    Deps
}
