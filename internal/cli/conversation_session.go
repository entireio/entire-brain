package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// conversation_session.go implements virtual session navigation: a VIRTUAL
// session identity over the reconciled exchange view, bounded adjacent-context
// expansion for `get conversation:<id>`, and a bounded session outline for
// `get conversation-session:<id>`. No session body or second archive exists;
// everything resolves from the reconciled records plus the canonical manifest,
// and every source read stays behind the trusted-ID and byte-bound contract.

const (
	conversationSessionIDPrefix = "conversation-session:"

	// conversationContextMax bounds --context-before/--context-after.
	conversationContextMax = 3
	// Outline pagination: default and maximum entries per page.
	conversationOutlineDefaultLimit = 20
	conversationOutlineMaxLimit     = 50
	// conversationOutlinePageMaxBytes caps one outline page; a navigation
	// packet (target plus expanded context) is capped separately.
	conversationOutlinePageMaxBytes = 64 * 1024
	conversationPacketMaxBytes      = 128 * 1024
	// conversationOutlineRequestMaxBytes bounds the per-entry request excerpt.
	conversationOutlineRequestMaxBytes = 256
)

// conversationSessionRef derives the stable virtual session identity:
// sha256(repo_key NUL canonical_branch NUL session_id). When the session id
// is absent the source digest substitutes and the identity is degraded; a
// degraded identity is never merged with a later non-degraded one.
func conversationSessionRef(repoKey, canonicalBranch, sessionID, sourceDigest string) (string, bool) {
	identity := strings.TrimSpace(sessionID)
	degraded := false
	if identity == "" {
		identity = strings.TrimSpace(sourceDigest)
		degraded = true
	}
	h := sha256.New()
	h.Write([]byte(repoKey))
	h.Write([]byte{0})
	h.Write([]byte(canonicalBranch))
	h.Write([]byte{0})
	h.Write([]byte(identity))
	return conversationSessionIDPrefix + hex.EncodeToString(h.Sum(nil)), degraded
}

// conversationCanonicalBranch resolves a record's canonical branch: the
// captured branch, else the manifest default, else the distill default.
func conversationCanonicalBranch(record historyRecord, manifest *exportManifest) string {
	if branch := strings.TrimSpace(record.Branch); branch != "" {
		return branch
	}
	if manifest != nil && manifest.Sources != nil && manifest.Sources.Sessions != nil {
		if branch := strings.TrimSpace(manifest.Sources.Sessions.DefaultBranch); branch != "" {
			return branch
		}
	}
	return distillDefaultBranch
}

// recordSessionRef computes the session reference for one reconciled record.
func recordSessionRef(record historyRecord, repoKey string, manifest *exportManifest) (string, bool) {
	return conversationSessionRef(repoKey, conversationCanonicalBranch(record, manifest), record.SessionID, record.SourceDigest)
}

// conversationSessionView is the reconciled per-session record list, ordered
// by turn ordinal. Duplicate ordinals resolve through the newest-record
// rule; there is no silent map-order winner.
type conversationSessionView struct {
	Ref      string
	Degraded bool
	Records  []historyRecord
}

// buildConversationSessionViews groups the guard-filtered exchange records by
// session reference. It reads the MERGED two-tier view, not the ID-collapsed
// one: a legacy exchange ID colliding across two session scopes must stay
// visible in both scopes so lookup can report the ambiguity instead of a
// silent winner. Within one scope, duplicate ordinals still resolve through
// the newest-record rule.
func buildConversationSessionViews(fresh freshHistory, repoKey string, manifest *exportManifest, guard sessionReadGuard) map[string]conversationSessionView {
	views := map[string]conversationSessionView{}
	for _, record := range fresh.mergedRecords() {
		if record.Kind != conversationKind || guard.blocksRecord(record) {
			continue
		}
		ref, degraded := recordSessionRef(record, repoKey, manifest)
		view := views[ref]
		view.Ref = ref
		view.Degraded = degraded
		view.Records = append(view.Records, record)
		views[ref] = view
	}
	for ref, view := range views {
		sort.SliceStable(view.Records, func(i, j int) bool {
			if view.Records[i].TurnOrdinal != view.Records[j].TurnOrdinal {
				return view.Records[i].TurnOrdinal < view.Records[j].TurnOrdinal
			}
			return view.Records[i].ID < view.Records[j].ID
		})
		// Duplicate ordinals: keep the deterministic newest-source winner.
		deduped := view.Records[:0:0]
		for _, record := range view.Records {
			if n := len(deduped); n > 0 && deduped[n-1].TurnOrdinal == record.TurnOrdinal && record.TurnOrdinal != 0 {
				deduped[n-1] = newestHistoryRecord(deduped[n-1], record)
				continue
			}
			deduped = append(deduped, record)
		}
		view.Records = deduped
		views[ref] = view
	}
	return views
}

// conversationTurn is one bounded outline or context entry. Outline entries
// carry the request excerpt; context entries carry the expanded bounded text.
// Neither ever contains hidden reasoning or raw tool output.
type conversationTurn struct {
	ConversationID  string            `json:"conversation_id"`
	TurnOrdinal     int               `json:"turn_ordinal"`
	CreatedAt       string            `json:"created_at,omitempty"`
	Request         string            `json:"request,omitempty"`
	Text            string            `json:"text,omitempty"`
	RangeIncomplete bool              `json:"range_incomplete,omitempty"`
	ToolNames       []string          `json:"tool_names,omitempty"`
	Truncated       bool              `json:"truncated,omitempty"`
	Caveats         []retrievalCaveat `json:"caveats,omitempty"`
}

func conversationOutlineEntry(record historyRecord) conversationTurn {
	request, _, _ := strings.Cut(record.Summary, "\n")
	bounded, cut := truncateUTF8Bytes(strings.TrimSpace(request), conversationOutlineRequestMaxBytes)
	return conversationTurn{
		ConversationID:  record.ID,
		TurnOrdinal:     record.TurnOrdinal,
		CreatedAt:       record.CreatedAt,
		Request:         bounded,
		RangeIncomplete: record.RangeIncomplete,
		ToolNames:       record.ToolNames,
		Truncated:       cut || record.ProjectionTruncated,
	}
}

func turnJSONBytes(turn conversationTurn) int {
	data, err := json.Marshal(turn)
	if err != nil {
		return conversationOutlineRequestMaxBytes
	}
	return len(data)
}

// conversationSessionOutline renders one bounded outline page: entries with
// TurnOrdinal > afterTurn, at most limit, under the page byte cap, with a
// stable next_turn cursor whenever more entries exist. Appends to the session
// never shift an existing page because the cursor is ordinal-based.
func conversationSessionOutline(view conversationSessionView, afterTurn, limit int) unifiedResult {
	if limit <= 0 {
		limit = conversationOutlineDefaultLimit
	}
	if limit > conversationOutlineMaxLimit {
		limit = conversationOutlineMaxLimit
	}
	result := unifiedResult{
		Source:               retrievalSourceConversation,
		ID:                   view.Ref,
		Heading:              "session_outline",
		SessionRef:           view.Ref,
		VerificationRequired: true,
		Caveats:              []retrievalCaveat{conversationHistoricalEvidenceCaveat()},
	}
	total := 0
	budget := conversationOutlinePageMaxBytes
	var turns []conversationTurn
	lastOrdinal := afterTurn
	more := false
	for _, record := range view.Records {
		if record.TurnOrdinal <= afterTurn {
			continue
		}
		if len(turns) >= limit {
			more = true
			break
		}
		entry := conversationOutlineEntry(record)
		cost := turnJSONBytes(entry)
		if total+cost > budget {
			more = true
			result.PacketTruncated = true
			break
		}
		total += cost
		turns = append(turns, entry)
		lastOrdinal = record.TurnOrdinal
	}
	result.Turns = turns
	if more {
		result.NextTurn = lastOrdinal
	}
	if len(turns) == 0 {
		result.Text = "session outline: no exchanges after the requested turn"
	} else {
		result.Text = fmt.Sprintf("session outline: %d of %d exchanges (ordinals %d..%d)", len(turns), len(view.Records), turns[0].TurnOrdinal, turns[len(turns)-1].TurnOrdinal)
	}
	return result
}

// conversationContextPacket expands the target exchange plus up to
// before/after adjacent exchanges from the same reconciled session, dropping
// context farthest-first when the packet byte cap is reached. A degraded
// neighbor becomes a structured entry with its caveat; it never suppresses
// the target.
func conversationContextPacket(brainDir string, view conversationSessionView, target historyRecord, before, after int) unifiedResult {
	result := conversationGetResult(brainDir, target)
	result.SessionRef = view.Ref
	result.TargetID = target.ID
	targetIndex := -1
	for i, record := range view.Records {
		if record.ID == target.ID {
			targetIndex = i
			break
		}
	}
	if targetIndex < 0 {
		return result
	}
	availableBefore := targetIndex
	availableAfter := len(view.Records) - targetIndex - 1
	wantBefore := min(before, availableBefore)
	wantAfter := min(after, availableAfter)
	result.OmittedBefore = before - wantBefore
	result.OmittedAfter = after - wantAfter

	expand := func(record historyRecord) conversationTurn {
		expanded := conversationGetResult(brainDir, record)
		return conversationTurn{
			ConversationID:  record.ID,
			TurnOrdinal:     record.TurnOrdinal,
			CreatedAt:       record.CreatedAt,
			Text:            expanded.Text,
			RangeIncomplete: record.RangeIncomplete,
			ToolNames:       record.ToolNames,
			Truncated:       expanded.Truncated,
			Caveats:         expanded.Caveats,
		}
	}
	type contextEntry struct {
		turn     conversationTurn
		distance int
		index    int
	}
	var entries []contextEntry
	for offset := 1; offset <= wantBefore; offset++ {
		entries = append(entries, contextEntry{turn: expand(view.Records[targetIndex-offset]), distance: offset, index: targetIndex - offset})
	}
	for offset := 1; offset <= wantAfter; offset++ {
		entries = append(entries, contextEntry{turn: expand(view.Records[targetIndex+offset]), distance: offset, index: targetIndex + offset})
	}
	// Farthest-first eviction under the packet cap; the target is never
	// evicted.
	budget := conversationPacketMaxBytes - len(result.Text)
	for {
		total := 0
		for _, entry := range entries {
			total += turnJSONBytes(entry.turn)
		}
		if total <= budget || len(entries) == 0 {
			break
		}
		farthest := 0
		for i, entry := range entries {
			if entry.distance > entries[farthest].distance {
				farthest = i
			}
		}
		if entries[farthest].index < targetIndex {
			result.OmittedBefore++
		} else {
			result.OmittedAfter++
		}
		entries = append(entries[:farthest], entries[farthest+1:]...)
		result.PacketTruncated = true
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].index < entries[j].index })
	for _, entry := range entries {
		if entry.index < targetIndex {
			result.ContextBefore++
		} else {
			result.ContextAfter++
		}
		result.Turns = append(result.Turns, entry.turn)
	}
	return result
}

// printConversationTurns renders outline or context entries for the text
// surface; JSON callers get the structured turns directly.
func printConversationTurns(out io.Writer, r unifiedResult) {
	for _, turn := range r.Turns {
		body := turn.Request
		if body == "" {
			body = turn.Text
		}
		fmt.Fprintf(out, "  turn %d [%s] %s\n", turn.TurnOrdinal, turn.ConversationID, strings.Join(strings.Fields(body), " "))
		for _, caveat := range turn.Caveats {
			if caveat.Kind != retrievalCaveatHistoricalConversation {
				fmt.Fprintf(out, "    caveat %s: %s\n", caveat.Kind, caveat.Message)
			}
		}
	}
	if r.NextTurn > 0 {
		fmt.Fprintf(out, "  next turn: %d\n", r.NextTurn)
	}
	if r.PacketTruncated {
		fmt.Fprintln(out, "  packet truncated: byte cap reached")
	}
}

// getOptions is the navigation contract for get: context counts for a
// conversation: target, cursor and limit for a conversation-session: target.
// Options are type-specific and never silently ignored.
type getOptions struct {
	ContextBefore int
	ContextAfter  int
	AfterTurn     int
	OutlineLimit  int
	// set marks which options the caller supplied explicitly.
	ContextSet bool
	OutlineSet bool
	// GlobalFacts are facts from the global store, so an id that query or
	// search returned can be fetched back by get. Nil means the caller did not
	// offer them and get sees only this repository's facts, as before.
	GlobalFacts []factRecord
}

func (o getOptions) navigation() bool { return o.ContextSet || o.OutlineSet }

func validateGetOptions(o getOptions) error {
	if o.ContextBefore < 0 || o.ContextBefore > conversationContextMax || o.ContextAfter < 0 || o.ContextAfter > conversationContextMax {
		return fmt.Errorf("context-before/context-after must be between 0 and %d", conversationContextMax)
	}
	if o.AfterTurn < 0 {
		return fmt.Errorf("after-turn must be non-negative")
	}
	if o.OutlineLimit < 0 || o.OutlineLimit > conversationOutlineMaxLimit {
		return fmt.Errorf("outline limit must be between 1 and %d", conversationOutlineMaxLimit)
	}
	return nil
}
