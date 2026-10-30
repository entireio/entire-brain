// Package stats hosts the brain stats command surface as it moves out of
// package cli (COR-2268). Doctor and the rest of the lifecycle explanation
// layer stay in cli (lifecycle_ops.go); the cli-private seams stats still
// needs — target resolution, repo storage paths, the conversation vector
// status, and the short-term overlay reader — are injected through Deps.
package stats

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/cmdutil"
	"github.com/entireio/entire-brain/internal/cli/internal/historystore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"github.com/spf13/cobra"
)

// Config carries the CLI options plus the injected cli-bound seams.
type Config struct {
	Options base.Options
	Deps    Deps
}

// Deps are the cli-bound seams the stats command still needs. root.go wires
// them from package cli; tests may supply fakes.
type Deps struct {
	// AgentSurfaceTarget resolves the repository path argument for a command.
	AgentSurfaceTarget func(opts base.Options, args []string) string
	// RepoStoragePaths resolves the established per-repo storage spelling
	// (brain dir and head path) without migrating or guessing between roots.
	RepoStoragePaths func(ctx context.Context, runner base.CommandRunner, env base.EntireEnv, repoDir string) (base.RepoStorage, error)
	// ConversationVectorStatus reports the conversation projection's vector
	// identity (buildConversationStatus in cli); ok is false when there is no
	// conversation projection at all.
	ConversationVectorStatus func(brainDir string, manifest *bmanifest.Manifest) (vectorState, vectorModelID string, vectors int, ok bool)
	// LoadShortTermOverlay loads the short-term memory overlay
	// (loadHistoryShortTermState in cli) reduced to the importable slice the
	// report needs.
	LoadShortTermOverlay func(brainDir string, source *bmanifest.HistorySource) ShortTermOverlay
	// ScanCacheVersion and FTSSchema are the history index versions
	// (historyScanCacheVersion / historyFTSSchema in cli): bumping either
	// invalidates/rebuilds the derived layer.
	ScanCacheVersion int
	FTSSchema        string
	// ConversationKind is the history record kind that marks a conversation
	// exchange (conversationKind in cli).
	ConversationKind string
}

// ShortTermOverlay is the stats-facing slice of the cli short-term memory
// overlay: the typed load state resolved into booleans, plus the per-transcript
// records the report counts. The cli wrapper adapts its private shortTermIndex.
type ShortTermOverlay struct {
	// State is the typed overlay load state (absent | current | stale |
	// corrupt | unsupported), reported verbatim.
	State string
	// Absent and Current resolve the two states the report builder branches
	// on; Complete reports whether the overlay fully represents every changed
	// transcript it was asked to cover (nothing failed, nothing dropped).
	Absent      bool
	Current     bool
	Complete    bool
	Files       map[string][]historystore.Record
	GeneratedAt time.Time
	Truncated   bool
	FailedFiles []string
}

type Report struct {
	GeneratedAt  time.Time     `json:"generated_at"`
	RepoKey      string        `json:"repo_key,omitempty"`
	Sessions     Sessions      `json:"sessions"`
	History      History       `json:"history"`
	Conversation *Conversation `json:"conversation,omitempty"`
	// ShortTerm reports the short-term memory overlay (changed transcripts
	// indexed since the last consolidation), when present.
	ShortTerm *ShortTerm `json:"short_term,omitempty"`
}

type ShortTerm struct {
	Files       int    `json:"files"`
	Records     int    `json:"records"`
	Exchanges   int    `json:"exchanges"`
	GeneratedAt string `json:"generated_at"`
	Truncated   bool   `json:"truncated,omitempty"`
	// State is the typed overlay load state; FailedFiles are the
	// transcripts the last delta could not scan.
	State       string   `json:"state,omitempty"`
	FailedFiles []string `json:"failed_files,omitempty"`
	// stateIsCurrent records whether State is the normal "current" overlay
	// state, which the human rendering suppresses; the state strings
	// themselves stay cli-private (history_delta.go).
	stateIsCurrent bool
}

type Sessions struct {
	Count    int            `json:"count"`
	ByBranch map[string]int `json:"by_branch,omitempty"`
	ByAgent  map[string]int `json:"by_agent,omitempty"`
	Oldest   string         `json:"oldest,omitempty"`
	Latest   string         `json:"latest,omitempty"`
}

type History struct {
	Records     int            `json:"records"`
	ByKind      map[string]int `json:"by_kind,omitempty"`
	GeneratedAt string         `json:"generated_at,omitempty"`
	// Index versions: bumping either invalidates/rebuilds the derived layer.
	ScanCacheVersion int    `json:"scan_cache_version"`
	FTSSchema        string `json:"fts_schema"`
}

type Conversation struct {
	Exchanges           int            `json:"exchanges"`
	IncompleteExchanges int            `json:"incomplete_exchanges"`
	ByBranch            map[string]int `json:"by_branch,omitempty"`
	ByAgent             map[string]int `json:"by_agent,omitempty"`
	// Completion state of indexed exchanges: complete ranges vs degraded
	// (range_incomplete / identity_degraded / projection_truncated).
	RangeIncomplete     int    `json:"range_incomplete"`
	IdentityDegraded    int    `json:"identity_degraded"`
	ProjectionTruncated int    `json:"projection_truncated"`
	OldestCreatedAt     string `json:"oldest_created_at,omitempty"`
	LatestCreatedAt     string `json:"latest_created_at,omitempty"`
	VectorState         string `json:"vector_state"`
	VectorModelID       string `json:"vector_model_id,omitempty"`
	Vectors             int    `json:"vectors,omitempty"`
}

func New(cfg Config) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Show brain record counts and breakdowns",
		Long:  "Report brain counts and ranges by branch, agent, source, completion state, and index version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cmd.Context(), cmd, cfg, jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}

func run(ctx context.Context, cmd *cobra.Command, cfg Config, jsonOut bool) error {
	opts := cfg.Options
	target := cfg.Deps.AgentSurfaceTarget(opts, nil)
	repoDir, local, err := base.ResolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return err
	}
	if !local {
		return fmt.Errorf("stats requires a local repository path: %s", target)
	}
	storage, err := cfg.Deps.RepoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}
	report, err := BuildReport(storage.BrainDir, opts.Now().UTC(), cfg.Deps)
	if err != nil {
		return err
	}
	if jsonOut {
		return cmdutil.WriteJSON(cmd, report)
	}
	return cmdutil.WriteText(cmd, func(out io.Writer) { renderReport(out, report) })
}

func BuildReport(brainDir string, now time.Time, deps Deps) (Report, error) {
	report := Report{GeneratedAt: now}
	manifest, err := bmanifest.Load(brainDir)
	if err != nil {
		return report, err
	}
	report.RepoKey = manifest.RepoKey
	if manifest.Sources != nil && manifest.Sources.Sessions != nil {
		sessions := manifest.Sources.Sessions.Sessions
		stats := Sessions{Count: len(sessions), ByBranch: map[string]int{}, ByAgent: map[string]int{}}
		var oldest, latest time.Time
		for _, session := range sessions {
			if session.Branch != "" {
				stats.ByBranch[session.Branch]++
			}
			if session.Agent != "" {
				stats.ByAgent[session.Agent]++
			}
			if !session.CreatedAt.IsZero() {
				if oldest.IsZero() || session.CreatedAt.Before(oldest) {
					oldest = session.CreatedAt
				}
				if session.CreatedAt.After(latest) {
					latest = session.CreatedAt
				}
			}
		}
		if !oldest.IsZero() {
			stats.Oldest = oldest.UTC().Format(time.RFC3339)
			stats.Latest = latest.UTC().Format(time.RFC3339)
		}
		report.Sessions = stats
	}
	report.History = History{ScanCacheVersion: deps.ScanCacheVersion, FTSSchema: deps.FTSSchema}
	if manifest.Sources == nil || manifest.Sources.History == nil {
		return report, nil
	}
	history := manifest.Sources.History
	report.History.GeneratedAt = history.GeneratedAt.UTC().Format(time.RFC3339)
	index, err := historystore.LoadIndex(brainDir, history)
	if err != nil {
		return report, fmt.Errorf("load history index: %w", err)
	}
	report.History.Records = len(index.Records)
	report.History.ByKind = map[string]int{}
	conversation := Conversation{
		IncompleteExchanges: history.IncompleteExchanges,
		ByBranch:            map[string]int{},
		ByAgent:             map[string]int{},
	}
	var oldest, latest time.Time
	for _, record := range index.Records {
		report.History.ByKind[record.Kind]++
		if record.Kind != deps.ConversationKind {
			continue
		}
		conversation.Exchanges++
		if record.Branch != "" {
			conversation.ByBranch[record.Branch]++
		}
		if record.Agent != "" {
			conversation.ByAgent[record.Agent]++
		}
		if record.RangeIncomplete {
			conversation.RangeIncomplete++
		}
		if record.IdentityDegraded {
			conversation.IdentityDegraded++
		}
		if record.ProjectionTruncated {
			conversation.ProjectionTruncated++
		}
		if created, err := time.Parse(time.RFC3339, record.CreatedAt); err == nil {
			if oldest.IsZero() || created.Before(oldest) {
				oldest = created
			}
			if created.After(latest) {
				latest = created
			}
		}
	}
	if !oldest.IsZero() {
		conversation.OldestCreatedAt = oldest.UTC().Format(time.RFC3339)
		conversation.LatestCreatedAt = latest.UTC().Format(time.RFC3339)
	}
	if vectorState, vectorModelID, vectors, ok := deps.ConversationVectorStatus(brainDir, manifest); ok {
		conversation.VectorState = vectorState
		conversation.VectorModelID = vectorModelID
		conversation.Vectors = vectors
	}
	report.Conversation = &conversation
	if overlay := deps.LoadShortTermOverlay(brainDir, history); !overlay.Absent && (len(overlay.Files) > 0 || !overlay.Complete || !overlay.Current) {
		stats := ShortTerm{
			Files:          len(overlay.Files),
			GeneratedAt:    overlay.GeneratedAt.UTC().Format(time.RFC3339),
			Truncated:      overlay.Truncated,
			State:          overlay.State,
			FailedFiles:    overlay.FailedFiles,
			stateIsCurrent: overlay.Current,
		}
		for _, records := range overlay.Files {
			stats.Records += len(records)
			for _, record := range records {
				if record.Kind == deps.ConversationKind {
					stats.Exchanges++
				}
			}
		}
		report.ShortTerm = &stats
	}
	return report, nil
}

func renderReport(out io.Writer, report Report) {
	fmt.Fprintf(out, "repo: %s\n", cmdutil.ValueOrUnset(report.RepoKey))
	fmt.Fprintf(out, "sessions: %d", report.Sessions.Count)
	if report.Sessions.Oldest != "" {
		fmt.Fprintf(out, " (%s .. %s)", report.Sessions.Oldest, report.Sessions.Latest)
	}
	fmt.Fprintln(out)
	renderCounts(out, "  by branch", report.Sessions.ByBranch)
	renderCounts(out, "  by agent", report.Sessions.ByAgent)
	fmt.Fprintf(out, "history: %d records (scan cache v%d, fts schema %s)\n", report.History.Records, report.History.ScanCacheVersion, report.History.FTSSchema)
	renderCounts(out, "  by kind", report.History.ByKind)
	if c := report.Conversation; c != nil {
		fmt.Fprintf(out, "conversation: %d exchanges (%d incomplete, %d range-incomplete, %d degraded-identity, %d truncated-projection)\n",
			c.Exchanges, c.IncompleteExchanges, c.RangeIncomplete, c.IdentityDegraded, c.ProjectionTruncated)
		if c.OldestCreatedAt != "" {
			fmt.Fprintf(out, "  sessions span %s .. %s\n", c.OldestCreatedAt, c.LatestCreatedAt)
		}
		renderCounts(out, "  by branch", c.ByBranch)
		renderCounts(out, "  by agent", c.ByAgent)
		line := "  vectors: " + c.VectorState
		if c.VectorState == "current" {
			line += fmt.Sprintf(" (%d, %s)", c.Vectors, c.VectorModelID)
		}
		fmt.Fprintln(out, line)
	}
	if s := report.ShortTerm; s != nil {
		line := fmt.Sprintf("short-term memory: %d records (%d exchanges) from %d changed transcripts (built %s)", s.Records, s.Exchanges, s.Files, s.GeneratedAt)
		if s.State != "" && !s.stateIsCurrent {
			line += "; state " + s.State
		}
		if s.Truncated {
			line += "; buffer full, consolidate with `" + base.BrainInvocation(os.LookupEnv, "refresh") + "`"
		}
		if len(s.FailedFiles) > 0 {
			line += fmt.Sprintf("; %d transcripts failed to scan", len(s.FailedFiles))
		}
		fmt.Fprintln(out, line)
	}
}

func renderCounts(out io.Writer, label string, counts map[string]int) {
	if len(counts) == 0 {
		return
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	fmt.Fprint(out, label+":")
	for _, key := range keys {
		fmt.Fprintf(out, " %s=%d", key, counts[key])
	}
	fmt.Fprintln(out)
}
