package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

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

// retrievalPrivacyPolicy is the checked tombstone generation under which a
// response was assembled. Keeping the brain path beside the identity lets the
// same final writer cover single-repository, workspace, get, and brief output.
type retrievalPrivacyPolicy struct {
	BrainDir            string
	Identity            string
	RequireDerivedClean bool
}

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

func captureRetrievalPrivacyPolicy(brainDir string) (retrievalPrivacyPolicy, error) {
	guard, err := loadSessionReadGuard(brainDir, nil)
	if err != nil {
		return retrievalPrivacyPolicy{}, err
	}
	return retrievalPrivacyPolicy{BrainDir: brainDir, Identity: guard.policyIdentity}, nil
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

// withLockedRetrievalPrivacyPolicies is the response linearization boundary.
// Tombstone mutations use the same per-Brain write lock. Locks are acquired in
// deterministic path order so a workspace response spanning several Brains
// cannot deadlock with another workspace response containing the same members.
func withLockedRetrievalPrivacyPolicies(policies []retrievalPrivacyPolicy, fn func() error) error {
	unlock, err := acquireRetrievalPrivacyPolicies(policies)
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}

func acquireRetrievalPrivacyPolicies(policies []retrievalPrivacyPolicy) (func(), error) {
	byBrain := make(map[string]string, len(policies))
	requireDerivedClean := make(map[string]bool, len(policies))
	for _, policy := range policies {
		if policy.BrainDir == "" {
			continue
		}
		brainDir := filepath.Clean(policy.BrainDir)
		if prior, exists := byBrain[brainDir]; exists && prior != policy.Identity {
			return nil, fmt.Errorf("%s: response was assembled under conflicting privacy policies", memoryErrPrivacyDirty)
		}
		byBrain[brainDir] = policy.Identity
		requireDerivedClean[brainDir] = requireDerivedClean[brainDir] || policy.RequireDerivedClean
	}
	brainDirs := make([]string, 0, len(byBrain))
	for brainDir := range byBrain {
		brainDirs = append(brainDirs, brainDir)
	}
	sort.Strings(brainDirs)
	unlocks := make([]func(), 0, len(brainDirs))
	for _, brainDir := range brainDirs {
		unlock, err := acquireBrainWriteLock(brainDir)
		if err != nil {
			for i := len(unlocks) - 1; i >= 0; i-- {
				unlocks[i]()
			}
			return nil, err
		}
		unlocks = append(unlocks, unlock)
	}
	release := func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}
	for _, brainDir := range brainDirs {
		if err := requireRetrievalPrivacyPolicyIdentity(brainDir, byBrain[brainDir]); err != nil {
			release()
			return nil, err
		}
		if requireDerivedClean[brainDir] {
			if err := requirePrivacyDerivedRead(brainDir); err != nil {
				release()
				return nil, err
			}
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
		return nil, "", fmt.Errorf("%s: retrieval state changed after ranking; retry after excluded-session cleanup verifies clean", memoryErrPrivacyDirty)
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
		return results, guard.policyIdentity, nil
	}
	if guard.empty() {
		return results, guard.policyIdentity, nil
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
		if guard.blocksSession(result.SessionID) || guard.blocksRecord(historyRecord{Path: result.Path}) {
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
	return kept, guard.policyIdentity, nil
}

func requireRetrievalPrivacyPolicyIdentity(brainDir, expected string) error {
	guard, err := loadSessionReadGuard(brainDir, nil)
	if err != nil {
		return err
	}
	if guard.policyIdentity != expected {
		return fmt.Errorf("%s: retrieval privacy policy changed during response assembly; retry the request", memoryErrPrivacyDirty)
	}
	return nil
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
			// Preserve the additive row marker shipped by the initial C2 surface,
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

// boundedRetrievalJSONPayload removes whole tail rows until the exact CLI or
// MCP serialization fits. The response_truncated marker is present during
// every truncated size check, so the marker itself can never push the emitted
// document over budget.
func boundedRetrievalJSONPayload(ctx context.Context, query, branch string, results []unifiedResult, extras retrievalTransportExtras, surface string) (map[string]any, error) {
	allRows := compactUnifiedResults(results, query)
	extras = cloneRetrievalTransportExtras(extras)
	count := len(allRows)
	anythingTruncated := false
	for {
		rows := append([]compactUnifiedResult(nil), allRows[:count]...)
		truncated := anythingTruncated || count != len(allRows)
		payload := buildRetrievalJSONPayload(query, branch, rows, extras, truncated)
		size, err := retrievalSerializedSize(ctx, payload, surface)
		if err != nil {
			return nil, err
		}
		if size <= conversationConceptResponseMaxBytes {
			return payload, nil
		}
		if trimRetrievalExtrasOnce(&extras) {
			anythingTruncated = true
			continue
		}
		if count > 0 {
			count--
			anythingTruncated = true
			continue
		}
		break
	}
	return nil, fmt.Errorf("retrieval response envelope exceeds %d bytes without any result rows", conversationConceptResponseMaxBytes)
}

type retrievalIDQualifier func(string) string

func identityRetrievalID(id string) string { return id }

// renderRetrievalRowsText is shared by single-repo and workspace text modes,
// keeping the C2 proof contract at parity. Every proof line is independently
// bounded even though concepts and identifiers already have stricter input
// limits.
func renderRetrievalRowsText(out io.Writer, results []unifiedResult, qualify retrievalIDQualifier) {
	if qualify == nil {
		qualify = identityRetrievalID
	}
	for _, result := range results {
		ex := truncateString(strings.Join(strings.Fields(result.Text), " "), 200)
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
	count := len(results)
	anythingTruncated := false
	for {
		truncated := anythingTruncated || count != len(results)
		data := renderSingleRepoRetrievalText(query, results[:count], extras, truncated)
		if len(data) <= conversationConceptResponseMaxBytes {
			return data, nil
		}
		if trimRetrievalExtrasOnce(&extras) {
			anythingTruncated = true
			continue
		}
		if count > 0 {
			count--
			anythingTruncated = true
			continue
		}
		break
	}
	return nil, fmt.Errorf("retrieval text envelope exceeds %d bytes without any result rows", conversationConceptResponseMaxBytes)
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

func trimWorkspaceExtrasOnce(groups []workspaceRetrieveResult) bool {
	for i := len(groups) - 1; i >= 0; i-- {
		switch {
		case len(groups[i].AbstractPreviews) > 0:
			groups[i].AbstractPreviews = groups[i].AbstractPreviews[:len(groups[i].AbstractPreviews)-1]
			groups[i].AbstractPreviewsTruncated = true
			return true
		case groups[i].Error != "":
			groups[i].Error = ""
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

func boundedWorkspaceRetrievalJSONPayload(workspaceName, query string, groups []workspaceRetrieveResult) (map[string]any, error) {
	qualified := cloneWorkspaceRetrieveGroups(qualifyWorkspaceRetrieveGroups(workspaceName, groups))
	total := 0
	for _, group := range qualified {
		total += len(group.Results)
	}
	keep := total
	anythingTruncated := false
	for {
		truncated := anythingTruncated || keep != total
		payload := map[string]any{
			"workspace": workspaceName,
			"query":     query,
			"results":   trimWorkspaceGroupsToCount(qualified, keep, truncated),
		}
		if truncated {
			payload["response_truncated"] = true
		}
		data, err := jsonOutputBytes(payload)
		if err != nil {
			return nil, err
		}
		if len(data) <= conversationConceptResponseMaxBytes {
			return payload, nil
		}
		if trimWorkspaceExtrasOnce(qualified) {
			anythingTruncated = true
			continue
		}
		if keep > 0 {
			keep--
			anythingTruncated = true
			continue
		}
		if len(qualified) > 0 {
			qualified = qualified[:len(qualified)-1]
			anythingTruncated = true
			continue
		}
		break
	}
	return nil, fmt.Errorf("workspace retrieval response envelope exceeds %d bytes without any result rows", conversationConceptResponseMaxBytes)
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
	keep := total
	anythingTruncated := false
	for {
		data := renderWorkspaceRetrievalText(workspaceName, groups, keep, anythingTruncated || keep != total)
		if len(data) <= conversationConceptResponseMaxBytes {
			return data, nil
		}
		if trimWorkspaceExtrasOnce(groups) {
			anythingTruncated = true
			continue
		}
		if keep > 0 {
			keep--
			anythingTruncated = true
			continue
		}
		if len(groups) > 0 {
			groups = groups[:len(groups)-1]
			anythingTruncated = true
			continue
		}
		break
	}
	return nil, fmt.Errorf("workspace retrieval text envelope exceeds %d bytes without any result rows", conversationConceptResponseMaxBytes)
}
