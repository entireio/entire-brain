package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// lifecycle_ops.go is Phase 3 (lifecycle reliability and operations) of the
// conversational-memory plan: observability for the capture -> export -> index
// -> recall chain. Automatic indexing itself is inherited (`watch` already
// drives the deterministic refresh that builds exchanges), so what lives here
// is the explanation layer: doctor's capture-to-recall checks and the stats
// surface (counts/ranges by branch, agent, source, completion state, and index
// version).

type doctorCheckResult struct {
	Name   string `json:"name"`
	State  string `json:"state"` // ok | warn | error
	Detail string `json:"detail,omitempty"`
}

func memoryDoctorChecks(snapshot memoryReadOnlyHealthSnapshot) []doctorCheckResult {
	checks := make([]doctorCheckResult, 0, 15)
	add := func(name, state, detail string) {
		checks = append(checks, doctorCheckResult{Name: name, State: state, Detail: detail})
	}
	install, _ := snapshot.Payload["install"].(map[string]any)
	if binary, ok := install["entire_binary"].(memoryInstallBinaryHealth); ok {
		state := "warn"
		switch binary.State {
		case "not_found", "unsafe_relative_path", "lookup_failed":
			state = "error"
		}
		detail := binary.State
		if binary.Path != "" {
			detail += " at " + binary.Path
		}
		detail += "; binary was not executed"
		add("memory_install", state, detail)
	}
	if adapter, ok := install["host_adapter"].(memoryHostAdapterHealth); ok {
		state := "warn"
		if adapter.Observability != "not_observable" && adapter.State == "trusted" {
			state = "ok"
		}
		add("memory_host_adapter", state, fmt.Sprintf("%s; authority %s; Claude Code/Codex presence, enablement, and trust are not attested by this process", adapter.State, adapter.Authority))
	}
	if coordinator, ok := snapshot.Payload["coordinator"].(map[string]any); ok {
		stateName, _ := coordinator["state"].(string)
		state := "ok"
		detail := stateName
		if stale, _ := coordinator["stale"].(bool); stale {
			state = "warn"
			detail += "; stale heartbeat, next coordinator owner may recover"
		}
		if code, _ := coordinator["error_code"].(string); code != "" {
			state = "error"
			detail = code
		}
		if stateName == "absent" {
			detail = "no coordinator state yet"
		}
		add("memory_coordinator", state, detail)
	}
	if locks, ok := snapshot.Payload["locks"].(map[string]any); ok {
		if writeLock, ok := locks["write"].(map[string]any); ok {
			lockState, _ := writeLock["state"].(string)
			state := "ok"
			detail := lockState + "; read-only inspection does not probe-acquire the lock"
			switch lockState {
			case "unsafe", "unavailable":
				state = "error"
			case "present_unproven":
				// NOT a warning. The lock file is created with O_CREATE and is
				// never unlinked on release, so it exists permanently after the
				// very first refresh; warning on its presence made every
				// healthy brain report write_lock: warn forever, and since a
				// genuinely held lock produces the identical read-only state,
				// the warning carried no information either way. Liveness is
				// reported by memory_coordinator (stale heartbeat), which is
				// the signal that can actually distinguish a stuck refresh.
				detail += "; the lock leaf persists after release, so presence alone is normal (see memory_coordinator for liveness)"
			}
			add("write_lock", state, detail)
		}
	}
	if provider, ok := install["provider_egress"].(map[string]any); ok {
		state := "ok"
		detail := fmt.Sprintf("config %v; provider %v; configured egress %v; global policy %v; effective %v", provider["state"], provider["provider"], provider["egress_class"], provider["global_policy"], provider["effective_state"])
		if code, _ := provider["error_code"].(string); code != "" {
			state = "error"
			detail = code
		} else if effectiveState, _ := provider["effective_state"].(string); strings.HasPrefix(effectiveState, "blocked_") {
			state = "warn"
			if blockCode, _ := provider["block_code"].(string); blockCode != "" {
				detail += "; " + blockCode
			}
		} else if providerState, _ := provider["provider_state"].(string); providerState == memoryErrProviderUnavail {
			state = "warn"
			detail += "; " + providerState
		}
		add("memory_provider_egress", state, detail)
	}
	add("memory_schema_capabilities", "ok", fmt.Sprintf("compiled support: manifest %d, overlay %d, FTS %s, vector %d, abstract %d, jobs %d", brainManifestSchemaVersion, historyShortTermVersion, historyFTSSchema, memoryVectorSchema, abstractSchemaVersion, memoryJobSchemaVersion))
	schemaState := "error"
	switch snapshot.ManifestHealth.State {
	case "current":
		schemaState = "ok"
	case "absent":
		schemaState = "warn"
	}
	schemaDetail := fmt.Sprintf("manifest observed state %s, schema %d, compiled support %d", snapshot.ManifestHealth.State, snapshot.ManifestHealth.SchemaVersion, snapshot.ManifestHealth.SupportedSchemaVersion)
	if snapshot.ManifestHealth.ErrorCode != "" {
		schemaDetail += "; " + snapshot.ManifestHealth.ErrorCode
	}
	if schemas, ok := snapshot.Payload["schemas"].(map[string]any); ok {
		if observed, ok := schemas["observed_health"].(map[string]any); ok {
			observedErrors := 0
			observedWarnings := 0
			errorCodes := map[string]bool{}
			for _, value := range observed {
				if health, ok := value.(map[string]any); ok {
					state := normalizeMemoryObservedState(memoryObservedStateValue(health["state"]))
					switch state {
					case "corrupt", "unsafe", "unsupported", "unavailable":
						observedErrors++
					case "stale", "migration_required", "degraded":
						observedWarnings++
					}
					code, _ := health["error_code"].(string)
					switch code {
					case memoryErrUnsupportedVersion, memoryErrStateCorrupt, memoryErrStateUnsafe, memoryErrMigrationRequired:
						errorCodes[code] = true
					}
				}
			}
			if observedErrors > 0 {
				schemaState = "error"
				schemaDetail += fmt.Sprintf("; %d observed schema health error(s)", observedErrors)
			} else if observedWarnings > 0 && schemaState == "ok" {
				schemaState = "warn"
				schemaDetail += fmt.Sprintf("; %d stale or migration-required schema observation(s)", observedWarnings)
			}
			if len(errorCodes) > 0 {
				codes := make([]string, 0, len(errorCodes))
				for code := range errorCodes {
					codes = append(codes, code)
				}
				sort.Strings(codes)
				schemaDetail += "; " + strings.Join(codes, ", ")
			}
		}
	}
	add("memory_schemas", schemaState, schemaDetail)
	for _, artifact := range []struct {
		name string
		key  string
	}{
		{name: "memory_overlay", key: "overlay"},
		{name: "memory_fts", key: "fts"},
		{name: "memory_jobs", key: "job_inventory"},
		{name: "memory_vectors", key: "vector_progress"},
	} {
		if health, ok := snapshot.Payload[artifact.key].(map[string]any); ok {
			state, _ := health["state"].(string)
			normalized := normalizeMemoryObservedState(state)
			checkState := "ok"
			switch normalized {
			case "corrupt", "unsafe", "unsupported", "unavailable":
				checkState = "error"
			case "stale", "migration_required", "pending", "degraded":
				checkState = "warn"
			}
			detail := normalized
			if version, ok := health["schema_version"]; ok {
				detail += fmt.Sprintf("; observed schema %v", version)
			}
			if supported, ok := health["supported_schema_version"]; ok {
				detail += fmt.Sprintf("; supported schema %v", supported)
			}
			if code, _ := health["error_code"].(string); code != "" {
				detail += "; " + code
			}
			add(artifact.name, checkState, detail)
		}
	}
	if migrations, ok := snapshot.Payload["migrations"].([]memoryMigrationFinding); ok && len(migrations) > 0 {
		state := "warn"
		for _, finding := range migrations {
			if finding.State == memoryErrStateUnsafe || finding.State == memoryErrStateCorrupt {
				state = "error"
				break
			}
		}
		add("memory_migrations", state, fmt.Sprintf("%d derived-state finding(s); inspect with memory migrate dry-run", len(migrations)))
	} else {
		add("memory_migrations", "ok", "no derived-state migration findings")
	}
	if logHealth, ok := snapshot.Payload["worker_log"].(map[string]any); ok {
		state := "ok"
		if code, _ := logHealth["error_code"].(string); code != "" {
			state = "error"
		}
		add("memory_worker_log", state, fmt.Sprintf("%v (bounded to %v bytes)", logHealth["path"], logHealth["max_bytes"]))
	}
	if sessions, ok := snapshot.Payload["canonical_sessions"].(map[string]any); ok {
		stateName, _ := sessions["state"].(string)
		state := "ok"
		if stateName != "available" {
			state = "warn"
		}
		if code, _ := sessions["error_code"].(string); code != "" {
			state = "error"
			stateName += "; " + code
		}
		add("memory_sessions", state, fmt.Sprintf("%s; %v canonical session(s)", stateName, sessions["count"]))
	}
	if reconciliation, ok := snapshot.Payload["reconciliation"].(map[string]any); ok {
		stateName, _ := reconciliation["state"].(string)
		state := "warn"
		if current, _ := reconciliation["current"].(bool); current {
			state = "ok"
		}
		if code, _ := reconciliation["error_code"].(string); code != "" {
			state = "error"
			stateName += "; " + code
		}
		detail := stateName
		if at, ok := reconciliation["last_successful_at"]; ok {
			detail += fmt.Sprintf("; last successful %v", at)
		}
		add("memory_reconciliation", state, detail)
	}
	return checks
}

// brainDoctorChecks walks the capture-to-recall chain for one brain:
// capture (exported sessions), history index health + freshness against the
// current session set, the conversation projection and its vector identity,
// the derived FTS index, and the write lock. Every failure is a state, not an
// error; doctor's job is to explain, never to crash on a broken brain.
func brainDoctorChecks(ctx context.Context, opts Options, target string) []doctorCheckResult {
	checks, _ := brainDoctorReadOnlyReport(ctx, opts, target)
	return checks
}

// brainDoctorReadOnlyReport resolves the Brain and samples its operational
// state exactly once. Doctor reuses the returned payload and checks so a
// changing heartbeat or filesystem cannot produce a self-contradictory report.
func brainDoctorReadOnlyReport(ctx context.Context, opts Options, target string) ([]doctorCheckResult, map[string]any) {
	var checks []doctorCheckResult
	var payload map[string]any
	add := func(name, state, detail string) {
		checks = append(checks, doctorCheckResult{Name: name, State: state, Detail: detail})
	}
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil || !local {
		add("brain", "warn", "not a local repository; brain checks skipped")
		return checks, payload
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		add("brain", "error", "brain storage unavailable: "+err.Error())
		return checks, payload
	}
	brainDir := storage.BrainDir
	snapshot := memoryReadOnlyHealth(brainDir, opts.Now().UTC())
	payload = snapshot.Payload
	checks = append(checks, memoryDoctorChecks(snapshot)...)
	manifest := snapshot.Manifest
	switch snapshot.ManifestHealth.State {
	case "current":
		add("manifest", "ok", brainDir)
	case "absent":
		add("manifest", "warn", "absent; run `entire brain refresh`")
		return checks, payload
	default:
		add("manifest", "error", fmt.Sprintf("%s: %s", snapshot.ManifestHealth.ErrorCode, snapshot.ManifestHealth.RecommendedAction))
		return checks, payload
	}

	// Capture: exported sessions are the canonical input to every projection.
	if manifest.Sources == nil || manifest.Sources.Sessions == nil || len(manifest.Sources.Sessions.Sessions) == 0 {
		add("capture", "warn", "no exported sessions; run `entire brain refresh` (sessions stage)")
	} else {
		sessions := manifest.Sources.Sessions.Sessions
		latest := time.Time{}
		for _, session := range sessions {
			if session.CreatedAt.After(latest) {
				latest = session.CreatedAt
			}
		}
		detail := fmt.Sprintf("%d exported sessions", len(sessions))
		if !latest.IsZero() {
			detail += ", latest " + latest.UTC().Format(time.RFC3339)
		}
		add("capture", "ok", detail)
	}

	// History index health and conversation freshness: the projection is fresh
	// only when it was built from the CURRENT session set (fingerprint match);
	// a semantic-only refresh must never claim conversation freshness.
	if manifest.Sources == nil || manifest.Sources.History == nil {
		add("history_index", "warn", "no history index; run `entire brain refresh`")
		return checks, payload
	}
	history := manifest.Sources.History
	index, err := loadBrainHistoryIndex(brainDir, history)
	if err != nil {
		add("history_index", "error", "declared but unreadable: "+err.Error())
		return checks, payload
	}
	add("history_index", "ok", fmt.Sprintf("%d records (generated %s)", len(index.Records), history.GeneratedAt.UTC().Format(time.RFC3339)))
	if history.IndexDigest == "" {
		add("history_integrity", "warn", "index predates manifest content digests; run `entire brain refresh`")
	} else {
		add("history_integrity", "ok", "index bytes match the manifest content digest")
	}
	current := brainSessionsFingerprint(brainDir)
	shortTerm, shortTermState := loadHistoryShortTermState(brainDir, history)
	// "Covers the gap" is claimable only for a CURRENT overlay built against
	// the exact current source fingerprint with nothing failed, dropped, or
	// truncated. Anything less is at best partial coverage.
	shortTermCurrentForSources := shortTermState == shortTermStateCurrent && shortTerm.SessionsFingerprint == current
	shortTermCovers := shortTermCurrentForSources && len(shortTerm.Files) > 0
	switch {
	case history.SessionsFingerprint == "":
		add("history_freshness", "warn", "index predates fingerprinting; run `entire brain refresh`")
	case current == history.SessionsFingerprint:
		add("history_freshness", "ok", "history and conversation projections were built from the current exported sessions")
	case shortTermCovers && shortTerm.complete():
		add("history_freshness", "ok", fmt.Sprintf("long-term index is behind, but short-term memory covers the gap (%d changed transcripts; consolidation pending via `entire brain refresh`)", len(shortTerm.Files)))
	case shortTermCurrentForSources && !shortTerm.complete():
		add("history_freshness", "warn", fmt.Sprintf("short-term memory covers the gap only partially (%s); run `entire brain refresh` to consolidate", shortTermIncompleteness(shortTerm)))
	case shortTermState == shortTermStateCorrupt:
		add("history_freshness", "warn", "exported sessions changed and the short-term overlay is corrupt; run `entire brain refresh delta` to rebuild it or `entire brain refresh` to consolidate")
	case shortTermState == shortTermStateUnsupported:
		add("history_freshness", "warn", "exported sessions changed and the short-term overlay uses an unsupported version; run `entire brain refresh delta` to rebuild it or `entire brain refresh` to consolidate")
	default:
		add("history_freshness", "warn", "exported sessions changed since the last index build; run `entire brain refresh delta` for immediate freshness or `entire brain refresh` to consolidate (a missed session-end hook is repaired the same way)")
	}
	switch shortTermState {
	case shortTermStateCorrupt:
		add("short_term_memory", "warn", "overlay unreadable or invalid (corrupt); rebuild with `entire brain refresh delta`")
	case shortTermStateUnsupported:
		add("short_term_memory", "warn", "overlay version unsupported by this binary; rebuild with `entire brain refresh delta`")
	case shortTermStateCurrent:
		if len(shortTerm.Files) == 0 && shortTerm.complete() {
			break
		}
		records := 0
		for _, entry := range shortTerm.Files {
			records += len(entry.Records)
		}
		detail := fmt.Sprintf("%d records from %d changed transcripts (built %s)", records, len(shortTerm.Files), shortTerm.GeneratedAt.UTC().Format(time.RFC3339))
		state := "ok"
		if !shortTerm.complete() {
			state = "warn"
			detail += "; " + shortTermIncompleteness(shortTerm)
		}
		add("short_term_memory", state, detail)
	}

	// Conversation projection + vector identity.
	conversation := buildConversationStatus(brainDir, manifest)
	if conversation == nil {
		add("conversation", "warn", "no conversation projection")
	} else {
		state := "ok"
		detail := fmt.Sprintf("%d exchanges (%d incomplete), vectors %s", conversation.Exchanges, conversation.IncompleteExchanges, conversation.VectorState)
		if conversation.VectorState == "current" {
			detail += fmt.Sprintf(" (%d, %s)", conversation.Vectors, conversation.VectorModelID)
		}
		if conversation.Exchanges == 0 && manifest.Sources.Sessions != nil && len(manifest.Sources.Sessions.Sessions) > 0 {
			state = "warn"
			detail += "; sessions exist but produced no exchanges"
		}
		add("conversation", state, detail)
	}

	// Derived FTS index: an optimization, so absent/stale is a warn (it
	// rebuilds lazily on the next query), never an error.
	if db, ferr := openHistoryFTSIfFresh(brainDir, index); ferr == nil && db != nil {
		_ = db.Close()
		add("history_fts", "ok", "BM25 index fresh")
	} else {
		add("history_fts", "warn", "BM25 index absent or stale; it rebuilds on the next query")
	}

	return checks, payload
}

// --- stats ---

type brainStatsReport struct {
	GeneratedAt  time.Time               `json:"generated_at"`
	RepoKey      string                  `json:"repo_key,omitempty"`
	Sessions     brainStatsSessions      `json:"sessions"`
	History      brainStatsHistory       `json:"history"`
	Conversation *brainStatsConversation `json:"conversation,omitempty"`
	// ShortTerm reports the short-term memory overlay (changed transcripts
	// indexed since the last consolidation), when present.
	ShortTerm *brainStatsShortTerm `json:"short_term,omitempty"`
}

type brainStatsShortTerm struct {
	Files       int    `json:"files"`
	Records     int    `json:"records"`
	Exchanges   int    `json:"exchanges"`
	GeneratedAt string `json:"generated_at"`
	Truncated   bool   `json:"truncated,omitempty"`
	// State is the typed overlay load state; FailedFiles are the
	// transcripts the last delta could not scan.
	State       string   `json:"state,omitempty"`
	FailedFiles []string `json:"failed_files,omitempty"`
}

type brainStatsSessions struct {
	Count    int            `json:"count"`
	ByBranch map[string]int `json:"by_branch,omitempty"`
	ByAgent  map[string]int `json:"by_agent,omitempty"`
	Oldest   string         `json:"oldest,omitempty"`
	Latest   string         `json:"latest,omitempty"`
}

type brainStatsHistory struct {
	Records     int            `json:"records"`
	ByKind      map[string]int `json:"by_kind,omitempty"`
	GeneratedAt string         `json:"generated_at,omitempty"`
	// Index versions: bumping either invalidates/rebuilds the derived layer.
	ScanCacheVersion int    `json:"scan_cache_version"`
	FTSSchema        string `json:"fts_schema"`
}

type brainStatsConversation struct {
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

func newStatsCommand(opts Options) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Report brain counts and ranges by branch, agent, source, completion state, and index version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBrainStats(cmd.Context(), cmd, opts, jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}

func runBrainStats(ctx context.Context, cmd *cobra.Command, opts Options, jsonOut bool) error {
	target := agentSurfaceTarget(opts, nil)
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return err
	}
	if !local {
		return fmt.Errorf("stats requires a local repository path: %s", target)
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}
	report, err := buildBrainStatsReport(storage.BrainDir, opts.Now().UTC())
	if err != nil {
		return err
	}
	if jsonOut {
		return writeJSON(cmd, report)
	}
	return writeText(cmd, func(out io.Writer) { renderBrainStats(out, report) })
}

func buildBrainStatsReport(brainDir string, now time.Time) (brainStatsReport, error) {
	report := brainStatsReport{GeneratedAt: now}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		return report, err
	}
	report.RepoKey = manifest.RepoKey
	if manifest.Sources != nil && manifest.Sources.Sessions != nil {
		sessions := manifest.Sources.Sessions.Sessions
		stats := brainStatsSessions{Count: len(sessions), ByBranch: map[string]int{}, ByAgent: map[string]int{}}
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
	report.History = brainStatsHistory{ScanCacheVersion: historyScanCacheVersion, FTSSchema: historyFTSSchema}
	if manifest.Sources == nil || manifest.Sources.History == nil {
		return report, nil
	}
	history := manifest.Sources.History
	report.History.GeneratedAt = history.GeneratedAt.UTC().Format(time.RFC3339)
	index, err := loadBrainHistoryIndex(brainDir, history)
	if err != nil {
		return report, fmt.Errorf("load history index: %w", err)
	}
	report.History.Records = len(index.Records)
	report.History.ByKind = map[string]int{}
	conversation := brainStatsConversation{
		IncompleteExchanges: history.IncompleteExchanges,
		ByBranch:            map[string]int{},
		ByAgent:             map[string]int{},
	}
	var oldest, latest time.Time
	for _, record := range index.Records {
		report.History.ByKind[record.Kind]++
		if record.Kind != conversationKind {
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
	if vectorStatus := buildConversationStatus(brainDir, manifest); vectorStatus != nil {
		conversation.VectorState = vectorStatus.VectorState
		conversation.VectorModelID = vectorStatus.VectorModelID
		conversation.Vectors = vectorStatus.Vectors
	}
	report.Conversation = &conversation
	if shortTerm, state := loadHistoryShortTermState(brainDir, history); state != shortTermStateAbsent && (len(shortTerm.Files) > 0 || !shortTerm.complete() || state != shortTermStateCurrent) {
		stats := brainStatsShortTerm{
			Files:       len(shortTerm.Files),
			GeneratedAt: shortTerm.GeneratedAt.UTC().Format(time.RFC3339),
			Truncated:   shortTerm.Truncated,
			State:       state,
			FailedFiles: shortTerm.FailedFiles,
		}
		for _, entry := range shortTerm.Files {
			stats.Records += len(entry.Records)
			for _, record := range entry.Records {
				if record.Kind == conversationKind {
					stats.Exchanges++
				}
			}
		}
		report.ShortTerm = &stats
	}
	return report, nil
}

func renderBrainStats(out io.Writer, report brainStatsReport) {
	fmt.Fprintf(out, "repo: %s\n", valueOrUnset(report.RepoKey))
	fmt.Fprintf(out, "sessions: %d", report.Sessions.Count)
	if report.Sessions.Oldest != "" {
		fmt.Fprintf(out, " (%s .. %s)", report.Sessions.Oldest, report.Sessions.Latest)
	}
	fmt.Fprintln(out)
	renderStatsCounts(out, "  by branch", report.Sessions.ByBranch)
	renderStatsCounts(out, "  by agent", report.Sessions.ByAgent)
	fmt.Fprintf(out, "history: %d records (scan cache v%d, fts schema %s)\n", report.History.Records, report.History.ScanCacheVersion, report.History.FTSSchema)
	renderStatsCounts(out, "  by kind", report.History.ByKind)
	if c := report.Conversation; c != nil {
		fmt.Fprintf(out, "conversation: %d exchanges (%d incomplete, %d range-incomplete, %d degraded-identity, %d truncated-projection)\n",
			c.Exchanges, c.IncompleteExchanges, c.RangeIncomplete, c.IdentityDegraded, c.ProjectionTruncated)
		if c.OldestCreatedAt != "" {
			fmt.Fprintf(out, "  sessions span %s .. %s\n", c.OldestCreatedAt, c.LatestCreatedAt)
		}
		renderStatsCounts(out, "  by branch", c.ByBranch)
		renderStatsCounts(out, "  by agent", c.ByAgent)
		line := "  vectors: " + c.VectorState
		if c.VectorState == "current" {
			line += fmt.Sprintf(" (%d, %s)", c.Vectors, c.VectorModelID)
		}
		fmt.Fprintln(out, line)
	}
	if s := report.ShortTerm; s != nil {
		line := fmt.Sprintf("short-term memory: %d records (%d exchanges) from %d changed transcripts (built %s)", s.Records, s.Exchanges, s.Files, s.GeneratedAt)
		if s.State != "" && s.State != shortTermStateCurrent {
			line += "; state " + s.State
		}
		if s.Truncated {
			line += "; buffer full, consolidate with `entire brain refresh`"
		}
		if len(s.FailedFiles) > 0 {
			line += fmt.Sprintf("; %d transcripts failed to scan", len(s.FailedFiles))
		}
		fmt.Fprintln(out, line)
	}
}

func renderStatsCounts(out io.Writer, label string, counts map[string]int) {
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
