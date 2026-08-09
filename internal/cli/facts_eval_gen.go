package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// Query-type strata. Metrics are reported per stratum because the retrieval
// levers help different kinds of query: locus helps "code", scope helps
// "convention", and the base ranker carries "concept"/"howto".
const (
	queryTypeCode       = "code"       // names a symbol/file/ref
	queryTypeConvention = "convention" // about how work is done (cross-cutting)
	queryTypeHowto      = "howto"      // where/how do I change X
	queryTypeConcept    = "concept"    // natural-language conceptual question

	evalGenSourceFacts    = "facts"
	evalGenSourceSessions = "sessions"
)

func newFactsEvalGenCommand(opts Options) *cobra.Command {
	var (
		out          string
		limit        int
		branch       string
		minFacts     int
		maxFacts     int
		refine       bool
		agent        string
		model        string
		agentCommand []string
		judgeCache   string
		source       string
	)
	cmd := &cobra.Command{
		Use:   "eval-gen",
		Short: "Generate a silver-labeled eval task set from the brain's own sessions",
		Long: `eval-gen turns the brain into a self-labeled benchmark: each captured session
	becomes a task (its opening user request), and the facts whose provenance points
	back to that session are provenance/silver labels. These labels are useful for
	recall-oriented measurement, but hand labels or --refine are still required
	before precision/recall release claims. Each task is tagged with a query-type
	stratum (code/convention/howto/concept). Emits a tasks.json consumable by
	'facts eval --tasks'.

	--source sessions emits tasks directly from exported sessions, including sessions
	with zero distilled facts. Those tasks have source anchors but no relevance labels,
	so use --judge or human labels before making relevance claims.

	--refine judge-filters each task's provenance label set with the agent, keeping
only facts genuinely relevant to the request — trading the recall-oriented "every
session fact" labels for precision-clean ones (verdicts cached for reuse).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			repoDir, brainDir, _, err := resolveFactsTarget(cmd.Context(), opts, agentSurfaceTarget(opts, nil), branch)
			if err != nil {
				return err
			}
			manifest, err := loadBrainManifest(brainDir)
			if err != nil {
				return err
			}
			if manifest.Sources == nil || manifest.Sources.Sessions == nil {
				return fmt.Errorf("no exported sessions; run `entire brain refresh` first")
			}
			var tasks []evalTask
			switch strings.TrimSpace(source) {
			case "", evalGenSourceFacts:
				tasks, err = generateEvalTasksContext(cmd.Context(), brainDir, manifest, branch, minFacts, maxFacts, limit)
			case evalGenSourceSessions:
				if refine {
					return fmt.Errorf("--refine requires --source facts; session-derived tasks have no fact labels to refine")
				}
				tasks, err = generateSessionEvalTasksContext(cmd.Context(), brainDir, manifest, branch, limit)
			default:
				return fmt.Errorf("--source must be facts or sessions")
			}
			if err != nil {
				return err
			}
			if refine {
				tasks, err = refineEvalTaskLabels(cmd.Context(), opts, brainDir, repoDir, tasks, agent, model, agentCommand, judgeCache)
				if err != nil {
					return err
				}
			}
			data, err := json.MarshalIndent(tasks, "", "  ")
			if err != nil {
				return err
			}
			data = append(data, '\n')
			if strings.TrimSpace(out) == "" {
				_, err = cmd.OutOrStdout().Write(data)
				return err
			}
			if err := writeFileAtomic(out, data, 0o600); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "wrote %d tasks to %s\n", len(tasks), out)
			return nil
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "Write tasks JSON to this file (default: stdout)")
	cmd.Flags().IntVar(&limit, "limit", 0, "Cap the number of tasks, sampled evenly across strata (0 = all)")
	cmd.Flags().StringVar(&branch, "branch", "", "Limit to one branch (default: all branches with facts)")
	cmd.Flags().IntVar(&minFacts, "min-facts", 2, "Minimum relevant facts for a session to become a task")
	cmd.Flags().IntVar(&maxFacts, "max-facts", 30, "Skip broad sessions with more relevant facts than this (diffuse targets); 0 = no cap")
	cmd.Flags().StringVar(&source, "source", evalGenSourceFacts, "Task source: facts (provenance-labeled) or sessions (source-anchored, unlabeled)")
	cmd.Flags().BoolVar(&refine, "refine", false, "Judge-filter provenance labels to the genuinely-relevant subset (agent-required)")
	cmd.Flags().StringVar(&agent, "agent", "auto", "Agent for --refine: auto, codex, claude-code, ollama, or command")
	cmd.Flags().StringVar(&model, "model", "", "Model for codex/claude-code/ollama refine calls")
	cmd.Flags().StringArrayVar(&agentCommand, "agent-command", nil, "Agent command argv for --agent command")
	cmd.Flags().StringVar(&judgeCache, "judge-cache", "", "Persist/reuse --refine verdicts at this path")
	return cmd
}

// refineEvalTaskLabels judge-filters each task's provenance-labeled facts to the
// subset the agent deems genuinely relevant to the request, re-tags the query
// stratum from the surviving facts, and drops tasks left with no relevant facts.
// Verdicts are cached so re-runs are cheap and deterministic.
func refineEvalTaskLabels(ctx context.Context, opts Options, brainDir, repoDir string, tasks []evalTask, agent, model string, agentCommand []string, cachePath string) ([]evalTask, error) {
	resolved := agent
	if resolved == "auto" {
		resolved = defaultRefreshAgent(ctx, opts.Runner, repoDir)
	}
	judgeArgs, err := distillAgentCommandArgs(resolved, agentCommand, judgePrompt())
	if err != nil {
		return nil, fmt.Errorf("refine agent: %w", err)
	}
	judgeArgs = injectAgentModel(judgeArgs, resolved, model)
	byBranch, err := loadAllFactBranches(brainDir)
	if err != nil {
		return nil, err
	}
	idToFact := map[string]factRecord{}
	for _, recs := range byBranch {
		for _, r := range recs {
			idToFact[r.ID] = r
		}
	}
	cache := loadJudgeCache(cachePath)
	if err := cache.validateLoaded(); err != nil {
		return nil, err
	}
	run := defaultDistillAgentRunner(resolved)
	out := make([]evalTask, 0, len(tasks))
	for _, t := range tasks {
		facts := make([]factRecord, 0, len(t.Relevant))
		for _, id := range t.Relevant {
			if f, ok := idToFact[id]; ok {
				facts = append(facts, f)
			}
		}
		if len(facts) == 0 {
			continue
		}
		relevant, err := judgeRelevance(ctx, run, repoDir, judgeArgs, t, facts, cache)
		if err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(relevant))
		kept := make([]factRecord, 0, len(relevant))
		for _, f := range facts {
			if _, ok := relevant[f.ID]; ok {
				ids = append(ids, f.ID)
				kept = append(kept, f)
			}
		}
		if len(ids) == 0 {
			continue // no genuinely-relevant facts: not a useful target
		}
		sort.Strings(ids)
		t.Relevant = ids
		t.LabelSource = evalLabelSourceJudgeRefined
		t.QueryType = classifyQueryType(t.Task, kept)
		out = append(out, t)
	}
	if err := cache.save(); err != nil {
		return nil, fmt.Errorf("save refine cache: %w", err)
	}
	return out, nil
}

// generateEvalTasks builds provenance-labeled tasks: one per session that has
// at least minFacts facts and a recoverable opening request. branch filters to
// a single branch; limit caps the set with even sampling across strata.
func generateEvalTasks(brainDir string, manifest *exportManifest, branch string, minFacts, maxFacts, limit int) ([]evalTask, error) {
	return generateEvalTasksContext(context.Background(), brainDir, manifest, branch, minFacts, maxFacts, limit)
}

func generateEvalTasksContext(ctx context.Context, brainDir string, manifest *exportManifest, branch string, minFacts, maxFacts, limit int) ([]evalTask, error) {
	byBranch, err := loadAllFactBranches(brainDir)
	if err != nil {
		return nil, err
	}
	// branch+session+transcript -> the facts that cite it (deduped per
	// session source). Older anchors may lack a transcript; those stay in the
	// branch+session fallback bucket and are used only when no transcript-scoped
	// labels exist for a manifest session.
	sessionFacts := map[string][]factRecord{}
	for b, recs := range byBranch {
		if branch != "" && b != branch {
			continue
		}
		for _, r := range recs {
			seen := map[string]struct{}{}
			for _, a := range r.Provenance {
				if a.SessionID == "" {
					continue
				}
				key := evalSessionFactKey(b, a.SessionID, a.Transcript)
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				sessionFacts[key] = append(sessionFacts[key], r)
			}
		}
	}

	var tasks []evalTask
	idCollisions := evalSessionIDCollisions(manifest, branch)
	for _, s := range manifest.Sources.Sessions.Sessions {
		resolvedBranch := resolveDistillBranch(manifest, s)
		if branch != "" && resolvedBranch != branch {
			continue
		}
		facts := evalFactsForSession(sessionFacts, resolvedBranch, s)
		if len(facts) < minFacts {
			continue
		}
		if maxFacts > 0 && len(uniqueFactIDs(facts)) > maxFacts {
			continue // broad session: too many facts to be a focused retrieval target
		}
		data, readErr := readCanonicalHistoryTranscript(ctx, brainDir, s.TranscriptPath)
		if readErr != nil {
			continue
		}
		content := string(data)
		request := firstUserRequest(content)
		if request == "" {
			continue
		}
		tasks = append(tasks, evalTask{
			ID:                   evalTaskIDForSession(s.SessionID, resolvedBranch, s.TranscriptPath, idCollisions),
			Task:                 request,
			Branch:               resolvedBranch,
			QueryType:            classifyQueryType(request, facts),
			Relevant:             uniqueFactIDs(facts),
			LabelSource:          evalLabelSourceProvenanceSilver,
			SourceSessionID:      s.SessionID,
			SourceTranscriptPath: filepath.ToSlash(s.TranscriptPath),
			SourceLines:          sourceLinesForSessionFacts(facts, s),
		})
	}
	tasks = dedupeEvalTasks(tasks)
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })
	if limit > 0 && len(tasks) > limit {
		tasks = sampleAcrossStrata(tasks, limit)
	}
	return tasks, nil
}

func evalSessionFactKey(branch, sessionID, transcript string) string {
	return strings.TrimSpace(branch) + "\x00" + strings.TrimSpace(sessionID) + "\x00" + filepath.ToSlash(strings.TrimSpace(transcript))
}

func evalFactsForSession(sessionFacts map[string][]factRecord, branch string, session exportSession) []factRecord {
	exactKey := evalSessionFactKey(branch, session.SessionID, session.TranscriptPath)
	exact := dedupeFactRecords(sessionFacts[exactKey])
	if len(exact) > 0 || strings.TrimSpace(session.TranscriptPath) == "" {
		return exact
	}
	return dedupeFactRecords(sessionFacts[evalSessionFactKey(branch, session.SessionID, "")])
}

func dedupeFactRecords(facts []factRecord) []factRecord {
	if len(facts) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]factRecord, 0, len(facts))
	for _, fact := range facts {
		if _, ok := seen[fact.ID]; ok {
			continue
		}
		seen[fact.ID] = struct{}{}
		out = append(out, fact)
	}
	return out
}

func evalSessionIDCollisions(manifest *exportManifest, branch string) map[string]bool {
	collisions := map[string]bool{}
	if manifest == nil || manifest.Sources == nil || manifest.Sources.Sessions == nil {
		return collisions
	}
	counts := map[string]int{}
	for _, s := range manifest.Sources.Sessions.Sessions {
		resolvedBranch := resolveDistillBranch(manifest, s)
		if branch != "" && resolvedBranch != branch {
			continue
		}
		counts[shortSessionID(s.SessionID)]++
	}
	for id, count := range counts {
		if count > 1 {
			collisions[id] = true
		}
	}
	return collisions
}

func evalTaskIDForSession(sessionID, branch, transcriptPath string, collisions map[string]bool) string {
	id := shortSessionID(sessionID)
	if !collisions[id] {
		return id
	}
	cleanBranch := strings.NewReplacer("/", "_", "\\", "_", " ", "_", "\x00", "_").Replace(strings.TrimSpace(branch))
	if cleanBranch == "" {
		cleanBranch = "branch"
	}
	return cleanBranch + ":" + id + ":" + evalTaskIDDisambiguator(sessionID, branch, transcriptPath)
}

func evalTaskIDDisambiguator(sessionID, branch, transcriptPath string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		strings.TrimSpace(branch),
		strings.TrimSpace(sessionID),
		filepath.ToSlash(strings.TrimSpace(transcriptPath)),
	}, "\x00")))
	return hex.EncodeToString(sum[:4])
}

func sourceLinesForSessionFacts(facts []factRecord, session exportSession) []int {
	sessionID := strings.TrimSpace(session.SessionID)
	transcript := filepath.ToSlash(strings.TrimSpace(session.TranscriptPath))
	seen := map[int]struct{}{}
	for _, fact := range facts {
		for _, anchor := range fact.Provenance {
			if anchor.Line <= 0 {
				continue
			}
			anchorTranscript := filepath.ToSlash(strings.TrimSpace(anchor.Transcript))
			switch {
			case transcript != "" && anchorTranscript != "" && anchorTranscript == transcript:
			case transcript == "" && sessionID != "" && strings.TrimSpace(anchor.SessionID) == sessionID:
			case transcript != "" && anchorTranscript == "" && sessionID != "" && strings.TrimSpace(anchor.SessionID) == sessionID:
			default:
				continue
			}
			seen[anchor.Line] = struct{}{}
		}
	}
	lines := make([]int, 0, len(seen))
	for line := range seen {
		lines = append(lines, line)
	}
	sort.Ints(lines)
	return lines
}

func generateSessionEvalTasks(brainDir string, manifest *exportManifest, branch string, limit int) ([]evalTask, error) {
	return generateSessionEvalTasksContext(context.Background(), brainDir, manifest, branch, limit)
}

func generateSessionEvalTasksContext(ctx context.Context, brainDir string, manifest *exportManifest, branch string, limit int) ([]evalTask, error) {
	if manifest == nil || manifest.Sources == nil || manifest.Sources.Sessions == nil {
		return nil, fmt.Errorf("no exported sessions; run `entire brain refresh` first")
	}
	var tasks []evalTask
	idCollisions := evalSessionIDCollisions(manifest, branch)
	for _, s := range manifest.Sources.Sessions.Sessions {
		resolvedBranch := resolveDistillBranch(manifest, s)
		if branch != "" && resolvedBranch != branch {
			continue
		}
		data, readErr := readCanonicalHistoryTranscript(ctx, brainDir, s.TranscriptPath)
		if readErr != nil {
			continue
		}
		content := string(data)
		request := firstUserRequest(content)
		if request == "" {
			continue
		}
		tasks = append(tasks, evalTask{
			ID:                   evalTaskIDForSession(s.SessionID, resolvedBranch, s.TranscriptPath, idCollisions),
			Task:                 request,
			Branch:               resolvedBranch,
			QueryType:            classifyQueryType(request, nil),
			Relevant:             []string{},
			SourceSessionID:      s.SessionID,
			SourceTranscriptPath: filepath.ToSlash(s.TranscriptPath),
		})
	}
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })
	if limit > 0 && len(tasks) > limit {
		tasks = sampleAcrossStrata(tasks, limit)
	}
	return tasks, nil
}

// dedupeEvalTasks collapses unanchored tasks with the same normalized request
// (repeated slash-command or templated sessions, e.g. "review the current
// branch", appear many times), keeping the variant with the most relevant facts
// so the richest labeling survives. Source-anchored generated tasks include the
// source in the dedupe key so two sessions with the same opening request do not
// lose distinct provenance labels.
func dedupeEvalTasks(tasks []evalTask) []evalTask {
	index := map[string]int{}
	var out []evalTask
	for _, t := range tasks {
		key := evalTaskDedupeKey(t)
		if i, ok := index[key]; ok {
			if len(t.Relevant) > len(out[i].Relevant) {
				out[i] = t
			}
			continue
		}
		index[key] = len(out)
		out = append(out, t)
	}
	return out
}

func evalTaskDedupeKey(t evalTask) string {
	key := normalizeTaskText(t.Task)
	if strings.TrimSpace(t.SourceTranscriptPath) == "" && strings.TrimSpace(t.SourceSessionID) == "" {
		return key
	}
	return strings.Join([]string{
		key,
		strings.TrimSpace(t.Branch),
		strings.TrimSpace(t.SourceSessionID),
		filepath.ToSlash(strings.TrimSpace(t.SourceTranscriptPath)),
	}, "\x00")
}

// normalizeTaskText is the dedupe key: lowercased, whitespace-collapsed, and
// truncated so near-identical prompts that differ only in a trailing clause
// collapse together.
func normalizeTaskText(task string) string {
	return truncateString(strings.ToLower(strings.Join(strings.Fields(task), " ")), 160)
}

// sampleAcrossStrata caps tasks to n, taking them round-robin across query
// types so no single stratum dominates the benchmark.
func sampleAcrossStrata(tasks []evalTask, n int) []evalTask {
	buckets := map[string][]evalTask{}
	var order []string
	for _, t := range tasks {
		if _, ok := buckets[t.QueryType]; !ok {
			order = append(order, t.QueryType)
		}
		buckets[t.QueryType] = append(buckets[t.QueryType], t)
	}
	sort.Strings(order)
	var out []evalTask
	for len(out) < n {
		progressed := false
		for _, qt := range order {
			if len(buckets[qt]) == 0 {
				continue
			}
			out = append(out, buckets[qt][0])
			buckets[qt] = buckets[qt][1:]
			progressed = true
			if len(out) >= n {
				break
			}
		}
		if !progressed {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// classifyQueryType assigns a task to a stratum from its text and the scope of
// its relevant facts.
func classifyQueryType(task string, relevant []factRecord) string {
	// A "code" query needs a structural code token (backtick span, file path,
	// dotted pkg.Symbol, or snake_case) — not a bare CamelCase word, which is
	// usually a product name (GitHub, PlanetScale) in an otherwise NL question.
	if hasStructuralCodeToken(task) {
		return queryTypeCode
	}
	cross := 0
	for _, f := range relevant {
		if factScope(f.Paths) == factScopeCrossCutting {
			cross++
		}
	}
	if len(relevant) > 0 && cross*2 >= len(relevant) {
		return queryTypeConvention
	}
	lower := strings.ToLower(strings.TrimSpace(task))
	if hasAnyPrefix(lower, "where ", "how do i", "how should", "how can i", "add ", "implement ", "fix ", "wire ", "make ", "build ", "create ", "update ", "remove ") {
		return queryTypeHowto
	}
	return queryTypeConcept
}

// Structural code signals for the "code" stratum. Each is tight enough to skip
// prose false positives: abbreviations ("e.g.", "i.e."), version numbers
// ("v1.1"), and "and/or" do not match because the dotted/path forms require
// multi-character identifier segments and the path form requires a real
// directory or extension.
var (
	backtickCodePattern = regexp.MustCompile("`[^`]+`")
	pathCodePattern     = regexp.MustCompile(`(?:internal|cmd|pkg|src|lib|apps|packages|docs|templates)/[A-Za-z0-9_./-]+|[A-Za-z0-9_./-]+\.(?:go|ts|tsx|js|jsx|py|rs|md|json|ndjson|ya?ml|toml)\b`)
	dottedCodePattern   = regexp.MustCompile(`\b[a-z][A-Za-z0-9_]{2,}\.[A-Za-z][A-Za-z0-9_]{2,}\b`)
	snakeCodePattern    = regexp.MustCompile(`\b[a-z][a-z0-9]*_[a-z0-9_]*[a-z0-9]\b`)
)

func hasStructuralCodeToken(s string) bool {
	return backtickCodePattern.MatchString(s) || pathCodePattern.MatchString(s) ||
		dottedCodePattern.MatchString(s) || snakeCodePattern.MatchString(s)
}

func uniqueFactIDs(facts []factRecord) []string {
	seen := map[string]struct{}{}
	var ids []string
	for _, f := range facts {
		if _, ok := seen[f.ID]; ok {
			continue
		}
		seen[f.ID] = struct{}{}
		ids = append(ids, f.ID)
	}
	sort.Strings(ids)
	return ids
}

func shortSessionID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// firstUserRequest recovers the first substantive user request from a session
// transcript, skipping injected wrappers (environment context, reminders, tool
// results). Returns "" when none is found. Supports the Codex (response_item /
// event_msg), Claude (user), pi (message) and opencode (document-form)
// transcript shapes.
func firstUserRequest(transcript string) string {
	// Document-form transcripts (e.g. opencode) have no parseable lines; use
	// the shared document parser the distill path uses.
	if messages, ok := parseDocumentConversation(transcript); ok {
		for _, message := range messages {
			// message.Text arrives already whitespace-collapsed from the parser.
			if message.Role != "user" || message.Text == "" || isWrapperRequest(message.Text) {
				continue
			}
			return truncateString(message.Text, 280)
		}
		return ""
	}
	for _, line := range strings.Split(transcript, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(line), &obj) != nil {
			continue
		}
		text := strings.TrimSpace(transcriptUserText(obj))
		if text == "" || isWrapperRequest(text) {
			continue
		}
		return truncateString(strings.Join(strings.Fields(text), " "), 280)
	}
	return ""
}

func transcriptUserText(obj map[string]any) string {
	payload := jsonMap(obj["payload"])
	switch jsonString(obj["type"]) {
	case "response_item":
		if jsonString(payload["type"]) == "message" && jsonString(payload["role"]) == "user" {
			return transcriptContentText(payload["content"])
		}
	case "event_msg":
		if jsonString(payload["type"]) == "user_message" {
			return jsonString(payload["message"])
		}
	case "user":
		if msg := jsonMap(obj["message"]); msg != nil {
			return transcriptContentText(msg["content"])
		}
	case "message":
		// pi wraps every turn as {"type":"message","message":{role,content}};
		// keep user turns (same blind spot distillConversationText covers).
		if msg := jsonMap(obj["message"]); jsonString(msg["role"]) == "user" {
			return transcriptContentText(msg["content"])
		}
	}
	return ""
}

func transcriptContentText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, part := range v {
			pm := jsonMap(part)
			if t := firstNonEmptyString(pm["text"], pm["input_text"]); t != "" {
				b.WriteString(t)
				b.WriteByte(' ')
			}
		}
		return b.String()
	}
	return ""
}

// isWrapperRequest reports whether a user-message body is an injected wrapper
// (environment context block, system reminder, tool result, slash-command
// envelope) rather than a real request.
func isWrapperRequest(text string) bool {
	if strings.HasPrefix(text, "<") {
		return true
	}
	lower := strings.ToLower(text)
	return strings.HasPrefix(lower, "caveat:") ||
		strings.HasPrefix(lower, "[request interrupted") ||
		strings.HasPrefix(lower, "command-name") ||
		strings.Contains(text[:min(len(text), 40)], "environment_context")
}
