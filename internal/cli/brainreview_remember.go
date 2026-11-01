package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/command/brainreview"
	"github.com/entireio/entire-brain/internal/cli/command/facts"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
)

// rememberReviewFindings is the cli half of RC.M2 write-back: it resolves the
// branch, the anchor commit and the session — none of which the command
// package can see — builds the records with brainreview's pure builder, and
// writes them under the brain write lock on the same path `remember` takes.
//
// It reuses that path deliberately rather than writing the file itself: the
// taxonomy write and the source-manifest update are part of a fact write, and
// a second writer that skipped either one would leave the store's manifest
// claiming a state the facts file no longer matches — a failure that has
// already cost this repo a 13-day frozen brain.
func rememberReviewFindings(ctx context.Context, opts Options, brainDir, repoRoot string, anomalies []brainreview.Anomaly) (brainreview.RememberOutcome, error) {
	var outcome brainreview.RememberOutcome
	if len(anomalies) == 0 {
		return outcome, nil
	}

	branch := reviewRememberBranch(ctx, opts, repoRoot)
	outcome.Branch = branch

	now := opts.Now().UTC()
	commit, _ := base.GitScalar(ctx, opts.Runner, repoRoot, "rev-parse", "HEAD")
	session := strings.TrimSpace(os.Getenv(facts.RememberSessionEnv))

	records, problems := brainreview.ReviewFindingRecords(anomalies, branch, strings.TrimSpace(commit), session, now)
	if len(records) == 0 {
		// Every finding failed to become a record. Reporting "remembered 0"
		// would read as "there was nothing to remember", which is the
		// opposite of what happened.
		return outcome, fmt.Errorf("no review finding could be recorded: %w", joinProblems(problems))
	}

	if err := brainstore.WithBrainWriteLock(brainDir, func() error {
		stored, err := factstore.Load(brainDir)
		if err != nil {
			return err
		}
		taxonomy, err := factstore.LoadTaxonomy(brainDir, now)
		if err != nil {
			return err
		}
		for _, record := range records {
			// A content-derived id means a re-review of an unchanged tree
			// matches the record it wrote last time. Upsert then unions the
			// provenance anchors and never touches Status, so an already
			// active record is not rewritten and one a human retracted is
			// NOT revived — a retraction is a human judgment that this
			// finding is not worth keeping, and a detector must not overturn
			// it on the next run.
			//
			// The store is flat per repository, so this presence check is
			// repo-wide rather than per-branch: a finding first recorded on
			// another branch counts as AlreadyPresent here. That is the right
			// reading of a content-derived id, which never depended on the
			// branch, and the record still carries its own Branch field.
			if i := factmerge.IndexOf(stored, record.ID); i >= 0 {
				outcome.AlreadyPresent++
			} else {
				outcome.Recorded++
			}
			stored = factmerge.Upsert(stored, record)
		}
		if err := factstore.Write(brainDir, stored); err != nil {
			return err
		}
		if err := factstore.WriteTaxonomy(brainDir, taxonomy); err != nil {
			return err
		}
		return updateFactSourceManifestLocked(brainDir, now)
	}); err != nil {
		return brainreview.RememberOutcome{}, err
	}

	return outcome, nil
}

// reviewRememberBranch resolves the branch the findings are filed on. Facts
// are partitioned per branch and a branch's file is a fork-time copy, so
// filing review reasoning on the wrong partition makes it silently
// unretrievable from where the work is happening — the same failure mode that
// makes a recall from a feature branch return zero rows with no warning.
//
// It mirrors resolveFactsTarget's fallback exactly: the current branch, or the
// distill default when git cannot name one (a detached HEAD, a fresh repo).
func reviewRememberBranch(ctx context.Context, opts Options, repoRoot string) string {
	if current, err := base.GitScalar(ctx, opts.Runner, repoRoot, "branch", "--show-current"); err == nil {
		if branch := strings.TrimSpace(current); branch != "" {
			return branch
		}
	}
	return distillDefaultBranch
}

func joinProblems(problems []error) error {
	if len(problems) == 0 {
		return nil
	}
	parts := make([]string, 0, len(problems))
	for _, problem := range problems {
		parts = append(parts, problem.Error())
	}
	return fmt.Errorf("%s", strings.Join(parts, "; "))
}
