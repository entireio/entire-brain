// Package patternscmd hosts the `entire brain patterns` command tree as it
// moves out of package cli (COR-2268). The feature logic — corpus, legacy and
// theme views, skill memory — lives in internal/cli/patterns; the remaining
// cli-bound seams (target/storage resolution, the derived-clean privacy gate,
// the retrieval-response transport, the episode/corpus rebuild engine, source
// freshness, and the verify/skills subcommand constructors) are injected
// through Deps.
package patternscmd

import (
	"context"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"github.com/spf13/cobra"
)

// Deps are the cli-bound seams the patterns commands still need. root.go wires
// them from package cli; tests may supply fakes.
type Deps struct {
	// AgentSurfaceTarget resolves the repository path argument for a command.
	AgentSurfaceTarget func(opts base.Options, args []string) string
	// ResolveRepoAndBrain resolves both the local repo directory (needed as
	// the agent runner's working dir) and the brain storage dir for that repo.
	ResolveRepoAndBrain func(ctx context.Context, opts base.Options, target string) (repoDir, brainDir string, err error)
	// BufferRetrievalCommandOutput buffers the rendered response and emits it
	// only when rendering succeeded, so a render error never leaves partial
	// output behind.
	BufferRetrievalCommandOutput func(cmd *cobra.Command, render func() error) error
	// RefreshPatternLayer rebuilds episodes and publishes the corpus for
	// `patterns refresh` — the stricter surface, where a corpus failure is
	// fatal.
	RefreshPatternLayer func(ctx context.Context, opts base.Options, repoDir, brainDir string) (*bmanifest.PatternSource, error)
	// PatternSourceCurrent reports whether the recorded patterns source is
	// still current for the brain's sessions.
	PatternSourceCurrent func(source *bmanifest.PatternSource, brainDir string) bool
	// VerifyCommand builds the `patterns verify` subcommand, which stays in
	// cli with the agent-verifier engine it binds.
	VerifyCommand func(opts base.Options) *cobra.Command
	// SkillsCommand builds the `patterns skills` subcommand, which stays in
	// cli with the skill-synthesis surface it binds.
	SkillsCommand func(opts base.Options) *cobra.Command
}

// Config carries the CLI options plus the injected cli-bound seams.
type Config struct {
	Options base.Options
	Deps    Deps
}
