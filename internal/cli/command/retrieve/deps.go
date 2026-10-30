// Package retrieve hosts the unified retrieval command surface — query,
// search, vsearch, get, multi-get — as it moves out of package cli (COR-2268).
// The remaining cli-bound seams — target resolution, the engine deps binding,
// the conversation arms, the pattern-corpus arm, the vitality receipt, the
// blind-spot note, the brief-derived task hints, and the retrieval-response
// transport — are injected through Deps; everything else comes from base,
// brainstore, factstore, cmdutil, and retrieval directly.
package retrieve

import (
	"context"
	"io"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/retrieval"
)

// Deps are the cli-bound seams the retrieval commands still need. root.go
// wires them from package cli; tests may supply fakes.
type Deps struct {
	// ResolveTarget resolves the repo, brain directory, and the branch facts
	// are scoped to (the live git branch unless overridden). It binds
	// resolveFactsTarget over the agent-surface target for the current
	// directory: none of these commands take a positional repository argument.
	ResolveTarget func(ctx context.Context, opts base.Options, branchOverride string) (repoDir, brainDir, branch string, err error)
	// RetrieveUnified is the unified retrieval engine bound to the cli-side
	// engine deps (guard composition, store rankers, conversation arms).
	RetrieveUnified func(repoDir, brainDir, branch, query string, limit int, mode retrieval.Mode, opts retrieval.Options) ([]retrieval.Result, error)
	// GetUnifiedBatch resolves prefixed ids to full records; it stays in cli
	// with the conversation session views and the pattern corpus readers.
	GetUnifiedBatch func(repoDir, brainDir, branch string, ids []string, gopts retrieval.GetOptions) (found []retrieval.Result, missing []string, err error)
	// AbstractPreviews builds the bounded evidence-linked session previews for
	// --include-abstract (conversation arm; read-only).
	AbstractPreviews func(brainDir string, results []retrieval.Result) ([]retrieval.SessionAbstractPreview, bool, error)
	// RequirePatternsReadable gates pattern:/theme: reads: the privacy
	// derived-read check (bound to cli's verifyDerivedClean) followed by the
	// pattern-corpus availability check.
	RequirePatternsReadable func(brainDir string) error
	// RelatedPatternPointers returns the capped pattern:/theme: pointers for
	// --patterns, with the cli-side pointer cap bound in.
	RelatedPatternPointers func(brainDir, query string) ([]retrieval.RelatedPatternRef, error)
	// RecordServedFacts writes the vitality receipt for served fact ids,
	// resolving the receipt clock and worktree HEAD from opts.
	RecordServedFacts func(errW io.Writer, ctx context.Context, opts base.Options, brainDir, branch, surface, repoDir, taskText string, factIDs []string)
	// EmptyResultBlindSpot explains an empty result on a branch whose store
	// holds memories elsewhere.
	EmptyResultBlindSpot func(brainDir, branch string) string
	// TaskHints derives likely edit/test files from indexed result paths; it
	// stays in cli with the brain-brief path classifiers it binds.
	TaskHints func(repoRoot string, results []retrieval.Result, query string) retrieval.TaskHints
	// RevalidateResponsePrivacy closes the ranking-to-write window for the
	// multi-concept arm (binds the cli race seam and the session-view rebuild).
	RevalidateResponsePrivacy func(brainDir string, results []retrieval.Result) ([]retrieval.Result, string, error)
	// WriteRetrievalResponse is the final transport write boundary: it
	// revalidates the captured privacy policies and writes under the Brain
	// write lock, binding the cli transport race seams.
	WriteRetrievalResponse func(out io.Writer, data []byte, policies ...brainstore.RetrievalPrivacyPolicy) error
	// BoundedResponseBytes renders the budget-bounded multi-concept response
	// (JSON or text), binding the cli transport trim/assembly machinery.
	BoundedResponseBytes func(query, branch string, results []retrieval.Result, extras retrieval.TransportExtras, jsonOut bool) ([]byte, error)
}

// Config carries the CLI options plus the injected cli-bound seams.
type Config struct {
	Options base.Options
	Deps    Deps
}
