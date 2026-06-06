package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
)

func newFactsEvalGenCommand(opts Options) *cobra.Command {
	var (
		out        string
		limit      int
		branch     string
		minFacts   int
		maxFacts   int
		refine       bool
		agent        string
		agentCommand []string
		judgeCache   string
	)
	cmd := &cobra.Command{
		Use:   "eval-gen",
		Short: "Generate a labeled eval task set from the brain's own sessions (provenance-labeled)",
		Long: `eval-gen turns the brain into a self-labeled benchmark: each captured session
becomes a task (its opening user request), and the facts whose provenance points
back to that session are the ground-truth relevant set. No agent or hand labels
needed. Each task is tagged with a query-type stratum (code/convention/howto/
concept). Emits a tasks.json consumable by 'facts eval --tasks'.

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
			tasks, err := generateEvalTasks(brainDir, manifest, branch, minFacts, maxFacts, limit)
			if err != nil {
				return err
			}
			if refine {
				tasks, err = refineEvalTaskLabels(cmd.Context(), opts, brainDir, repoDir, tasks, agent, agentCommand, judgeCache)
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
			if err := os.WriteFile(out, data, 0o600); err != nil {
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
	cmd.Flags().BoolVar(&refine, "refine", false, "Judge-filter provenance labels to the genuinely-relevant subset (agent-required)")
	cmd.Flags().StringVar(&agent, "agent", "auto", "Agent for --refine: auto, codex, claude-code, or command")
	cmd.Flags().StringArrayVar(&agentCommand, "agent-command", nil, "Agent command argv for --agent command")
	cmd.Flags().StringVar(&judgeCache, "judge-cache", "", "Persist/reuse --refine verdicts at this path")
	return cmd
}

// refineEvalTaskLabels judge-filters each task's provenance-labeled facts to the
// subset the agent deems genuinely relevant to the request, re-tags the query
// stratum from the surviving facts, and drops tasks left with no relevant facts.
// Verdicts are cached so re-runs are cheap and deterministic.
func refineEvalTaskLabels(ctx context.Context, opts Options, brainDir, repoDir string, tasks []evalTask, agent string, agentCommand []string, cachePath string) ([]evalTask, error) {
	resolved := agent
	if resolved == "auto" {
		resolved = defaultRefreshAgent(ctx, opts.Runner, repoDir)
	}
	judgeArgs, err := distillAgentCommandArgs(resolved, agentCommand, judgePrompt())
	if err != nil {
		return nil, fmt.Errorf("refine agent: %w", err)
	}
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
	run := execDistillAgent
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
	byBranch, err := loadAllFactBranches(brainDir)
	if err != nil {
		return nil, err
	}
	// session id -> the facts that cite it (deduped per session).
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
				if _, ok := seen[a.SessionID]; ok {
					continue
				}
				seen[a.SessionID] = struct{}{}
				sessionFacts[a.SessionID] = append(sessionFacts[a.SessionID], r)
			}
		}
	}

	var tasks []evalTask
	for _, s := range manifest.Sources.Sessions.Sessions {
		if branch != "" && s.Branch != branch {
			continue
		}
		facts := sessionFacts[s.SessionID]
		if len(facts) < minFacts {
			continue
		}
		if maxFacts > 0 && len(uniqueFactIDs(facts)) > maxFacts {
			continue // broad session: too many facts to be a focused retrieval target
		}
		content, readErr := readBrainRelativeFile(brainDir, s.TranscriptPath)
		if readErr != nil {
			continue
		}
		request := firstUserRequest(content)
		if request == "" {
			continue
		}
		tasks = append(tasks, evalTask{
			ID:        shortSessionID(s.SessionID),
			Task:      request,
			Branch:    s.Branch,
			QueryType: classifyQueryType(request, facts),
			Relevant:  uniqueFactIDs(facts),
		})
	}
	tasks = dedupeEvalTasks(tasks)
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })
	if limit > 0 && len(tasks) > limit {
		tasks = sampleAcrossStrata(tasks, limit)
	}
	return tasks, nil
}

// dedupeEvalTasks collapses tasks with the same normalized request (repeated
// slash-command or templated sessions, e.g. "review the current branch", appear
// many times), keeping the variant with the most relevant facts so the richest
// labeling survives. This stops a few templated prompts from dominating the
// benchmark.
func dedupeEvalTasks(tasks []evalTask) []evalTask {
	index := map[string]int{}
	var out []evalTask
	for _, t := range tasks {
		key := normalizeTaskText(t.Task)
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
// event_msg) and Claude (user) transcript shapes.
func firstUserRequest(transcript string) string {
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
