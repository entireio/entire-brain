package cli

// This file contains the offline Phase 2 review seam.  It deliberately takes
// already-exported sessions and canonical transcript bytes: it has no storage,
// provider, cache, or filesystem dependencies.  The public side of the seam
// is safe to hand to a reviewer; the private side is retained separately for
// joining a review decision back to its source later.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const (
	distillQualityBundleSchemaVersionV1    = 1
	distillQualityBundleDefaultSampleV1    = 500
	distillQualityBundleDefaultItemBytesV1 = 40 << 10
	distillQualityBundleDefaultRunBytesV1  = 32 << 20
	distillQualityBundleMaxItemsV1         = 100000
)

// distillQualityBundleInputV1 is a pure input value. Content must be the
// canonical transcript corresponding to Session; it is never copied to the
// public result.
type distillQualityBundleInputV1 struct {
	Session exportSession
	Branch  string
	Content string
}

// DistillQualityBundleInputV1 is the exported spelling for callers that build
// review bundles outside the cli package's tests.
type DistillQualityBundleInputV1 = distillQualityBundleInputV1

type distillQualityBundleConfigV1 struct {
	MaxFilteredItems int
	MaxItemBytes     int
	MaxRunBytes      int
}

type DistillQualityBundleConfigV1 = distillQualityBundleConfigV1

// distillQualityAdmissionItemV1 is intentionally small and source-free. Text
// in Evidence is bounded and redacted; IDs and Digest are opaque hashes.
type distillQualityAdmissionItemV1 struct {
	ID                string   `json:"id"`
	Digest            string   `json:"digest"`
	Stratum           string   `json:"stratum"`
	Roles             []string `json:"roles"`
	Authorities       []string `json:"authorities"`
	Cues              []string `json:"cues"`
	Evidence          string   `json:"evidence"`
	EvidenceBytes     int      `json:"evidence_bytes"`
	EvidenceTruncated bool     `json:"evidence_truncated,omitempty"`
	Admitted          bool     `json:"admitted"`
}

type DistillQualityAdmissionItemV1 = distillQualityAdmissionItemV1

// distillQualityPrivateSourceV1 must not be embedded in or serialized with
// the public review payload. It is a separate type by design.
type distillQualityPrivateSourceV1 struct {
	PublicID       string
	SessionID      string
	Branch         string
	CheckpointID   string
	TranscriptPath string
	TurnIDs        []string
	StartLine      int
	EndLine        int
}

type DistillQualityPrivateSourceV1 = distillQualityPrivateSourceV1

type distillQualityBundleV1 struct {
	SchemaVersion int                             `json:"schema_version"`
	RunID         string                          `json:"run_id"`
	Admission     []distillQualityAdmissionItemV1 `json:"admission"`
	Filtered      []distillQualityAdmissionItemV1 `json:"filtered"`
	Sources       []distillQualityPrivateSourceV1 `json:"-"`
}

type DistillQualityBundleV1 = distillQualityBundleV1

func defaultDistillQualityBundleConfigV1() distillQualityBundleConfigV1 {
	return distillQualityBundleConfigV1{
		MaxFilteredItems: distillQualityBundleDefaultSampleV1,
		MaxItemBytes:     distillQualityBundleDefaultItemBytesV1,
		MaxRunBytes:      distillQualityBundleDefaultRunBytesV1,
	}
}

// buildDistillQualityBundleV1 builds a bundle for one or more sessions. The
// output order is independent of input order, making repeated adjudication
// runs byte-for-byte comparable.
func buildDistillQualityBundleV1(inputs []distillQualityBundleInputV1, config distillQualityBundleConfigV1) (distillQualityBundleV1, error) {
	config = normalizeDistillQualityBundleConfigV1(config)
	if len(inputs) > distillQualityBundleMaxItemsV1 {
		return distillQualityBundleV1{}, fmt.Errorf("quality bundle has too many sessions: %d", len(inputs))
	}
	var admitted, filtered []distillQualityWorkV1
	for _, input := range inputs {
		branch := strings.TrimSpace(input.Branch)
		if branch == "" {
			branch = strings.TrimSpace(input.Session.Branch)
		}
		normalized := normalizeDistillTranscriptV1(input.Session, branch, input.Content)
		if normalized.Overflow || normalized.Unsupported || !normalized.Recognized {
			return distillQualityBundleV1{}, fmt.Errorf("quality bundle cannot normalize session %q", strings.TrimSpace(input.Session.SessionID))
		}
		cards, overflow := selectDistillCandidateCardsLimitedV1(normalized, distillCandidateMaxCards)
		if overflow {
			return distillQualityBundleV1{}, fmt.Errorf("quality bundle session exceeds candidate card bound")
		}
		admittedTurns := make(map[string]bool)
		for _, card := range cards {
			for _, turn := range card.Turns {
				admittedTurns[turn.ID] = true
			}
			for _, trigger := range card.Triggers {
				admittedTurns[trigger.TurnID] = true
			}
			item, source, err := distillQualityAdmittedItemV1(card, normalized.Source, config.MaxItemBytes)
			if err != nil {
				return distillQualityBundleV1{}, err
			}
			admitted = append(admitted, distillQualityWorkV1{item: item, source: source})
		}
		for _, turn := range normalized.Turns {
			if admittedTurns[turn.ID] {
				continue
			}
			item, source, err := distillQualityFilteredItemV1(turn, normalized.Source, config.MaxItemBytes)
			if err != nil {
				return distillQualityBundleV1{}, err
			}
			stratum := string(turn.Role) + ":"
			if turn.Role == distillTranscriptRoleUserV1 && turn.DirectUser {
				stratum += "direct"
			} else {
				stratum += "context"
			}
			item.Stratum = stratum
			filtered = append(filtered, distillQualityWorkV1{item: item, source: source, stratum: stratum})
		}
	}
	// Identical redacted cards are one human-review item. Every source anchor is
	// retained privately, so deduplication never loses provenance.
	allAdmittedSources := make(map[string][]distillQualityPrivateSourceV1)
	allFilteredSources := make(map[string][]distillQualityPrivateSourceV1)
	for _, work := range admitted {
		allAdmittedSources[work.item.ID] = append(allAdmittedSources[work.item.ID], work.source)
	}
	for _, work := range filtered {
		allFilteredSources[work.item.ID] = append(allFilteredSources[work.item.ID], work.source)
	}
	admitted = dedupeDistillQualityAdmittedV1(admitted)
	filtered = dedupeDistillQualityWorkV1(filtered)
	sort.Slice(admitted, func(i, j int) bool { return admitted[i].item.ID < admitted[j].item.ID })
	selected := stratifiedDistillQualitySampleV1(filtered, config.MaxFilteredItems)
	sort.Slice(selected, func(i, j int) bool { return selected[i].item.ID < selected[j].item.ID })
	out := distillQualityBundleV1{SchemaVersion: distillQualityBundleSchemaVersionV1, Admission: make([]distillQualityAdmissionItemV1, len(admitted)), Filtered: make([]distillQualityAdmissionItemV1, len(selected))}
	for i := range admitted {
		out.Admission[i] = admitted[i].item
	}
	for i := range selected {
		out.Filtered[i] = selected[i].item
	}
	out.RunID = distillQualityBundleRunIDV1(out.Admission, out.Filtered)
	for _, w := range admitted {
		out.Sources = append(out.Sources, allAdmittedSources[w.item.ID]...)
	}
	for _, w := range selected {
		out.Sources = append(out.Sources, allFilteredSources[w.item.ID]...)
	}
	sort.Slice(out.Sources, func(i, j int) bool {
		left, right := out.Sources[i], out.Sources[j]
		leftKey := strings.Join([]string{left.PublicID, left.SessionID, left.Branch, left.CheckpointID, left.TranscriptPath, strings.Join(left.TurnIDs, "\x00")}, "\x00")
		rightKey := strings.Join([]string{right.PublicID, right.SessionID, right.Branch, right.CheckpointID, right.TranscriptPath, strings.Join(right.TurnIDs, "\x00")}, "\x00")
		return leftKey < rightKey
	})
	if err := validateDistillQualityPublicV1(out, out.Sources, config.MaxRunBytes, config.MaxItemBytes); err != nil {
		return distillQualityBundleV1{}, err
	}
	return out, nil
}

func BuildDistillQualityBundleV1(inputs []DistillQualityBundleInputV1, config DistillQualityBundleConfigV1) (DistillQualityBundleV1, error) {
	return buildDistillQualityBundleV1(inputs, config)
}

// buildDistillQualityBundleForSessionV1 is the single-session convenience
// adapter used by evaluators that already have one export row in hand.
func buildDistillQualityBundleForSessionV1(session exportSession, branch, content string, config distillQualityBundleConfigV1) (distillQualityBundleV1, error) {
	return buildDistillQualityBundleV1([]distillQualityBundleInputV1{{Session: session, Branch: branch, Content: content}}, config)
}

func BuildDistillQualityBundleForSessionV1(session exportSession, branch, content string, config distillQualityBundleConfigV1) (distillQualityBundleV1, error) {
	return buildDistillQualityBundleForSessionV1(session, branch, content, config)
}

func normalizeDistillQualityBundleConfigV1(c distillQualityBundleConfigV1) distillQualityBundleConfigV1 {
	d := defaultDistillQualityBundleConfigV1()
	if c.MaxFilteredItems > 0 {
		d.MaxFilteredItems = c.MaxFilteredItems
	}
	if c.MaxItemBytes > 0 {
		d.MaxItemBytes = c.MaxItemBytes
	}
	if c.MaxRunBytes > 0 {
		d.MaxRunBytes = c.MaxRunBytes
	}
	return d
}

func distillQualityAdmittedItemV1(card distillCandidateCardV1, source distillCandidateSourceV1, maxBytes int) (distillQualityAdmissionItemV1, distillQualityPrivateSourceV1, error) {
	var roles, authorities, cues []string
	var evidence []string
	var ids []string
	for _, trigger := range card.Triggers {
		roles = append(roles, string(trigger.Role))
		authorities = append(authorities, distillQualityAuthorityV1(trigger))
		cues = append(cues, cueStringsV1(trigger.Cues...)...)
		ids = append(ids, trigger.TurnID)
	}
	for _, turn := range card.Turns {
		evidence = append(evidence, string(turn.Role)+": "+turn.Text)
		ids = append(ids, turn.ID)
	}
	redactedEvidence := redactText(strings.Join(evidence, "\n"))
	item := distillQualityAdmissionItemV1{Digest: strings.Repeat("0", 64), Stratum: distillQualityAdmittedStratumV1(cues, authorities), Roles: uniqueSortedStringsV1(roles), Authorities: uniqueSortedStringsV1(authorities), Cues: uniqueSortedStringsV1(cues), Evidence: redactedEvidence, EvidenceBytes: len(redactedEvidence), Admitted: true}
	if err := boundDistillQualityItemV1(&item, maxBytes, false); err != nil {
		return item, distillQualityPrivateSourceV1{}, err
	}
	item.Digest = distillQualityDigestV1(item.Evidence)
	item.ID = distillQualityContentIDV1("admitted", item)
	return item, distillQualityPrivateSourceV1{PublicID: item.ID, SessionID: source.SessionID, Branch: source.Branch, CheckpointID: source.LatestCheckpoint, TranscriptPath: source.Transcript, TurnIDs: append([]string(nil), ids...), StartLine: card.StartLine, EndLine: card.EndLine}, nil
}

func distillQualityFilteredItemV1(turn distillNormalizedTurnV1, source distillCandidateSourceV1, maxBytes int) (distillQualityAdmissionItemV1, distillQualityPrivateSourceV1, error) {
	authority := "context_only"
	if turn.Role == distillTranscriptRoleUserV1 && turn.DirectUser {
		authority = "direct_user_context"
	}
	evidence := redactText(string(turn.Role) + ": " + turn.Text)
	item := distillQualityAdmissionItemV1{ID: distillQualityOpaqueIDV1("filtered", source, turn.ID), Digest: distillQualityDigestV1(evidence), Roles: []string{string(turn.Role)}, Authorities: []string{authority}, Cues: []string{}, Evidence: evidence, EvidenceBytes: len(evidence), Admitted: false}
	if err := boundDistillQualityItemV1(&item, maxBytes, true); err != nil {
		return item, distillQualityPrivateSourceV1{}, err
	}
	return item, distillQualityPrivateSourceV1{PublicID: item.ID, SessionID: source.SessionID, Branch: source.Branch, CheckpointID: source.LatestCheckpoint, TranscriptPath: source.Transcript, TurnIDs: []string{turn.ID}, StartLine: turn.StartLine, EndLine: turn.EndLine}, nil
}

type distillQualityWorkV1 struct {
	item    distillQualityAdmissionItemV1
	source  distillQualityPrivateSourceV1
	stratum string
}

func dedupeDistillQualityAdmittedV1(items []distillQualityWorkV1) []distillQualityWorkV1 {
	return dedupeDistillQualityWorkV1(items)
}

func dedupeDistillQualityWorkV1(items []distillQualityWorkV1) []distillQualityWorkV1 {
	unique := make([]distillQualityWorkV1, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		if seen[item.item.ID] {
			continue
		}
		seen[item.item.ID] = true
		unique = append(unique, item)
	}
	return unique
}

func stratifiedDistillQualitySampleV1(items []distillQualityWorkV1, limit int) []distillQualityWorkV1 {
	if limit <= 0 || len(items) <= limit {
		return append([]distillQualityWorkV1(nil), items...)
	}
	strata := make(map[string][]distillQualityWorkV1)
	for _, item := range items {
		strata[item.stratum] = append(strata[item.stratum], item)
	}
	keys := make([]string, 0, len(strata))
	for key := range strata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		sort.Slice(strata[key], func(i, j int) bool { return strata[key][i].item.ID < strata[key][j].item.ID })
	}
	out := make([]distillQualityWorkV1, 0, limit)
	for len(out) < limit {
		progress := false
		for _, key := range keys {
			if len(strata[key]) == 0 {
				continue
			}
			out = append(out, strata[key][0])
			strata[key] = strata[key][1:]
			progress = true
			if len(out) == limit {
				break
			}
		}
		if !progress {
			break
		}
	}
	return out
}

func distillQualityAuthorityV1(trigger distillCandidateTriggerV1) string {
	if trigger.Authoritative {
		return "direct_user"
	}
	if trigger.CorroborationEligible {
		return "assistant_corroboration"
	}
	return "context_only"
}

func distillQualityAdmittedStratumV1(cues, authorities []string) string {
	cleanCues := uniqueSortedStringsV1(cues)
	cleanAuthorities := uniqueSortedStringsV1(authorities)
	if len(cleanCues) == 0 {
		cleanCues = []string{"uncued"}
	}
	if len(cleanAuthorities) == 0 {
		cleanAuthorities = []string{"unknown"}
	}
	return "admitted:" + strings.Join(cleanAuthorities, "+") + ":" + strings.Join(cleanCues, "+")
}

func cueStringsV1(cues ...distillCandidateCueV1) []string {
	out := make([]string, len(cues))
	for i, cue := range cues {
		out[i] = string(cue)
	}
	return out
}

func uniqueSortedStringsV1(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, value := range in {
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func distillQualityOpaqueIDV1(kind string, source distillCandidateSourceV1, parts ...string) string {
	material := append([]string{kind, distillCandidateSourceIdentityV1(source)}, parts...)
	return "quality-" + kind + "-v1:" + distillQualityDigestV1(strings.Join(material, "\x00"))
}

func distillQualityDigestV1(values ...string) string {
	h := sha256.New()
	for _, value := range values {
		h.Write([]byte(value))
		h.Write([]byte{'\x00'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func distillQualityContentIDV1(kind string, item distillQualityAdmissionItemV1) string {
	material, _ := json.Marshal(struct {
		Kind                     string
		Digest                   string
		Stratum                  string
		Roles, Authorities, Cues []string
		Evidence                 string
		EvidenceBytes            int
		EvidenceTruncated        bool
		Admitted                 bool
	}{kind, item.Digest, item.Stratum, item.Roles, item.Authorities, item.Cues, item.Evidence, item.EvidenceBytes, item.EvidenceTruncated, item.Admitted})
	return "quality-" + kind + "-v1:" + distillQualityDigestV1(string(material))
}

func boundDistillQualityItemV1(item *distillQualityAdmissionItemV1, maxBytes int, allowTruncation bool) error {
	if maxBytes <= 0 {
		maxBytes = distillQualityBundleDefaultItemBytesV1
	}
	for {
		data, err := json.Marshal(item)
		if err != nil {
			return err
		}
		if len(data) <= maxBytes {
			return nil
		}
		if !allowTruncation {
			return fmt.Errorf("quality admitted item %s exceeds %d bytes; complete trigger evidence is required", item.ID, maxBytes)
		}
		runes := []rune(item.Evidence)
		if len(runes) < 32 {
			return fmt.Errorf("quality item %s exceeds %d bytes", item.ID, maxBytes)
		}
		keep := len(runes) * 3 / 4
		if keep < 16 {
			keep = 16
		}
		head := keep / 2
		tail := keep - head
		item.Evidence = string(runes[:head]) + "…" + string(runes[len(runes)-tail:])
		item.EvidenceTruncated = true
	}
}

func distillQualityBundleRunIDV1(admitted, filtered []distillQualityAdmissionItemV1) string {
	var b strings.Builder
	for _, item := range admitted {
		b.WriteString(item.ID)
		b.WriteByte('\n')
	}
	b.WriteByte('|')
	for _, item := range filtered {
		b.WriteString(item.ID)
		b.WriteByte('\n')
	}
	return "quality-run-v1:" + distillQualityDigestV1(b.String())
}

func validateDistillQualityPublicV1(bundle distillQualityBundleV1, sources []distillQualityPrivateSourceV1, maxRunBytes, maxItemBytes int) error {
	data, err := json.Marshal(struct {
		SchemaVersion int                             `json:"schema_version"`
		RunID         string                          `json:"run_id"`
		Admission     []distillQualityAdmissionItemV1 `json:"admission"`
		Filtered      []distillQualityAdmissionItemV1 `json:"filtered"`
	}{bundle.SchemaVersion, bundle.RunID, bundle.Admission, bundle.Filtered})
	if err != nil {
		return err
	}
	if len(data) > maxRunBytes {
		return fmt.Errorf("quality bundle exceeds %d bytes", maxRunBytes)
	}
	if len(bundle.Admission)+len(bundle.Filtered) > distillQualityBundleMaxItemsV1 {
		return fmt.Errorf("quality bundle item count exceeds %d", distillQualityBundleMaxItemsV1)
	}
	_ = sources // Source provenance is structurally excluded by json:"-".
	for _, item := range append(append([]distillQualityAdmissionItemV1{}, bundle.Admission...), bundle.Filtered...) {
		if item.ID == "" || item.Digest == "" || item.Evidence != redactText(item.Evidence) {
			return fmt.Errorf("quality item %s is not safely redacted", item.ID)
		}
		if item.Stratum == "" || len(item.Roles) == 0 || len(item.Authorities) == 0 {
			return fmt.Errorf("quality item %s lacks authority metadata", item.ID)
		}
		if item.EvidenceBytes < len(item.Evidence) || item.EvidenceTruncated != (item.EvidenceBytes > len(item.Evidence)) {
			return fmt.Errorf("quality item %s has inconsistent evidence bounds", item.ID)
		}
		if maxItemBytes > 0 {
			itemData, marshalErr := json.Marshal(item)
			if marshalErr != nil {
				return marshalErr
			}
			if len(itemData) > maxItemBytes {
				return fmt.Errorf("quality item %s exceeds %d bytes", item.ID, maxItemBytes)
			}
		}
	}
	return nil
}
