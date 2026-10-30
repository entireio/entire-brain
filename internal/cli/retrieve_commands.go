package cli

import (
	"context"
	"io"

	"github.com/entireio/entire-brain/internal/cli/command/retrieve"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
)

// retrieveCommandConfig binds the cli-private seams the extracted retrieval
// commands still depend on (COR-2268): target resolution, the engine deps
// binding, the conversation arms, the pattern-corpus arm, the vitality
// receipt, the blind-spot note, the brief-derived task hints, and the
// retrieval-response transport.
func retrieveCommandConfig(opts Options) retrieve.Config {
	return retrieve.Config{
		Options: opts,
		Deps: retrieve.Deps{
			ResolveTarget:             resolveRetrieveTarget,
			RetrieveUnified:           retrieveUnifiedWithOptions,
			GetUnifiedBatch:           getUnifiedBatchOptions,
			AbstractPreviews:          abstractPreviewsForResults,
			RequirePatternsReadable:   requirePatternsReadable,
			RelatedPatternPointers:    relatedRetrievalPatternPointers,
			RecordServedFacts:         recordServedRetrievalFacts,
			EmptyResultBlindSpot:      emptyResultBlindSpotOnBranch,
			TaskHints:                 retrievalHintsForResults,
			RevalidateResponsePrivacy: revalidateRetrievalResponsePrivacy,
			WriteRetrievalResponse:    writeRetrievalResponseBytes,
			BoundedResponseBytes:      boundedRetrievalResponseBytes,
		},
	}
}

// resolveRetrieveTarget binds resolveFactsTarget over the agent-surface target
// for the current directory: the retrieval verbs take no positional repository
// argument.
func resolveRetrieveTarget(ctx context.Context, opts base.Options, branchOverride string) (repoDir, brainDir, branch string, err error) {
	return resolveFactsTarget(ctx, opts, agentSurfaceTarget(opts, nil), branchOverride)
}

// requirePatternsReadable is the pattern:/theme: read gate shared by the
// ranked --patterns path and get/multi-get: the privacy derived-read check
// (bound to cli's verifyDerivedClean) followed by the corpus availability
// check, in that order.
func requirePatternsReadable(brainDir string) error {
	if err := requirePrivacyDerivedRead(brainDir); err != nil {
		return err
	}
	return requirePatternCorpusAvailable(brainDir)
}

// relatedRetrievalPatternPointers binds the cli-side pointer cap to the
// corpus-backed pointer lookup.
func relatedRetrievalPatternPointers(brainDir, query string) ([]relatedPatternRef, error) {
	return relatedPatternPointersChecked(brainDir, query, patternPointerCap)
}

// recordServedRetrievalFacts binds the cli-private vitality receipt arm for
// the extracted retrieval commands: the receipt clock, the worktree HEAD, and
// the best-effort recording path.
func recordServedRetrievalFacts(errW io.Writer, ctx context.Context, opts base.Options, brainDir, branch, surface, repoDir, taskText string, factIDs []string) {
	recordServedFacts(errW, vitalityNow(opts), brainDir, branch, surface,
		vitalityHead(ctx, opts.Runner, repoDir), taskText, factIDs)
}
