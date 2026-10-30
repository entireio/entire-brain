// Package hook hosts the hidden agent-harness hook surface — pre-edit,
// post-failure, session-end — as it moves out of package cli (COR-2268).
// The remaining cli-bound seams — target resolution, the locus-relevance
// engine, the legacy lexical scorer, the vitality receipt, and the whole
// engine-bound session-end arm (incremental export, memory lifecycle hint,
// entity refresh, hosted sync, distill) — are injected through Deps;
// everything else comes from base, factstore, factmerge, and cmdutil
// directly.
package hook

import (
	"context"
	"io"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/spf13/cobra"
)

// Deps are the cli-bound seams the hook commands still need. root.go wires
// them from package cli; tests may supply fakes.
type Deps struct {
	// ResolveTarget resolves the repo, brain directory, and the branch facts
	// are scoped to (the live git branch unless overridden). It binds
	// resolveFactsTarget over the agent-surface target for the current
	// directory: the hook verbs take no positional repository argument.
	ResolveTarget func(ctx context.Context, opts base.Options, branchOverride string) (repoDir, brainDir, branch string, err error)
	// FactsRelevantToChange is the locus-relevance engine for pre-edit, bound
	// with no semantic symbols: the hook is warm-path only (no semantic-store
	// open), and the engine stays in cli with the semantic `inspect changes`
	// arm that also consumes it.
	FactsRelevantToChange func(files []string, facts []factmerge.Record, limit int) []factmerge.Record
	// FactLexicalScore is the legacy integer scorer post-failure's precision
	// threshold is calibrated to. It stays in cli because it reuses the
	// history index's term-matching machinery (history.go), which the history
	// retrieval engine also consumes.
	FactLexicalScore func(f factmerge.Record, query string, queryLocus []string) int
	// RecordServed writes the vitality receipt for the facts an emission
	// actually carried, resolving the receipt clock and worktree HEAD from
	// opts. Best-effort: recording problems go to errW, never fail the hook.
	RecordServed func(errW io.Writer, ctx context.Context, opts base.Options, brainDir, branch, surface, repoDir, taskText string, served []factmerge.Record)
	// SessionEnd is the whole engine-bound session-end arm, injected as one
	// seam: pause/worker-origin gating, incremental export, the durable
	// lifecycle hint, entity-index refresh, hosted sync, and the bounded
	// per-session distill all bind cli-private engines (watch, memory
	// coordinator, distill, hosted sync) and the cli-private
	// distillCommandOptions type.
	SessionEnd func(cmd *cobra.Command, opts base.Options, sessionID, branch, agent, model, effort string) error
}

// Config carries the CLI options plus the injected cli-bound seams.
type Config struct {
	Options base.Options
	Deps    Deps
}
