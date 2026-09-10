package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	distillDiscoveryCacheVersionV1  = 1
	distillDiscoveryCacheRelV1      = factsDirName + "/distill-v2/discovery.json"
	distillDiscoveryCacheMaxBytesV1 = 64 << 20
)

type distillDiscoveryCacheV1 struct {
	Version         int                                     `json:"version"`
	CandidateSchema int                                     `json:"candidate_schema"`
	Entries         map[string]distillDiscoveryCacheEntryV1 `json:"entries"`
}

type distillDiscoveryCacheEntryV1 struct {
	SourceSHA256   string                        `json:"source_sha256"`
	SourceFormat   string                        `json:"source_format,omitempty"`
	SourceBytes    int                           `json:"source_bytes"`
	SourceLines    int                           `json:"source_lines"`
	Complete       bool                          `json:"complete_newline"`
	EvidenceSHA256 string                        `json:"evidence_sha256"`
	Turns          []distillDiscoveryTurnV1      `json:"turns"`
	Cards          []distillDiscoveryCandidateV1 `json:"cards"`
}

type distillDiscoveryTurnV1 struct {
	ID          string                      `json:"id"`
	Role        distillTranscriptRoleV1     `json:"role"`
	Origins     []distillTranscriptOriginV1 `json:"origins"`
	SourceLines []int                       `json:"source_lines"`
	StartLine   int                         `json:"start_line"`
	EndLine     int                         `json:"end_line"`
	TextSHA256  string                      `json:"text_sha256"`
	DirectUser  bool                        `json:"direct_user,omitempty"`
}

type distillDiscoveryCandidateV1 struct {
	ID         string                      `json:"id"`
	StartLine  int                         `json:"start_line"`
	EndLine    int                         `json:"end_line"`
	Triggers   []distillCandidateTriggerV1 `json:"triggers"`
	Evidence   distillCandidateEvidenceV1  `json:"evidence,omitempty"`
	TurnIDs    []string                    `json:"turn_ids"`
	TurnSHA256 []string                    `json:"turn_sha256"`
}

func newDistillDiscoveryCacheV1() distillDiscoveryCacheV1 {
	return distillDiscoveryCacheV1{Version: distillDiscoveryCacheVersionV1, CandidateSchema: distillCandidateSchemaVersion, Entries: map[string]distillDiscoveryCacheEntryV1{}}
}

func distillDiscoveryDigestV1(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func distillDiscoveryEvidenceDigestV1(session exportSession) string {
	files := normalizeDistillCandidateFilesTouchedV1(session.FilesTouched)
	sort.Strings(files)
	material, _ := json.Marshal(struct {
		DirectUser   bool     `json:"direct_user"`
		AgentReview  bool     `json:"agent_review"`
		CheckpointOK bool     `json:"checkpoint_ok"`
		FilesTouched []string `json:"files_touched"`
	}{distillCandidateSessionHasDirectUserAuthorityV1(session), distillCandidateSessionIsAgentReviewV1(session), distillCandidateCheckpointSucceededV1(session), files})
	return distillDiscoveryDigestV1(string(material))
}

func buildDistillDiscoveryEntryV1(session exportSession, content string, normalized distillNormalizedTranscriptV1, cards []distillCandidateCardV1) distillDiscoveryCacheEntryV1 {
	format := ""
	if directDistillDiscoveryJSONLV1(content) {
		format = "jsonl-v1"
	}
	entry := distillDiscoveryCacheEntryV1{SourceSHA256: distillDiscoveryDigestV1(content), SourceFormat: format, SourceBytes: len(content), SourceLines: strings.Count(content, "\n"), Complete: strings.HasSuffix(content, "\n"), EvidenceSHA256: distillDiscoveryEvidenceDigestV1(session)}
	for _, turn := range normalized.Turns {
		entry.Turns = append(entry.Turns, distillDiscoveryTurnV1{ID: turn.ID, Role: turn.Role, Origins: append([]distillTranscriptOriginV1(nil), turn.Origins...), SourceLines: append([]int(nil), turn.SourceLines...), StartLine: turn.StartLine, EndLine: turn.EndLine, TextSHA256: distillDiscoveryDigestV1(turn.Text), DirectUser: turn.DirectUser})
	}
	for _, card := range cards {
		candidate := distillDiscoveryCandidateV1{ID: card.ID, StartLine: card.StartLine, EndLine: card.EndLine, Triggers: append([]distillCandidateTriggerV1(nil), card.Triggers...), Evidence: card.Evidence}
		for _, turn := range card.Turns {
			candidate.TurnIDs = append(candidate.TurnIDs, turn.ID)
			candidate.TurnSHA256 = append(candidate.TurnSHA256, distillDiscoveryDigestV1(turn.Text))
		}
		entry.Cards = append(entry.Cards, candidate)
	}
	return entry
}

type distillDiscoveryReuseStatsV1 struct{ ExactHits, AppendHits, ParsedRecords, FullFallbacks, CorruptRebuilds int }

func discoveryStatsFromOptionsV1(opts distillCommandOptions) distillDiscoveryReuseStatsV1 {
	if opts.discoveryStats == nil {
		return distillDiscoveryReuseStatsV1{}
	}
	return *opts.discoveryStats
}

func applyDistillDiscoveryStatsV1(source *factSourceManifest, stats distillDiscoveryReuseStatsV1) {
	if source == nil {
		return
	}
	source.DiscoveryExactHits, source.DiscoveryAppendResumed = stats.ExactHits, stats.AppendHits
	source.DiscoveryFullRescans, source.DiscoveryNormalizedRecords = stats.FullFallbacks, stats.ParsedRecords
	source.DiscoveryCorruptRebuilds = stats.CorruptRebuilds
}

func appendDistillDiscoveryV1(session exportSession, branch, content string, entry distillDiscoveryCacheEntryV1) ([]distillCandidateCardV1, distillDiscoveryCacheEntryV1, int, bool) {
	if entry.SourceFormat != "jsonl-v1" || !entry.Complete || entry.SourceBytes <= 0 || entry.SourceBytes >= len(content) || !strings.HasSuffix(content, "\n") || entry.EvidenceSHA256 != distillDiscoveryEvidenceDigestV1(session) || distillDiscoveryDigestV1(content[:entry.SourceBytes]) != entry.SourceSHA256 {
		return nil, entry, 0, false
	}
	suffix := content[entry.SourceBytes:]
	if strings.TrimSpace(suffix) == "" {
		return nil, entry, 0, false
	}
	next := normalizeDistillTranscriptV1(session, branch, suffix)
	if !next.Recognized || next.Unsupported || next.Overflow {
		return nil, entry, 0, false
	}
	priorCounts := map[string]int{}
	for _, turn := range entry.Turns {
		priorCounts[string(turn.Role)+"\x00"+turn.TextSHA256]++
	}
	for index := range next.Turns {
		turn := &next.Turns[index]
		turn.StartLine += entry.SourceLines
		turn.EndLine += entry.SourceLines
		for lineIndex := range turn.SourceLines {
			turn.SourceLines[lineIndex] += entry.SourceLines
		}
		key := string(turn.Role) + "\x00" + distillDiscoveryDigestV1(turn.Text)
		priorCounts[key]++
		turn.ID = distillCandidateStableIDV1("turn-v1:", distillCandidateSourceIdentityV1(next.Source), string(turn.Role), turn.Text, fmt.Sprintf("%d", priorCounts[key]))
	}
	window := next
	if len(entry.Turns) > 0 {
		cached := entry.Turns[len(entry.Turns)-1]
		if prior, ok := reconstructDistillDiscoveryTurnV1(session, content[:entry.SourceBytes], cached); ok {
			window.Turns = append([]distillNormalizedTurnV1{prior}, window.Turns...)
		} else {
			return nil, entry, 0, false
		}
	}
	discovered, overflow := selectDistillCandidateCardsLimitedV1(window, distillCandidateMaxCards)
	if overflow {
		return nil, entry, 0, false
	}
	newIDs := map[string]bool{}
	for _, turn := range next.Turns {
		newIDs[turn.ID] = true
	}
	boundaryID := ""
	if len(entry.Turns) > 0 {
		boundaryID = entry.Turns[len(entry.Turns)-1].ID
	}
	cards, restored := restoreDistillDiscoveryV1(session, branch, content[:entry.SourceBytes], entry)
	if !restored {
		return nil, entry, 0, false
	}
	kept := cards[:0]
	for _, card := range cards {
		affected := false
		for _, trigger := range card.Triggers {
			affected = affected || trigger.TurnID == boundaryID
		}
		if !affected {
			kept = append(kept, card)
		}
	}
	cards = kept
	for _, card := range discovered {
		include := false
		for _, trigger := range card.Triggers {
			include = include || newIDs[trigger.TurnID] || trigger.TurnID == boundaryID
		}
		if include {
			cards = append(cards, card)
		}
	}
	allTurns := append([]distillDiscoveryTurnV1(nil), entry.Turns...)
	for _, turn := range next.Turns {
		allTurns = append(allTurns, distillDiscoveryTurnV1{ID: turn.ID, Role: turn.Role, Origins: append([]distillTranscriptOriginV1(nil), turn.Origins...), SourceLines: append([]int(nil), turn.SourceLines...), StartLine: turn.StartLine, EndLine: turn.EndLine, TextSHA256: distillDiscoveryDigestV1(turn.Text), DirectUser: turn.DirectUser})
	}
	updated := buildDistillDiscoveryEntryV1(session, content, distillNormalizedTranscriptV1{Turns: next.Turns}, cards)
	updated.Turns = allTurns
	return cards, updated, strings.Count(suffix, "\n"), true
}

func directDistillDiscoveryJSONLV1(content string) bool {
	found := false
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(line), &obj) != nil || obj["messages"] != nil || !isRecognizedDistillCandidateRecordV1(obj) {
			return false
		}
		found = true
	}
	return found
}

func reconstructDistillDiscoveryTurnV1(session exportSession, content string, cached distillDiscoveryTurnV1) (distillNormalizedTurnV1, bool) {
	lines := strings.Split(content, "\n")
	for _, lineNo := range cached.SourceLines {
		if lineNo < 1 || lineNo > len(lines) {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(lines[lineNo-1])), &obj) != nil {
			continue
		}
		raw, ok := distillCandidateTurnFromJSONV1(obj, lineNo, distillCandidateSessionHasDirectUserAuthorityV1(session))
		if ok && raw.Role == cached.Role && distillDiscoveryDigestV1(raw.Text) == cached.TextSHA256 {
			return distillNormalizedTurnV1{ID: cached.ID, Role: cached.Role, Origins: append([]distillTranscriptOriginV1(nil), cached.Origins...), SourceLines: append([]int(nil), cached.SourceLines...), StartLine: cached.StartLine, EndLine: cached.EndLine, Text: raw.Text, DirectUser: cached.DirectUser}, true
		}
	}
	return distillNormalizedTurnV1{}, false
}

func restoreDistillDiscoveryV1(session exportSession, branch, content string, entry distillDiscoveryCacheEntryV1) ([]distillCandidateCardV1, bool) {
	if entry.SourceSHA256 != distillDiscoveryDigestV1(content) || entry.EvidenceSHA256 != distillDiscoveryEvidenceDigestV1(session) {
		return nil, false
	}
	lines := strings.Split(content, "\n")
	wanted := map[string]bool{}
	for _, card := range entry.Cards {
		for _, id := range card.TurnIDs {
			wanted[id] = true
		}
	}
	turns := make(map[string]distillNormalizedTurnV1, len(wanted))
	for _, cached := range entry.Turns {
		if !wanted[cached.ID] {
			continue
		}
		var text string
		for _, lineNo := range cached.SourceLines {
			if lineNo < 1 || lineNo > len(lines) {
				return nil, false
			}
			var obj map[string]any
			if json.Unmarshal([]byte(strings.TrimSpace(lines[lineNo-1])), &obj) != nil {
				return nil, false
			}
			raw, ok := distillCandidateTurnFromJSONV1(obj, lineNo, distillCandidateSessionHasDirectUserAuthorityV1(session))
			if ok && raw.Role == cached.Role && distillDiscoveryDigestV1(raw.Text) == cached.TextSHA256 {
				text = raw.Text
				break
			}
		}
		if text == "" {
			return nil, false
		}
		turns[cached.ID] = distillNormalizedTurnV1{ID: cached.ID, Role: cached.Role, Origins: append([]distillTranscriptOriginV1(nil), cached.Origins...), SourceLines: append([]int(nil), cached.SourceLines...), StartLine: cached.StartLine, EndLine: cached.EndLine, Text: text, DirectUser: cached.DirectUser}
	}
	source := distillCandidateSourceV1{SessionID: strings.TrimSpace(session.SessionID), LatestCheckpoint: strings.TrimSpace(session.LatestCheckpoint), Branch: strings.TrimSpace(branch), Transcript: filepath.ToSlash(strings.TrimSpace(session.TranscriptPath))}
	cards := make([]distillCandidateCardV1, 0, len(entry.Cards))
	for _, cached := range entry.Cards {
		if len(cached.TurnIDs) != len(cached.TurnSHA256) {
			return nil, false
		}
		card := distillCandidateCardV1{SchemaVersion: distillCandidateSchemaVersion, ID: cached.ID, Source: source, StartLine: cached.StartLine, EndLine: cached.EndLine, Triggers: append([]distillCandidateTriggerV1(nil), cached.Triggers...), Evidence: cached.Evidence}
		for index, id := range cached.TurnIDs {
			turn, ok := turns[id]
			if !ok || distillDiscoveryDigestV1(turn.Text) != cached.TurnSHA256[index] {
				return nil, false
			}
			card.Turns = append(card.Turns, turn)
		}
		cards = append(cards, card)
	}
	return cards, true
}

func loadDistillDiscoveryCacheV1(brainDir string) distillDiscoveryCacheV1 {
	cache, _ := loadDistillDiscoveryCacheV1WithStatus(brainDir)
	return cache
}

func loadDistillDiscoveryCacheV1WithStatus(brainDir string) (distillDiscoveryCacheV1, bool) {
	data, err := readBrainRelativeStateFile(brainDir, distillDiscoveryCacheRelV1)
	if err != nil {
		return newDistillDiscoveryCacheV1(), !os.IsNotExist(err)
	}
	cache, parseErr := parseDistillDiscoveryCacheV1([]byte(data))
	if parseErr != nil {
		return newDistillDiscoveryCacheV1(), true
	}
	return cache, false
}

func parseDistillDiscoveryCacheV1(data []byte) (distillDiscoveryCacheV1, error) {
	if len(data) > distillDiscoveryCacheMaxBytesV1 {
		return distillDiscoveryCacheV1{}, fmt.Errorf("discovery cache exceeds %d bytes", distillDiscoveryCacheMaxBytesV1)
	}
	var cache distillDiscoveryCacheV1
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cache); err != nil {
		return distillDiscoveryCacheV1{}, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return distillDiscoveryCacheV1{}, fmt.Errorf("discovery cache has trailing data")
	}
	if cache.Version != distillDiscoveryCacheVersionV1 || cache.CandidateSchema != distillCandidateSchemaVersion || cache.Entries == nil {
		return distillDiscoveryCacheV1{}, fmt.Errorf("unsupported discovery cache version")
	}
	if len(cache.Entries) > 10000 {
		return distillDiscoveryCacheV1{}, fmt.Errorf("discovery cache has too many entries")
	}
	for id, entry := range cache.Entries {
		if strings.TrimSpace(id) == "" || len(id) > distillCandidateMaxSourceFieldBytes || (entry.SourceFormat != "" && entry.SourceFormat != "jsonl-v1") || len(entry.SourceSHA256) != 64 || len(entry.EvidenceSHA256) != 64 || len(entry.Turns) > distillCandidateMaxTurns || len(entry.Cards) > distillCandidateMaxCards {
			return distillDiscoveryCacheV1{}, fmt.Errorf("invalid discovery cache entry")
		}
		for _, turn := range entry.Turns {
			if turn.ID == "" || len(turn.ID) > 128 || len(turn.TextSHA256) != 64 || len(turn.SourceLines) == 0 || len(turn.SourceLines) > 16 || turn.StartLine < 1 || turn.EndLine < turn.StartLine {
				return distillDiscoveryCacheV1{}, fmt.Errorf("invalid discovery turn")
			}
		}
		for _, card := range entry.Cards {
			if card.ID == "" || len(card.TurnIDs) == 0 || len(card.TurnIDs) != len(card.TurnSHA256) || len(card.TurnIDs) > 3 || len(card.Triggers) == 0 || len(card.Triggers) > 1 {
				return distillDiscoveryCacheV1{}, fmt.Errorf("invalid discovery candidate")
			}
		}
	}
	return cache, nil
}

func purgeDistillDiscoveryCacheV1(brainDir string) error {
	err := os.Remove(filepath.Join(brainDir, filepath.FromSlash(distillDiscoveryCacheRelV1)))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func saveDistillDiscoveryCacheV1(brainDir string, cache distillDiscoveryCacheV1) error {
	data, err := marshalDistillDiscoveryCacheForWriteV1(cache, distillDiscoveryCacheMaxBytesV1)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(brainDir, filepath.FromSlash(distillDiscoveryCacheRelV1)), data, 0o600)
}

func marshalDistillDiscoveryCacheForWriteV1(cache distillDiscoveryCacheV1, maxBytes int) ([]byte, error) {
	if maxBytes <= 0 || maxBytes > distillDiscoveryCacheMaxBytesV1 {
		return nil, fmt.Errorf("invalid discovery cache write bound %d", maxBytes)
	}
	// The store is disposable performance state. Deterministically evict
	// entries instead of failing a run when retained metadata reaches its cap.
	// Measure the indented bytes plus final newline that are actually persisted.
	entries := make(map[string]distillDiscoveryCacheEntryV1, len(cache.Entries))
	for key, entry := range cache.Entries {
		entries[key] = entry
	}
	cache.Entries = entries
	keys := make([]string, 0, len(cache.Entries))
	for key := range cache.Entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	for len(cache.Entries) > 10000 || len(data) > maxBytes {
		if len(keys) == 0 {
			return nil, fmt.Errorf("discovery cache cannot fit bounded store")
		}
		delete(cache.Entries, keys[0])
		keys = keys[1:]
		data, err = json.MarshalIndent(cache, "", "  ")
		if err != nil {
			return nil, err
		}
		data = append(data, '\n')
	}
	if _, err := parseDistillDiscoveryCacheV1(data); err != nil {
		return nil, err
	}
	return data, nil
}
