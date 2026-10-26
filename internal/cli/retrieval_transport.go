package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/spf13/cobra"
)

// retrievalTransportExtras is the already-bounded, optional material carried
// beside ranked rows. The final response budget is still measured against the
// complete serialized shape: these fields, query/branch, indentation, and an
// MCP tool-result envelope all count.
type retrievalTransportExtras struct {
	AbstractPreviews          []sessionAbstractPreview
	AbstractPreviewsTruncated bool
	Hints                     retrievalTaskHints
	Related                   []relatedPatternRef
	BlindSpot                 string
}

// beforeRetrievalResponsePrivacyRecheck is a deterministic race seam for
// tests. Production leaves it as a no-op. It runs after ranking and immediately
// before the authoritative pre-emission privacy check.
var beforeRetrievalResponsePrivacyRecheck = func() {}

// beforeRetrievalResponsePrivacyEmissionCheck is the final transport-boundary
// race seam. Production leaves it as a no-op; tests use it to land a tombstone
// after the complete response has been buffered but before any byte is written.
var beforeRetrievalResponsePrivacyEmissionCheck = func() {}

// beforeRetrievalResponseWrite is a deterministic seam after the authoritative
// policy checks have succeeded but while every involved Brain write lock is
// still held. Tests use it to prove a concurrent tombstone writer cannot
// linearize between the last privacy check and the first response byte.
var beforeRetrievalResponseWrite = func() {}

// The retrieval privacy policy lives in brainstore; the alias keeps the
// package-local name for the many existing readers and tests.
type retrievalPrivacyPolicy = brainstore.RetrievalPrivacyPolicy

// errIssueEvidenceChanged is the refusal every surface returns when the issue
// evidence a response was assembled from moved before it could be delivered.
var errIssueEvidenceChanged = errors.New("issue evidence changed while assembling response; retry retrieval")

// bufferRetrievalCommandOutput preserves each command's exact legacy bytes
// while moving the authoritative privacy check to one shared final boundary.
// Rendering failures discard the private buffer and emit nothing.
func bufferRetrievalCommandOutput(cmd *cobra.Command, policies []retrievalPrivacyPolicy, render func() error) error {
	original := cmd.OutOrStdout()
	var buffered bytes.Buffer
	cmd.SetOut(&buffered)
	defer cmd.SetOut(original)
	if err := render(); err != nil {
		return err
	}
	cmd.SetOut(original)
	return writeRetrievalResponseBytes(original, buffered.Bytes(), policies...)
}

// privacyLinearizedWriter is for an interactive surface that cannot buffer its
// entire lifetime (the dashboard TUI). Each terminal write is linearized under
// the captured policy; once policy changes, the next draw fails closed.
type privacyLinearizedWriter struct {
	out      io.Writer
	policies []retrievalPrivacyPolicy
}

func (w privacyLinearizedWriter) Write(p []byte) (int, error) {
	err := writeRetrievalResponseBytes(w.out, p, w.policies...)
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

// captureRetrievalPrivacyPolicy is the CLI adapter's capture: brainstore pins
// the checked tombstone identity, and the issue-evidence identity is pinned
// beside it so a disconnect or purge landing during ranking cannot deliver
// previously assembled issue evidence.
func captureRetrievalPrivacyPolicy(brainDir string) (retrievalPrivacyPolicy, error) {
	policy, err := brainstore.CaptureRetrievalPrivacyPolicy(brainDir)
	if err != nil {
		return retrievalPrivacyPolicy{}, err
	}
	issueIdentity, err := issueVisibilityIdentity(brainDir)
	if err != nil {
		return retrievalPrivacyPolicy{}, err
	}
	policy.IssueIdentity = issueIdentity
	return policy, nil
}

// writeRetrievalResponseBytes is the only final write boundary for retrieval,
// get, brief, and workspace payloads. Callers must buffer the exact response
// first. A policy change never produces a partial stale response: every checked
// brain is revalidated before the first byte is handed to the consumer.
func writeRetrievalResponseBytes(out io.Writer, data []byte, policies ...retrievalPrivacyPolicy) error {
	beforeRetrievalResponsePrivacyEmissionCheck()
	if transactional, ok := out.(interface {
		writeRetrievalResponse([]byte, []retrievalPrivacyPolicy) error
	}); ok {
		return transactional.writeRetrievalResponse(data, policies)
	}
	return withLockedRetrievalPrivacyPolicies(policies, func() error {
		beforeRetrievalResponseWrite()
		n, err := out.Write(data)
		if err == nil && n != len(data) {
			return io.ErrShortWrite
		}
		return err
	})
}

// withLockedRetrievalPrivacyPolicies is the response linearization boundary
// (brainstore owns the lock ordering and identity revalidation); the CLI
// adapters bind the derived-read gate.
func withLockedRetrievalPrivacyPolicies(policies []retrievalPrivacyPolicy, fn func() error) error {
	unlock, err := acquireRetrievalPrivacyPolicies(policies)
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}

// acquireRetrievalPrivacyPolicies binds the CLI adapter's gates around
// brainstore's lock ordering and tombstone revalidation: the derived-read gate
// is injected, and the pinned issue-evidence identity of every policy is
// re-read under the lock so a concurrent disconnect or purge refuses delivery
// instead of exposing evidence that is no longer available.
func acquireRetrievalPrivacyPolicies(policies []retrievalPrivacyPolicy) (func(), error) {
	issueByBrain := make(map[string]string, len(policies))
	for _, policy := range policies {
		if policy.BrainDir == "" || policy.IssueIdentity == "" {
			continue
		}
		brainDir := filepath.Clean(policy.BrainDir)
		if prior := issueByBrain[brainDir]; prior != "" && prior != policy.IssueIdentity {
			return nil, errIssueEvidenceChanged
		}
		issueByBrain[brainDir] = policy.IssueIdentity
	}
	release, err := brainstore.AcquireRetrievalPrivacyPolicies(policies, requirePrivacyDerivedRead)
	if err != nil {
		return nil, err
	}
	brainDirs := make([]string, 0, len(issueByBrain))
	for brainDir := range issueByBrain {
		brainDirs = append(brainDirs, brainDir)
	}
	sort.Strings(brainDirs)
	for _, brainDir := range brainDirs {
		actual, err := issueVisibilityIdentity(brainDir)
		if err != nil {
			release()
			return nil, err
		}
		if actual != issueByBrain[brainDir] {
			release()
			return nil, errIssueEvidenceChanged
		}
	}
	return release, nil
}

// revalidateRetrievalResponsePrivacy closes the ranking-to-write window. A
// tombstone that appears after ranking either makes the authoritative derived
// state dirty (and fails the response closed) or is fully cleaned and reaches
// the freshly loaded session guard below.
func revalidateRetrievalResponsePrivacy(brainDir string, results []unifiedResult) ([]unifiedResult, string, error) {
	beforeRetrievalResponsePrivacyRecheck()
	allowed, err := privacyDerivedReadGate(brainDir)
	if err != nil {
		return nil, "", err
	}
	if !allowed {
		return nil, "", fmt.Errorf("%s: retrieval state changed after ranking; retry after excluded-session cleanup verifies clean", brainstore.PrivacyDirtyCode)
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		return nil, "", err
	}
	guard, err := loadSessionReadGuard(brainDir, manifest)
	if err != nil {
		return nil, "", err
	}
	if len(results) == 0 {
		return results, guard.PolicyIdentity, nil
	}
	if guard.Empty() {
		return results, guard.PolicyIdentity, nil
	}

	// A completed exclusion deliberately retains canonical transcripts, so a
	// clean derived-state verification alone is insufficient. Rebuild the
	// allowed virtual-session set through the fresh guard and drop any response
	// row whose direct or nested conversation proof is no longer readable.
	allowedRefs := map[string]bool{}
	allowedEvidence := map[string]bool{}
	if manifest.Sources != nil && manifest.Sources.History != nil {
		fresh, loadErr := loadFreshHistory(brainDir, manifest.Sources.History)
		if loadErr != nil {
			return nil, "", fmt.Errorf("load history for privacy recheck: %w", loadErr)
		}
		for ref, view := range buildConversationSessionViews(fresh, manifest.RepoKey, manifest, guard) {
			allowedRefs[ref] = true
			for _, record := range view.Records {
				allowedEvidence[record.ID] = true
			}
		}
	}
	kept := make([]unifiedResult, 0, len(results))
	for _, result := range results {
		if guard.BlocksSession(result.SessionID) || guardBlocksRecord(guard, historyRecord{Path: result.Path}) {
			continue
		}
		if result.SessionRef != "" && !allowedRefs[result.SessionRef] {
			continue
		}
		blockedProof := false
		for _, id := range result.EvidenceIDs {
			if strings.HasPrefix(id, conversationIDPrefix) && !allowedEvidence[id] {
				blockedProof = true
				break
			}
		}
		if !blockedProof {
			for _, match := range result.ConceptMatches {
				if strings.HasPrefix(match.ConversationID, conversationIDPrefix) && !allowedEvidence[match.ConversationID] {
					blockedProof = true
					break
				}
			}
		}
		if !blockedProof {
			kept = append(kept, result)
		}
	}
	return kept, guard.PolicyIdentity, nil
}

func jsonOutputBytes(value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// retrievalSerializedSize measures the bytes that this invocation will
// actually hand to its consumer. MCP carries the indented JSON document as an
// escaped text item, so measuring only the inner rows undercounts quotes,
// backslashes, newlines, and the content envelope.
func retrievalSerializedSize(ctx context.Context, payload map[string]any, surface string) (int, error) {
	inner, err := jsonOutputBytes(payload)
	if err != nil {
		return 0, err
	}
	if !strings.HasPrefix(surface, "mcp:") {
		return len(inner), nil
	}
	toolResult := map[string]any{"content": []map[string]any{{"type": "text", "text": string(inner)}}}
	return mcpToolResultTransportSize(ctx, toolResult)
}

func filterAbstractPreviewsForRows(previews []sessionAbstractPreview, rows []compactUnifiedResult) ([]sessionAbstractPreview, bool) {
	if len(previews) == 0 {
		return nil, false
	}
	refs := make(map[string]bool, len(rows))
	for _, row := range rows {
		if row.SessionRef != "" {
			refs[row.SessionRef] = true
		}
	}
	out := make([]sessionAbstractPreview, 0, len(previews))
	for _, preview := range previews {
		if refs[preview.SessionRef] {
			out = append(out, preview)
		}
	}
	return out, len(out) != len(previews)
}

func buildRetrievalJSONPayload(query, branch string, rows []compactUnifiedResult, extras retrievalTransportExtras, truncated bool) map[string]any {
	payload := map[string]any{"query": query, "branch": branch, "results": rows}
	if truncated {
		payload["response_truncated"] = true
		if len(rows) > 0 {
			// Preserve the additive row marker shipped by the initial multi-concept surface,
			// while making the envelope marker usable even when zero rows fit.
			rows[len(rows)-1].ResponseTruncated = true
			payload["results"] = rows
		}
	}
	if previews, cut := filterAbstractPreviewsForRows(extras.AbstractPreviews, rows); len(previews) > 0 || extras.AbstractPreviews != nil {
		payload["abstract_previews"] = previews
		payload["abstract_previews_truncated"] = extras.AbstractPreviewsTruncated || cut
	}
	if len(extras.Hints.LikelyEditFiles) > 0 {
		payload["likely_edit_files"] = extras.Hints.LikelyEditFiles
	}
	if len(extras.Hints.LikelyTestFiles) > 0 {
		payload["likely_test_files"] = extras.Hints.LikelyTestFiles
	}
	if len(extras.Hints.ActionChecklist) > 0 {
		payload["action_checklist"] = extras.Hints.ActionChecklist
	}
	if len(extras.Related) > 0 {
		payload["related_patterns"] = extras.Related
	}
	if len(rows) == 0 && extras.BlindSpot != "" {
		payload["blind_spot"] = extras.BlindSpot
	}
	return payload
}

func cloneRetrievalTransportExtras(extras retrievalTransportExtras) retrievalTransportExtras {
	extras.AbstractPreviews = append([]sessionAbstractPreview(nil), extras.AbstractPreviews...)
	extras.Hints.LikelyEditFiles = append([]string(nil), extras.Hints.LikelyEditFiles...)
	extras.Hints.LikelyTestFiles = append([]string(nil), extras.Hints.LikelyTestFiles...)
	extras.Hints.ActionChecklist = append([]brainBriefAction(nil), extras.Hints.ActionChecklist...)
	extras.Related = append([]relatedPatternRef(nil), extras.Related...)
	return extras
}

// trimRetrievalExtrasOnce drops one lowest-priority optional tail item. This
// gives the caller a useful bounded response even when previews or discovery
// pointers consume the budget before any ranked row does.
func trimRetrievalExtrasOnce(extras *retrievalTransportExtras) bool {
	switch {
	case len(extras.AbstractPreviews) > 0:
		extras.AbstractPreviews = extras.AbstractPreviews[:len(extras.AbstractPreviews)-1]
		extras.AbstractPreviewsTruncated = true
	case len(extras.Related) > 0:
		extras.Related = extras.Related[:len(extras.Related)-1]
	case len(extras.Hints.ActionChecklist) > 0:
		extras.Hints.ActionChecklist = extras.Hints.ActionChecklist[:len(extras.Hints.ActionChecklist)-1]
	case len(extras.Hints.LikelyTestFiles) > 0:
		extras.Hints.LikelyTestFiles = extras.Hints.LikelyTestFiles[:len(extras.Hints.LikelyTestFiles)-1]
	case len(extras.Hints.LikelyEditFiles) > 0:
		extras.Hints.LikelyEditFiles = extras.Hints.LikelyEditFiles[:len(extras.Hints.LikelyEditFiles)-1]
	case extras.BlindSpot != "":
		extras.BlindSpot = ""
	default:
		return false
	}
	return true
}

// countRetrievalExtrasTrims reports how many steps trimRetrievalExtrasOnce
// can apply to extras before it is exhausted.
func countRetrievalExtrasTrims(extras retrievalTransportExtras) int {
	ex := cloneRetrievalTransportExtras(extras)
	n := 0
	for trimRetrievalExtrasOnce(&ex) {
		n++
	}
	return n
}

// smallestTrimStepsThatFit finds the fewest trim steps, between 1 and
// maxSteps, whose rendering fits the response budget. Trimming is monotone —
// every step only removes content — so once some step count fits, every
// larger one fits, and a binary search returns the same state the former
// one-step-at-a-time loop reached while rendering O(log n) candidates
// instead of O(n). sizeAt must rebuild from pristine inputs so probes are
// order-independent. The untrimmed rendering is the caller's job to check
// first: step one adds the truncation marker and can grow the output.
func smallestTrimStepsThatFit(maxSteps int, sizeAt func(int) (int, error)) (int, bool, error) {
	if maxSteps < 1 {
		return 0, false, nil
	}
	if size, err := sizeAt(maxSteps); err != nil || size > conversationConceptResponseMaxBytes {
		return 0, false, err
	}
	lo, hi := 1, maxSteps
	for lo < hi {
		mid := (lo + hi) / 2
		size, err := sizeAt(mid)
		if err != nil {
			return 0, false, err
		}
		if size <= conversationConceptResponseMaxBytes {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo, true, nil
}

// boundedRetrievalJSONPayload removes optional extras, then whole tail rows,
// until the exact CLI or MCP serialization fits. The response_truncated
// marker is present during every truncated size check, so the marker itself
// can never push the emitted document over budget.
func boundedRetrievalJSONPayload(ctx context.Context, query, branch string, results []unifiedResult, extras retrievalTransportExtras, surface string) (map[string]any, error) {
	allRows := compactUnifiedResults(results, query)
	extras = cloneRetrievalTransportExtras(extras)
	extrasSteps := countRetrievalExtrasTrims(extras)
	build := func(steps int) map[string]any {
		ex := cloneRetrievalTransportExtras(extras)
		for i := 0; i < min(steps, extrasSteps); i++ {
			trimRetrievalExtrasOnce(&ex)
		}
		rows := append([]compactUnifiedResult{}, allRows[:len(allRows)-max(steps-extrasSteps, 0)]...)
		return buildRetrievalJSONPayload(query, branch, rows, ex, steps > 0)
	}
	sizeAt := func(steps int) (int, error) {
		return retrievalSerializedSize(ctx, build(steps), surface)
	}
	if size, err := sizeAt(0); err != nil {
		return nil, err
	} else if size <= conversationConceptResponseMaxBytes {
		return build(0), nil
	}
	steps, ok, err := smallestTrimStepsThatFit(extrasSteps+len(allRows), sizeAt)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("retrieval response envelope exceeds %d bytes without any result rows", conversationConceptResponseMaxBytes)
	}
	return build(steps), nil
}

type retrievalIDQualifier func(string) string

func identityRetrievalID(id string) string { return id }

// renderRetrievalRowsText is shared by single-repo and workspace text modes,
// keeping the multi-concept proof contract at parity. Every proof line is independently
// bounded even though concepts and identifiers already have stricter input
// limits.
func renderRetrievalRowsText(out io.Writer, results []unifiedResult, qualify retrievalIDQualifier) {
	if qualify == nil {
		qualify = identityRetrievalID
	}
	for _, result := range results {
		ex := base.TruncateString(strings.Join(strings.Fields(result.Text), " "), 200)
		label := result.Source
		if result.VerificationRequired {
			label += " verify"
		}
		fmt.Fprintf(out, "[%s] %s  %s\n    %s\n", label, qualify(result.ID), unifiedResultLocation(result), ex)
		printRetrievalCaveats(out, result)
		if len(result.Concepts) == 0 {
			continue
		}
		concepts, _ := truncateUTF8Bytes(strings.Join(result.Concepts, " | "), 3*1024)
		fmt.Fprintf(out, "    proof concepts=%s worst_rank=%d rank_sum=%d approximate=%t\n", concepts, result.WorstRank, result.RankSum, result.Approximate)
		for _, match := range result.ConceptMatches {
			terms, _ := truncateUTF8Bytes(strings.Join(match.MatchedTerms, ","), 1024)
			concept, _ := truncateUTF8Bytes(match.Concept, conversationConceptMaxBytes)
			fmt.Fprintf(out, "    proof concept=%q evidence=%s rank=%d arm=%s matched_terms=%s\n",
				concept, qualify(match.ConversationID), match.Rank, match.Arm, terms)
		}
		if len(result.EvidenceIDs) > 0 {
			ids := make([]string, len(result.EvidenceIDs))
			for i, id := range result.EvidenceIDs {
				ids[i] = qualify(id)
			}
			fmt.Fprintf(out, "    evidence_ids=%s\n", strings.Join(ids, ","))
		}
	}
}

func filteredAbstractPreviewsForResults(previews []sessionAbstractPreview, results []unifiedResult) ([]sessionAbstractPreview, bool) {
	rows := compactUnifiedResults(results, "")
	return filterAbstractPreviewsForRows(previews, rows)
}

func renderSingleRepoRetrievalText(query string, results []unifiedResult, extras retrievalTransportExtras, truncated bool) []byte {
	var out bytes.Buffer
	if len(results) == 0 && !truncated {
		fmt.Fprintf(&out, "no results for %q\n", query)
		if extras.BlindSpot != "" {
			fmt.Fprintln(&out, extras.BlindSpot)
		}
	}
	hints := extras.Hints
	for _, file := range hints.LikelyEditFiles {
		fmt.Fprintf(&out, "edit_file %s\n", file)
	}
	for _, file := range hints.LikelyTestFiles {
		fmt.Fprintf(&out, "test_file %s\n", file)
	}
	for _, action := range hints.ActionChecklist {
		fmt.Fprintf(&out, "action %s %s: %s\n", action.File, action.Symbol, action.Action)
	}
	renderRetrievalRowsText(&out, results, nil)
	for _, pattern := range extras.Related {
		fmt.Fprintf(&out, "related [%s] %s  %s\n", pattern.Type, pattern.ID, pattern.Title)
	}
	previews, previewCut := filteredAbstractPreviewsForResults(extras.AbstractPreviews, results)
	for _, preview := range previews {
		text := ""
		if preview.Overview != nil {
			text = "  " + strings.Join(strings.Fields(preview.Overview.Text), " ")
		}
		fmt.Fprintf(&out, "abstract [%s] %s%s\n", preview.AbstractStatus, preview.SessionRef, text)
		if preview.AbstractIssue != "" {
			issue, _ := truncateUTF8Bytes(strings.Join(strings.Fields(preview.AbstractIssue), " "), 1024)
			fmt.Fprintf(&out, "  automatic enqueue issue: %s\n", issue)
		}
	}
	if extras.AbstractPreviewsTruncated || previewCut {
		fmt.Fprintln(&out, "abstract previews truncated")
	}
	fmt.Fprintf(&out, "response_truncated %t\n", truncated)
	return out.Bytes()
}

func boundedSingleRepoRetrievalText(query string, results []unifiedResult, extras retrievalTransportExtras) ([]byte, error) {
	extras = cloneRetrievalTransportExtras(extras)
	extrasSteps := countRetrievalExtrasTrims(extras)
	render := func(steps int) []byte {
		ex := cloneRetrievalTransportExtras(extras)
		for i := 0; i < min(steps, extrasSteps); i++ {
			trimRetrievalExtrasOnce(&ex)
		}
		count := len(results) - max(steps-extrasSteps, 0)
		return renderSingleRepoRetrievalText(query, results[:count], ex, steps > 0)
	}
	if data := render(0); len(data) <= conversationConceptResponseMaxBytes {
		return data, nil
	}
	steps, ok, err := smallestTrimStepsThatFit(extrasSteps+len(results), func(steps int) (int, error) {
		return len(render(steps)), nil
	})
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("retrieval text envelope exceeds %d bytes without any result rows", conversationConceptResponseMaxBytes)
	}
	return render(steps), nil
}

func qualifyWorkspaceAddressableID(repoKey, workspaceName, id string) string {
	id = strings.TrimSpace(id)
	if id == "" || strings.HasPrefix(id, repoKey+"/") {
		return id
	}
	if repoKey == workspaceName && (strings.HasPrefix(id, "graph:") || strings.HasPrefix(id, "pattern:") || strings.HasPrefix(id, "theme:")) {
		return id
	}
	return repoKey + "/" + id
}

func qualifyAbstractStatement(repoKey, workspaceName string, statement abstractStatement) abstractStatement {
	statement.EvidenceIDs = append([]string(nil), statement.EvidenceIDs...)
	for i := range statement.EvidenceIDs {
		statement.EvidenceIDs[i] = qualifyWorkspaceAddressableID(repoKey, workspaceName, statement.EvidenceIDs[i])
	}
	return statement
}

// qualifyWorkspaceUnifiedResult recursively qualifies every addressable id,
// not merely the top-level hit. A nested concept proof, session turn, related
// id, or abstract citation can therefore be pasted directly into workspace
// get without reconstructing the member namespace.
func qualifyWorkspaceUnifiedResult(repoKey, workspaceName string, result unifiedResult) unifiedResult {
	qualify := func(id string) string { return qualifyWorkspaceAddressableID(repoKey, workspaceName, id) }
	result.ID = qualify(result.ID)
	if result.Issue != nil {
		citation := *result.Issue
		citation.Snapshot = qualify(citation.Snapshot)
		citation.NextID = qualify(citation.NextID)
		result.Issue = &citation
	}
	result.SessionRef = qualify(result.SessionRef)
	result.TargetID = qualify(result.TargetID)
	result.RelatedIDs = append([]string(nil), result.RelatedIDs...)
	for i := range result.RelatedIDs {
		result.RelatedIDs[i] = qualify(result.RelatedIDs[i])
	}
	result.EvidenceIDs = append([]string(nil), result.EvidenceIDs...)
	for i := range result.EvidenceIDs {
		result.EvidenceIDs[i] = qualify(result.EvidenceIDs[i])
	}
	result.ConceptMatches = append([]conceptMatch(nil), result.ConceptMatches...)
	for i := range result.ConceptMatches {
		result.ConceptMatches[i].ConversationID = qualify(result.ConceptMatches[i].ConversationID)
		result.ConceptMatches[i].MatchedTerms = append([]string(nil), result.ConceptMatches[i].MatchedTerms...)
	}
	result.Turns = append([]conversationTurn(nil), result.Turns...)
	for i := range result.Turns {
		result.Turns[i].ConversationID = qualify(result.Turns[i].ConversationID)
	}
	if result.Abstract != nil {
		abstract := *result.Abstract
		abstract.SessionRef = qualify(abstract.SessionRef)
		abstract.Overview = qualifyAbstractStatement(repoKey, workspaceName, abstract.Overview)
		abstract.Outcomes = append([]abstractStatement(nil), abstract.Outcomes...)
		for i := range abstract.Outcomes {
			abstract.Outcomes[i] = qualifyAbstractStatement(repoKey, workspaceName, abstract.Outcomes[i])
		}
		abstract.Decisions = append([]abstractStatement(nil), abstract.Decisions...)
		for i := range abstract.Decisions {
			abstract.Decisions[i] = qualifyAbstractStatement(repoKey, workspaceName, abstract.Decisions[i])
		}
		abstract.Unresolved = append([]abstractStatement(nil), abstract.Unresolved...)
		for i := range abstract.Unresolved {
			abstract.Unresolved[i] = qualifyAbstractStatement(repoKey, workspaceName, abstract.Unresolved[i])
		}
		result.Abstract = &abstract
	}
	return result
}

func qualifyWorkspacePreview(repoKey, workspaceName string, preview sessionAbstractPreview) sessionAbstractPreview {
	preview.SessionRef = qualifyWorkspaceAddressableID(repoKey, workspaceName, preview.SessionRef)
	if preview.Overview != nil {
		overview := qualifyAbstractStatement(repoKey, workspaceName, *preview.Overview)
		preview.Overview = &overview
	}
	return preview
}

func qualifyWorkspaceRetrieveGroups(workspaceName string, groups []workspaceRetrieveResult) []workspaceRetrieveResult {
	out := make([]workspaceRetrieveResult, len(groups))
	for i, group := range groups {
		out[i] = group
		out[i].Results = make([]unifiedResult, len(group.Results))
		for j, result := range group.Results {
			out[i].Results[j] = qualifyWorkspaceUnifiedResult(group.RepoKey, workspaceName, result)
		}
		out[i].AbstractPreviews = make([]sessionAbstractPreview, len(group.AbstractPreviews))
		for j, preview := range group.AbstractPreviews {
			out[i].AbstractPreviews[j] = qualifyWorkspacePreview(group.RepoKey, workspaceName, preview)
		}
	}
	return out
}

func trimWorkspaceGroupsToCount(groups []workspaceRetrieveResult, keep int, truncated bool) []workspaceRetrieveResult {
	out := make([]workspaceRetrieveResult, len(groups))
	remaining := keep
	lastGroup, lastRow := -1, -1
	for i, group := range groups {
		out[i] = group
		count := min(remaining, len(group.Results))
		out[i].Results = append([]unifiedResult(nil), group.Results[:count]...)
		remaining -= count
		if count > 0 {
			lastGroup, lastRow = i, count-1
		}
		refs := map[string]bool{}
		for _, result := range out[i].Results {
			if result.SessionRef != "" {
				refs[result.SessionRef] = true
			}
		}
		previews := make([]sessionAbstractPreview, 0, len(group.AbstractPreviews))
		for _, preview := range group.AbstractPreviews {
			if refs[preview.SessionRef] {
				previews = append(previews, preview)
			}
		}
		out[i].AbstractPreviews = previews
		out[i].AbstractPreviewsTruncated = group.AbstractPreviewsTruncated || len(previews) != len(group.AbstractPreviews)
	}
	if truncated && lastGroup >= 0 {
		out[lastGroup].Results[lastRow].ResponseTruncated = true
	}
	return out
}

func cloneWorkspaceRetrieveGroups(groups []workspaceRetrieveResult) []workspaceRetrieveResult {
	out := make([]workspaceRetrieveResult, len(groups))
	for i, group := range groups {
		out[i] = group
		out[i].Results = append([]unifiedResult(nil), group.Results...)
		out[i].AbstractPreviews = append([]sessionAbstractPreview(nil), group.AbstractPreviews...)
	}
	return out
}

// trimWorkspaceExtrasOnce drops one lowest-priority optional tail item so the
// caller keeps a useful bounded response. A member's Error is deliberately NOT
// trimmable: clearing it makes a member whose retrieval failed serialize as
// {"results": []}, byte-identical to a healthy member with no matches, and the
// generic response_truncated marker does not say a failure was suppressed.
// Ranked rows are the cheap thing to drop; a member's failure is not.
func trimWorkspaceExtrasOnce(groups []workspaceRetrieveResult) bool {
	for i := len(groups) - 1; i >= 0; i-- {
		switch {
		case len(groups[i].AbstractPreviews) > 0:
			groups[i].AbstractPreviews = groups[i].AbstractPreviews[:len(groups[i].AbstractPreviews)-1]
			groups[i].AbstractPreviewsTruncated = true
			return true
		case groups[i].Freshness.Detail != "":
			groups[i].Freshness.Detail = ""
			return true
		case groups[i].Freshness.ContractDetail != "":
			groups[i].Freshness.ContractDetail = ""
			return true
		}
	}
	return false
}

// countWorkspaceExtrasTrims reports how many steps trimWorkspaceExtrasOnce
// can apply to groups before it is exhausted.
func countWorkspaceExtrasTrims(groups []workspaceRetrieveResult) int {
	gs := cloneWorkspaceRetrieveGroups(groups)
	n := 0
	for trimWorkspaceExtrasOnce(gs) {
		n++
	}
	return n
}

// applyWorkspaceTrimSteps rebuilds the trim state after `steps` steps of the
// deterministic sequence shared by both workspace surfaces: optional extras
// first, then one kept row at a time, then whole tail groups.
func applyWorkspaceTrimSteps(pristine []workspaceRetrieveResult, extrasSteps, total, steps int) (groups []workspaceRetrieveResult, keep int) {
	gs := cloneWorkspaceRetrieveGroups(pristine)
	for i := 0; i < min(steps, extrasSteps); i++ {
		trimWorkspaceExtrasOnce(gs)
	}
	rowDrops := min(max(steps-extrasSteps, 0), total)
	groupDrops := max(steps-extrasSteps-total, 0)
	return gs[:len(gs)-groupDrops], total - rowDrops
}

func boundedWorkspaceRetrievalJSONPayload(workspaceName, query string, groups []workspaceRetrieveResult) (map[string]any, error) {
	qualified := cloneWorkspaceRetrieveGroups(qualifyWorkspaceRetrieveGroups(workspaceName, groups))
	total := 0
	for _, group := range qualified {
		total += len(group.Results)
	}
	extrasSteps := countWorkspaceExtrasTrims(qualified)
	build := func(steps int) map[string]any {
		gs, keep := applyWorkspaceTrimSteps(qualified, extrasSteps, total, steps)
		payload := map[string]any{
			"workspace": workspaceName,
			"query":     query,
			"results":   trimWorkspaceGroupsToCount(gs, keep, steps > 0),
		}
		if steps > 0 {
			payload["response_truncated"] = true
		}
		return payload
	}
	sizeAt := func(steps int) (int, error) {
		data, err := jsonOutputBytes(build(steps))
		if err != nil {
			return 0, err
		}
		return len(data), nil
	}
	if size, err := sizeAt(0); err != nil {
		return nil, err
	} else if size <= conversationConceptResponseMaxBytes {
		return build(0), nil
	}
	steps, ok, err := smallestTrimStepsThatFit(extrasSteps+total+len(qualified), sizeAt)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("workspace retrieval response envelope exceeds %d bytes without any result rows", conversationConceptResponseMaxBytes)
	}
	return build(steps), nil
}

func renderWorkspaceRetrievalText(workspaceName string, groups []workspaceRetrieveResult, keep int, truncated bool) []byte {
	var out bytes.Buffer
	remaining := keep
	for _, group := range groups {
		count := min(remaining, len(group.Results))
		remaining -= count
		qualify := func(id string) string { return qualifyWorkspaceAddressableID(group.RepoKey, workspaceName, id) }
		renderRetrievalRowsText(&out, group.Results[:count], qualify)
		if group.Error != "" {
			fmt.Fprintf(&out, "%s error %s\n", group.RepoKey, group.Error)
		}
	}
	fmt.Fprintf(&out, "response_truncated %t\n", truncated)
	return out.Bytes()
}

func boundedWorkspaceRetrievalText(workspaceName string, groups []workspaceRetrieveResult) ([]byte, error) {
	groups = cloneWorkspaceRetrieveGroups(groups)
	total := 0
	for _, group := range groups {
		total += len(group.Results)
	}
	extrasSteps := countWorkspaceExtrasTrims(groups)
	render := func(steps int) []byte {
		gs, keep := applyWorkspaceTrimSteps(groups, extrasSteps, total, steps)
		return renderWorkspaceRetrievalText(workspaceName, gs, keep, steps > 0)
	}
	if data := render(0); len(data) <= conversationConceptResponseMaxBytes {
		return data, nil
	}
	steps, ok, err := smallestTrimStepsThatFit(extrasSteps+total+len(groups), func(steps int) (int, error) {
		return len(render(steps)), nil
	})
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("workspace retrieval text envelope exceeds %d bytes without any result rows", conversationConceptResponseMaxBytes)
	}
	return render(steps), nil
}
