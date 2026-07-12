package cli

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// factsVitalityReport is the JSON contract of `facts vitality`: the branch's
// serve receipts, joined against the fact store so departed facts are visible
// as such. Inspection only — it changes no read behavior.
type factsVitalityReport struct {
	SchemaVersion int                   `json:"schema_version"`
	GeneratedAt   time.Time             `json:"generated_at"`
	Branch        string                `json:"branch"`
	Facts         []factsVitalityEntry  `json:"facts"`
	Log           factsVitalityLogState `json:"log"`
	Warnings      []string              `json:"warnings,omitempty"`
}

type factsVitalityEntry struct {
	ID          string         `json:"id"`
	Served      int            `json:"served"`
	FirstServed time.Time      `json:"first_served"`
	LastServed  time.Time      `json:"last_served"`
	LastSurface string         `json:"last_surface,omitempty"`
	LastHead    string         `json:"last_head,omitempty"`
	LastTaskSHA string         `json:"last_task_sha256,omitempty"`
	Surfaces    map[string]int `json:"surfaces,omitempty"`
	// Missing marks receipts whose fact has since left the store (retracted +
	// gc'd, or rewritten under a new id). Receipts are evidence, so they are
	// reported rather than silently dropped.
	Missing bool   `json:"missing,omitempty"`
	Status  string `json:"status,omitempty"`
	Text    string `json:"text,omitempty"`
}

type factsVitalityLogState struct {
	PendingEvents  int   `json:"pending_events"`
	MalformedLines int   `json:"malformed_lines,omitempty"`
	Bytes          int64 `json:"bytes"`
}

func newFactsVitalityCommand(opts Options) *cobra.Command {
	var (
		branch  string
		factID  string
		limit   int
		compact bool
		jsonOut bool
	)
	cmd := &cobra.Command{
		Use:   "vitality",
		Short: "Show serve receipts per fact (which facts the read surfaces actually emitted)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if limit <= 0 {
				return fmt.Errorf("--limit must be greater than 0")
			}
			_, brainDir, resolvedBranch, err := resolveFactsTarget(cmd.Context(), opts, agentSurfaceTarget(opts, nil), branch)
			if err != nil {
				return err
			}
			report, err := buildFactsVitalityReport(brainDir, resolvedBranch, compact, opts.Now().UTC())
			if err != nil {
				return err
			}
			if factID != "" {
				filtered := report.Facts[:0]
				for _, entry := range report.Facts {
					if entry.ID == factID {
						filtered = append(filtered, entry)
					}
				}
				report.Facts = filtered
			}
			if len(report.Facts) > limit {
				report.Facts = report.Facts[:limit]
			}
			if jsonOut {
				return writeJSON(cmd, report)
			}
			printFactsVitality(cmd, report, factID)
			return nil
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "", "Branch whose receipts to show (default: current branch)")
	cmd.Flags().StringVar(&factID, "fact", "", "Show only this fact id")
	cmd.Flags().IntVar(&limit, "limit", 20, "Maximum facts to list (most recently served first)")
	cmd.Flags().BoolVar(&compact, "compact", false, "Fold the append log into the compacted rollup before reporting")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}

func buildFactsVitalityReport(brainDir, branch string, compact bool, now time.Time) (factsVitalityReport, error) {
	report := factsVitalityReport{
		SchemaVersion: factsVitalitySchemaVersion,
		GeneratedAt:   now,
		Branch:        branch,
		Facts:         []factsVitalityEntry{},
	}
	if compact {
		if _, stats, err := compactVitality(brainDir, branch); err != nil {
			report.Warnings = append(report.Warnings, "compaction failed: "+err.Error())
		} else if stats.RollupReset {
			report.Warnings = append(report.Warnings, "compacted rollup was corrupt and has been rebuilt from the remaining log")
		}
	}
	rollup, stats, err := loadVitalityView(brainDir, branch)
	if err != nil {
		// The view is inspection over best-effort telemetry: report the problem
		// instead of failing the command.
		report.Warnings = append(report.Warnings, "vitality ledger unreadable: "+err.Error())
		rollup = newVitalityRollup()
	}
	if stats.RollupCorrupt {
		report.Warnings = append(report.Warnings, "compacted rollup is corrupt; showing pending log events only (run --compact to rebuild)")
	}
	report.Log = factsVitalityLogState{
		PendingEvents:  stats.PendingEvents,
		MalformedLines: stats.MalformedLines,
		Bytes:          stats.LogBytes,
	}

	// Join against the fact store so missing facts are visible. A broken fact
	// store degrades the join, never the receipts.
	factsByID := map[string]factRecord{}
	storeLoaded := false
	if facts, factsErr := loadFacts(brainDir, branch); factsErr != nil {
		report.Warnings = append(report.Warnings, "fact store unavailable for join: "+factsErr.Error())
	} else {
		storeLoaded = true
		for _, f := range facts {
			factsByID[f.ID] = f
		}
	}
	for id, entry := range rollup.Facts {
		out := factsVitalityEntry{
			ID:          id,
			Served:      entry.Served,
			FirstServed: entry.FirstServed,
			LastServed:  entry.LastServed,
			LastSurface: entry.LastSurface,
			LastHead:    entry.LastHead,
			LastTaskSHA: entry.LastTaskSHA,
			Surfaces:    entry.Surfaces,
		}
		if f, ok := factsByID[id]; ok {
			out.Status = f.Status
			out.Text = f.Text
		} else if storeLoaded {
			out.Missing = true
		}
		report.Facts = append(report.Facts, out)
	}
	sort.Slice(report.Facts, func(a, b int) bool {
		if !report.Facts[a].LastServed.Equal(report.Facts[b].LastServed) {
			return report.Facts[a].LastServed.After(report.Facts[b].LastServed)
		}
		return report.Facts[a].ID < report.Facts[b].ID
	})
	return report, nil
}

func printFactsVitality(cmd *cobra.Command, report factsVitalityReport, factID string) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "facts vitality for %s\n", report.Branch)
	if len(report.Facts) == 0 {
		if factID != "" {
			fmt.Fprintf(out, "no serve receipts for %s on %s\n", factID, report.Branch)
		} else {
			fmt.Fprintf(out, "no serve receipts on %s\n", report.Branch)
		}
	}
	for _, entry := range report.Facts {
		marker := ""
		if entry.Missing {
			marker = " (missing from store)"
		} else if entry.Status != "" && entry.Status != factStatusActive {
			marker = " (" + entry.Status + ")"
		}
		fmt.Fprintf(out, "%s served=%d last=%s surface=%s%s\n",
			entry.ID, entry.Served, entry.LastServed.UTC().Format(time.RFC3339), valueOrUnset(entry.LastSurface), marker)
		if text := strings.TrimSpace(entry.Text); text != "" {
			fmt.Fprintf(out, "  %s\n", truncateString(text, 120))
		}
	}
	fmt.Fprintf(out, "log: pending_events=%d malformed_lines=%d bytes=%d\n",
		report.Log.PendingEvents, report.Log.MalformedLines, report.Log.Bytes)
	for _, warning := range report.Warnings {
		fmt.Fprintf(out, "warning: %s\n", warning)
	}
}
