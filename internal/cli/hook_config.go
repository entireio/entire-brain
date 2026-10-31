package cli

import (
	"context"
	"io"

	"github.com/entireio/entire-brain/internal/cli/command/hook"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
)

// hookCommandConfig binds the cli-private seams the extracted hook commands
// still depend on (COR-2268): target resolution, the locus-relevance engine,
// the legacy lexical scorer, and the vitality receipt.
func hookCommandConfig(opts Options) hook.Config {
	return hook.Config{
		Options: opts,
		Deps: hook.Deps{
			ResolveTarget:         resolveRetrieveTarget,
			FactsRelevantToChange: hookFactsRelevantToChange,
			FactLexicalScore:      factLexicalScore,
			RecordServed:          recordServedHookFacts,
		},
	}
}

// hookFactsRelevantToChange binds the locus-relevance engine with no semantic
// symbols: the hook is warm-path only (no semantic-store open; symbol-level
// pre-edit enrichment arrives with the worktree overlay seam).
func hookFactsRelevantToChange(files []string, facts []factRecord, limit int) []factRecord {
	return factsRelevantToChange(files, nil, facts, limit)
}

// recordServedHookFacts binds the cli-private vitality receipt arm for the
// extracted hook commands: the receipt clock, the worktree HEAD, and the
// best-effort recording path.
func recordServedHookFacts(errW io.Writer, ctx context.Context, opts base.Options, brainDir, branch, surface, repoDir, taskText string, served []factRecord) {
	recordServedFacts(errW, vitalityNow(opts), brainDir, branch, surface,
		vitalityHead(ctx, opts.Runner, repoDir), taskText, factRecordIDs(served))
}
