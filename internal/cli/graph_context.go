package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// graphTaskContext is the response of `entire graph context`, the graph
// plugin's delegation surface for task context (COR-2217). The graph owns the
// queryable store; the brief calls this instead of maintaining its own copy of
// the snapshot. Field names mirror the provider's contextResponse contract.
type graphTaskContext struct {
	SchemaVersion string                 `json:"schema_version"`
	Commit        string                 `json:"commit"`
	Tree          string                 `json:"tree"`
	Symbols       []graphContextSymbol   `json:"symbols"`
	Relations     []graphContextRelation `json:"relations"`
	Tests         []graphContextSymbol   `json:"tests"`
	LatencyMS     int64                  `json:"latency_ms"`
	// StoreUnavailable means the provider ran but has no store for the
	// committed tree. The brief degrades to its internal semantic path (or a
	// warning), exactly as it would with the plugin absent.
	StoreUnavailable bool   `json:"store_unavailable"`
	Detail           string `json:"detail"`
	// Stale is present only when the provider answered from an ANCESTOR of the
	// committed tree instead of the tree itself. Its absence means the answer
	// is exact, which is why this field can ship before any graph emits it: an
	// older provider simply never sets it and nothing changes.
	//
	// The agreed contract (graph G1) is that changed_files is mandatory and
	// COMPLETE whenever the block appears — anything the provider could not
	// enumerate (over 5% of files changed, a diff failure, unresolvable
	// renames, a partial-clone missing blob) makes it refuse the ancestor and
	// return an exact miss instead. Brain still verifies that here rather than
	// trusting it, because the whole value of the list is negative-suppression
	// and a silently short list is worse than no list.
	Stale *graphContextStale `json:"stale,omitempty"`
}

// graphContextStale describes an answer served from an ancestor commit.
//
// ChangedFiles is the load-bearing field. An ancestor store has had its
// records for changed files dropped, so a query touching one of those files
// can come back empty while consumers really do exist there. A confident
// "nothing found" over such a file is the failure mode this block exists to
// prevent, and ExcludedRecords is how brain can tell that records really were
// removed rather than the file merely being listed.
type graphContextStale struct {
	AncestorCommit  string   `json:"ancestor_commit"`
	AncestorTree    string   `json:"ancestor_tree"`
	CommitsBehind   int      `json:"commits_behind"`
	ChangedFiles    []string `json:"changed_files"`
	ExcludedRecords int      `json:"excluded_records"`
}

// usable reports whether a stale block can be relied on.
//
// Fail closed: a block that claims records were excluded but names no files is
// internally inconsistent, and so is one with no ancestor identity. Either way
// brain cannot say WHICH answers are incomplete, so it must not present the
// answer as if it could. nil is not an error — it is the exact case.
func (s *graphContextStale) usable() bool {
	if s == nil {
		return true
	}
	if strings.TrimSpace(s.AncestorCommit) == "" || s.CommitsBehind <= 0 {
		return false
	}
	if s.ExcludedRecords > 0 && len(s.ChangedFiles) == 0 {
		return false
	}
	return true
}

// covers reports whether a path is one the ancestor answer had records dropped
// for, so a negative about it cannot be trusted. Matching is exact on the
// provider's repo-relative path, with a directory prefix allowed so a renamed
// or deleted tree still suppresses its children.
func (s *graphContextStale) covers(path string) bool {
	if s == nil {
		return false
	}
	path = strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(path)), "./")
	if path == "" {
		return false
	}
	for _, changed := range s.ChangedFiles {
		changed = strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(changed)), "./")
		if changed == "" {
			continue
		}
		if path == changed || strings.HasPrefix(path, strings.TrimSuffix(changed, "/")+"/") {
			return true
		}
	}
	return false
}

type graphContextSymbol struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	QualifiedName string  `json:"qualified_name"`
	Kind          string  `json:"kind"`
	FilePath      string  `json:"file_path"`
	StartLine     int     `json:"start_line"`
	EndLine       int     `json:"end_line"`
	Signature     string  `json:"signature"`
	Language      string  `json:"language"`
	Score         float64 `json:"score"`
}

type graphContextRelation struct {
	FromID     string  `json:"from_id"`
	ToID       string  `json:"to_id"`
	Type       string  `json:"type"`
	Confidence float64 `json:"confidence"`
}

// graphTaskContextTimeout bounds the delegation subprocess. The command
// answers from an indexed store in well under a second; the margin covers a
// cold binary start and a loaded machine, not a build.
const graphTaskContextTimeout = 15 * time.Second

// fetchGraphTaskContext asks the graph plugin for task context. Any error —
// binary missing, a plugin version without the `context` verb, malformed
// output — reads as "delegation unavailable" and the caller falls back; it is
// never surfaced as a brief failure.
func fetchGraphTaskContext(ctx context.Context, runner CommandRunner, repoDir, graphBinary, task string, limit, testsLimit int) (graphTaskContext, error) {
	if runner == nil {
		return graphTaskContext{}, fmt.Errorf("graph context requires a command runner")
	}
	binary := strings.TrimSpace(graphBinary)
	if binary == "" {
		binary = "entire"
	}
	runCtx, cancel := context.WithTimeout(ctx, graphTaskContextTimeout)
	defer cancel()
	args := []string{"graph", "context", "--repo", repoDir, "--task", task, "--limit", fmt.Sprint(limit), "--tests-limit", fmt.Sprint(testsLimit)}
	// Asked for only when the provider advertises it. The probe is skipped
	// entirely when the budget is zero, so a caller that does not want an
	// ancestor answer pays no subprocess for the question.
	if graphBriefAncestorBudget > 0 && graphContextAllowsAncestor(ctx, runner, repoDir, binary) {
		args = append(args, "--allow-ancestor", fmt.Sprint(graphBriefAncestorBudget))
	}
	ignores, err := semanticProviderIgnoreFiles(repoDir)
	if err != nil {
		return graphTaskContext{}, err
	}
	for _, ignore := range ignores {
		args = append(args, "--ignore-file", ignore)
	}
	stdout, stderr, err := runner.Run(runCtx, repoDir, binary, args...)

	if err != nil {
		detail := strings.TrimSpace(string(stderr))
		if detail == "" {
			detail = strings.TrimSpace(string(stdout))
		}
		if graphBinaryRejectsHostVerb(err) || strings.Contains(detail, `unknown command "graph"`) {
			// Named rather than wrapped: a reader who passed the wrong kind of
			// binary otherwise sees only "code context unavailable".
			return graphTaskContext{}, fmt.Errorf("graph context: %s", graphHostVerbDetail(binary))
		}
		if detail != "" {
			return graphTaskContext{}, fmt.Errorf("graph context: %w: %s", err, firstLine(detail))
		}
		return graphTaskContext{}, fmt.Errorf("graph context: %w", err)
	}
	var response graphTaskContext
	if err := json.Unmarshal(stdout, &response); err != nil {
		return graphTaskContext{}, fmt.Errorf("graph context returned unparseable output: %w", err)
	}
	return response, nil
}

func firstLine(s string) string {
	if index := strings.IndexByte(s, '\n'); index >= 0 {
		return s[:index]
	}
	return s
}

// graphContextSymbolRecords projects provider symbols into the brief's
// semanticRecord shape so every downstream consumer (departed-file filtering,
// caps, likely-file synthesis, rendering) works unchanged.
func graphContextSymbolRecords(symbols []graphContextSymbol) []semanticRecord {
	records := make([]semanticRecord, 0, len(symbols))
	for _, symbol := range symbols {
		records = append(records, semanticRecord{
			RecordType:    "symbol",
			ID:            symbol.ID,
			Kind:          symbol.Kind,
			Name:          symbol.Name,
			QualifiedName: symbol.QualifiedName,
			FilePath:      symbol.FilePath,
			StartLine:     symbol.StartLine,
			EndLine:       symbol.EndLine,
			Signature:     symbol.Signature,
			Language:      symbol.Language,
		})
	}
	return records
}

// graphContextRelationRecords keeps only relations whose endpoints are both
// among the returned context symbols (or external graph endpoints): that is
// the brief packet's relation contract, enforced again by the semantic cap and
// departed-file filtering, and an edge to a symbol the packet does not show is
// an opaque ID the agent cannot use. Test edges are conveyed by the tests
// section instead.
func graphContextRelationRecords(symbols []graphContextSymbol, relations []graphContextRelation) []semanticRecord {
	known := make(map[string]struct{}, len(symbols))
	for _, symbol := range symbols {
		known[symbol.ID] = struct{}{}
	}
	endpointKnown := func(id string) bool {
		if _, ok := known[id]; ok {
			return true
		}
		return strings.HasPrefix(id, "external:")
	}
	records := make([]semanticRecord, 0, len(relations))
	for _, relation := range relations {
		if !endpointKnown(relation.FromID) || !endpointKnown(relation.ToID) {
			continue
		}
		records = append(records, semanticRecord{
			RecordType: "relation",
			FromID:     relation.FromID,
			ToID:       relation.ToID,
			Type:       relation.Type,
			Confidence: relation.Confidence,
		})
	}
	return records
}

func graphContextTestsResult(tests []graphContextSymbol) semanticTestsResult {
	suggestions := make([]semanticTestSuggestion, 0, len(tests))
	for _, record := range graphContextSymbolRecords(tests) {
		suggestions = append(suggestions, semanticTestSuggestion{Symbol: record, Reason: "task match (graph store)"})
	}
	return semanticTestsResult{Roots: []semanticRecord{}, Suggestions: suggestions}
}

// semanticStalenessFor converts a provider stale block into what the brief
// reports. nil in, nil out: an exact answer carries no staleness.
//
// Unreliable is deliberately narrower than ChangedFiles. A reader does not need
// every file the commit range touched; they need the ones this answer actually
// returned records from, because those are the ones where a partial answer
// looks like a complete one. The full changed list stays available for the
// cases where a caller is asking about a file that returned nothing at all.
func semanticStalenessFor(stale *graphContextStale, symbols []graphContextSymbol, tests []graphContextSymbol) *semanticContextStaleness {
	if stale == nil {
		return nil
	}
	out := &semanticContextStaleness{
		AncestorCommit: strings.TrimSpace(stale.AncestorCommit),
		CommitsBehind:  stale.CommitsBehind,
		ChangedFiles:   append([]string(nil), stale.ChangedFiles...),
		// Carried through deliberately: without it an empty Unreliable is
		// indistinguishable from a complete answer.
		ExcludedRecords: stale.ExcludedRecords,
	}
	seen := map[string]struct{}{}
	for _, group := range [][]graphContextSymbol{symbols, tests} {
		for _, symbol := range group {
			path := strings.TrimSpace(symbol.FilePath)
			if path == "" || !stale.covers(path) {
				continue
			}
			if _, ok := seen[path]; ok {
				continue
			}
			seen[path] = struct{}{}
			out.Unreliable = append(out.Unreliable, path)
		}
	}
	sort.Strings(out.Unreliable)
	return out
}

// graphBriefAncestorBudget is how many commits back the brief will accept an
// answer from when the provider has no store for the committed tree.
//
// Three, for two independent reasons that happen to agree: the provider's
// store retention keeps the newest K=3 stores per group, so at most two
// ancestors exist to be served anyway; and the provider's own measurement puts
// staleness of relations touching changed files at 0.05% one commit back and
// 3.5% at five, so three sits well inside the band where positives are sound.
// The provider independently refuses an ancestor whose changed files exceed 5%
// of the index, so this is a ceiling, not a promise.
const graphBriefAncestorBudget = 3

// graphAncestorSupport memoizes the capability probe per graph binary. The
// probe is a subprocess, the answer cannot change under a running brief, and
// the brief asks once per run — but refresh and status share this process.
var graphAncestorSupport sync.Map // binary path -> bool

// graphContextAllowsAncestor reports whether this provider advertises
// `context --allow-ancestor`.
//
// It FAILS CLOSED, and that is the whole design. Passing a flag an older
// provider does not know makes the subprocess exit non-zero, which
// fetchGraphTaskContext reads as "delegation unavailable" — so a wrong answer
// here would turn every brief on an older graph into a silent fallback, which
// is strictly worse than never asking. Any probe failure, any malformed
// report, any absent key therefore means "no".
//
// The key is a flat boolean in the report's existing feature map
// (optional_local_only_features.context_allow_ancestor). Absence means
// unsupported, which is the same rule as the stale block's absence meaning
// exact: one convention in both directions, and no version arithmetic.
func graphContextAllowsAncestor(ctx context.Context, runner CommandRunner, repoDir, graphBinary string) bool {
	binary := strings.TrimSpace(graphBinary)
	if binary == "" {
		binary = "entire"
	}
	if cached, ok := graphAncestorSupport.Load(binary); ok {
		supported, _ := cached.(bool)
		return supported
	}
	supported, definitive := probeGraphAncestorSupport(ctx, runner, repoDir, binary)
	// Only a definitive answer is worth remembering. A timeout, a cancelled
	// context or a failure to spawn says nothing about the binary, and caching
	// it would mean a provider that DOES support the feature is never asked
	// again for the life of the process.
	if definitive {
		graphAncestorSupport.Store(binary, supported)
	}
	return supported
}

// graphBinaryRejectsHostVerb recognises the one failure that means the binary
// is not what --graph-binary asks for.
//
// That flag documents itself as "Entire CLI binary that exposes `graph`
// provider commands", so a bare provider plugin — which answers `capabilities`
// and `context` directly and rejects the `graph` prefix — is an unsupported
// input rather than a shape to accommodate. Failing closed is therefore
// correct. Failing SILENTLY is not: every provider call degrades, the brief
// simply has no code context, and nothing says why.
func graphBinaryRejectsHostVerb(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), `unknown command "graph"`)
}

// graphHostVerbDetail is the one wording for that cause, so the brief and the
// capability probe explain it identically.
func graphHostVerbDetail(binary string) string {
	return fmt.Sprintf("%s does not expose `graph` provider commands; --graph-binary needs an Entire CLI, not a bare provider plugin", binary)
}

// probeGraphAncestorSupport reports whether the binary supports the feature,
// and whether the answer is DEFINITIVE -- i.e. whether it learned anything
// about this binary at all. It always fails closed; the second return exists
// only so the caller knows what is safe to remember.
func probeGraphAncestorSupport(ctx context.Context, runner CommandRunner, repoDir, binary string) (supported, definitive bool) {
	if runner == nil {
		return false, false
	}
	probeCtx, cancel := context.WithTimeout(ctx, graphTaskContextTimeout)
	defer cancel()
	stdout, _, err := runner.Run(probeCtx, repoDir, binary, "graph", "capabilities", "--json")
	if err != nil {
		// A binary that rejects the host verb is not a transient failure: it
		// is the wrong kind of binary and will answer the same way every time.
		return false, graphBinaryRejectsHostVerb(err)
	}
	var report struct {
		OptionalLocalOnlyFeatures map[string]bool `json:"optional_local_only_features"`
	}
	if err := json.Unmarshal(stdout, &report); err != nil {
		// Could be a truncated read as easily as a binary that cannot speak
		// JSON, so fail closed without concluding anything about the binary.
		return false, false
	}
	return report.OptionalLocalOnlyFeatures["context_allow_ancestor"], true
}
