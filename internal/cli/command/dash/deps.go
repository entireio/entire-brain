// Package dash hosts the `entire brain dash` terminal dashboard command as it
// moves out of package cli (COR-2268). The remaining cli-bound seams —
// repo/target resolution, the status-report Home view, the unified retrieval
// engine, the retrieval-response transport (buffered and linearized), and the
// on-demand transcript reader — are injected through Deps; everything else
// comes from bmanifest, brainstore, factstore, factmerge, historystore,
// semstore, retrieval, and tui directly.
package dash

import (
	"context"
	"io"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"github.com/entireio/entire-brain/internal/cli/internal/retrieval"
	"github.com/entireio/entire-brain/internal/tui"
	"github.com/spf13/cobra"
)

// StatusHome is the status-report-derived piece of the dashboard snapshot. The
// status report itself is a cli-private type, so the wrapper hands over only
// the values the snapshot needs.
type StatusHome struct {
	Home        tui.HomeView
	Manifest    *bmanifest.Manifest
	Guard       brainstore.SessionReadGuard
	RepoRoot    string
	GeneratedAt string
}

// Deps are the cli-bound seams the dash command still needs. root.go wires
// them from package cli; tests may supply fakes.
type Deps struct {
	// AgentSurfaceTarget resolves the repository path argument for a command.
	AgentSurfaceTarget func(opts base.Options, args []string) string
	// ResolveFactsTarget resolves the repo, brain directory, and the branch
	// the brain is scoped to (the live git branch unless overridden).
	ResolveFactsTarget func(ctx context.Context, opts base.Options, target, branchOverride string) (repoDir, brainDir, branch string, err error)
	// StatusHome builds the Home tab view plus the manifest, session read
	// guard, repo root, and build stamp from the target's full status report.
	StatusHome func(ctx context.Context, opts base.Options, target, branch, brainDir string) (StatusHome, error)
	// RetrieveUnified is the unified retrieval engine bound to the cli-side
	// deps; same code path as `entire brain search`.
	RetrieveUnified func(repoDir, brainDir, branch, query string, limit int, mode retrieval.Mode) ([]retrieval.Result, error)
	// BufferRetrievalOutput buffers the command's exact bytes and commits them
	// through the shared final privacy boundary (--json / --plain paths).
	BufferRetrievalOutput func(cmd *cobra.Command, policies []brainstore.RetrievalPrivacyPolicy, render func() error) error
	// TerminalWriter wraps the dashboard's terminal stream in the
	// privacy-linearized writer while keeping the underlying file visible to
	// the terminal library.
	TerminalWriter func(out io.Writer, policy brainstore.RetrievalPrivacyPolicy) io.Writer
	// BrainSourceReader reads virtual transcripts on demand under the fresh
	// privacy gate, keeping transcript addresses out of the OS opener.
	BrainSourceReader func(ctx context.Context, runner base.CommandRunner, brainDir string) tui.SourceReader
}

// Config carries the CLI options plus the injected cli-bound seams.
type Config struct {
	Options base.Options
	Deps    Deps
}
