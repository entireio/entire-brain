package cli

// Phase 3A application receipts are the small, content-free commit record for
// deterministic fact materialization.  Extraction results are cached
// separately; this file only records that one result was successfully applied
// to one branch/provenance view.  In particular, it must never become another
// card, transcript, or provider-response store.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
)

const (
	distillApplicationReceiptV2Version = 1
	distillApplicationReceiptsV2Path   = factsDirName + "/distill-v2/applications.ndjson"

	// These limits are deliberately independent of transcript and provider
	// limits.  Receipt data is expected to remain compact even when a fact store
	// has many branches or an application unions several provenance anchors.
	distillApplicationReceiptsV2MaxBytes   = 8 << 20
	distillApplicationReceiptsV2MaxEntries = 4096
	distillApplicationReceiptsV2MaxLine    = 32 << 10
	distillApplicationReceiptsV2MaxField   = 4096
	distillApplicationReceiptsV2MaxFactIDs = 64
	distillApplicationReceiptsV2MaxLineNo  = 1 << 20
)

// distillApplicationIdentityV2 is the complete invalidation identity for one
// materialization.  ResultID identifies the successful parsed extraction;
// SourceDigest protects against a changed source at the same path/checkpoint;
// Branch, TranscriptPath, CheckpointID, and FactStoreGeneration identify the
// branch/provenance view to which the result was applied.
//
// CheckpointID is optional because a session may have no checkpoint.  Its
// empty value is still part of the identity and therefore does not collide
// with a non-empty checkpoint.
type distillApplicationIdentityV2 struct {
	CandidateID         string `json:"candidate_id"`
	ResultID            string `json:"result_id"`
	SourceSessionID     string `json:"source_session_id"`
	SourceDigest        string `json:"source_digest"`
	Branch              string `json:"branch"`
	TranscriptPath      string `json:"transcript_path"`
	CheckpointID        string `json:"checkpoint_id,omitempty"`
	SourceStartLine     int    `json:"source_start_line"`
	SourceEndLine       int    `json:"source_end_line"`
	FactStoreGeneration string `json:"fact_store_generation"`
}

// distillApplicationSlotV2 is the stable ownership slot for a candidate.
// Result and provenance changes replace this slot instead of accumulating
// obsolete receipts or anchors.
type distillApplicationSlotV2 struct {
	Branch          string `json:"branch"`
	SourceSessionID string `json:"source_session_id"`
	CandidateID     string `json:"candidate_id"`
}

// distillApplicationReceiptV2 contains only identifiers and digests needed by
// materialization and privacy lifecycle code.  AppliedFactIDs are fact-store
// identities, not fact text.  An empty list is a successful empty extraction;
// there is no representation for a failed or incomplete application.
type distillApplicationReceiptV2 struct {
	ReceiptID               string                       `json:"receipt_id"`
	SlotID                  string                       `json:"slot_id"`
	Identity                distillApplicationIdentityV2 `json:"identity"`
	AppliedFactIDs          []string                     `json:"applied_fact_ids,omitempty"`
	AppliedProvenanceDigest string                       `json:"applied_provenance_digest"`
}

type distillApplicationReceiptStoreV2 struct {
	entries map[string]distillApplicationReceiptV2
}

type distillApplicationReceiptLineV2 struct {
	Type    string                       `json:"type"`
	Version int                          `json:"version,omitempty"`
	Receipt *distillApplicationReceiptV2 `json:"receipt,omitempty"`
}

func newDistillApplicationReceiptStoreV2() distillApplicationReceiptStoreV2 {
	return distillApplicationReceiptStoreV2{entries: make(map[string]distillApplicationReceiptV2)}
}

// distillApplicationReceiptIDV2 is stable for an identity and intentionally
// excludes the output IDs.  Re-recording the same successful application is a
// replacement/idempotent operation; changing any source, branch, checkpoint,
// path, extraction result, or fact-store generation is a distinct receipt.
func distillApplicationReceiptIDV2(identity distillApplicationIdentityV2) (string, error) {
	if err := validateDistillApplicationIdentityV2(identity); err != nil {
		return "", err
	}
	canonical, err := json.Marshal(identity)
	if err != nil {
		return "", fmt.Errorf("marshal application identity: %w", err)
	}
	sum := sha256.Sum256(append([]byte("distill-application-v2\x00"), canonical...))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func distillApplicationSlotIDV2(identity distillApplicationIdentityV2) (string, error) {
	if err := validateDistillApplicationIdentityV2(identity); err != nil {
		return "", err
	}
	canonical, err := json.Marshal(distillApplicationSlotV2{
		Branch: identity.Branch, SourceSessionID: identity.SourceSessionID, CandidateID: identity.CandidateID,
	})
	if err != nil {
		return "", fmt.Errorf("marshal application slot: %w", err)
	}
	sum := sha256.Sum256(append([]byte("distill-application-slot-v2\x00"), canonical...))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// distillApplicationResultDigestV2 derives the content identity used in an
// application identity from a protocol-valid parsed result. Only the digest
// belongs in receipt state; the result itself remains in the extraction cache.
func distillApplicationResultDigestV2(result distillCandidateCacheResultV2) (string, error) {
	if err := validateDistillCandidateCacheResultV2(result); err != nil {
		return "", err
	}
	canonical, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("marshal application result: %w", err)
	}
	sum := sha256.Sum256(append([]byte("distill-application-result-v2\x00"), canonical...))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// distillApplicationProvenanceDigestV2 derives a content-free digest for the
// source anchor identity. It lets materialization and privacy code compare
// provenance without retaining an anchor or transcript excerpt in receipts.
func distillApplicationProvenanceDigestV2(identity distillApplicationIdentityV2) (string, error) {
	if err := validateDistillApplicationIdentityV2(identity); err != nil {
		return "", err
	}
	canonical, err := json.Marshal(struct {
		SourceSessionID string `json:"source_session_id"`
		Branch          string `json:"branch"`
		TranscriptPath  string `json:"transcript_path"`
		CheckpointID    string `json:"checkpoint_id,omitempty"`
		StartLine       int    `json:"start_line"`
		EndLine         int    `json:"end_line"`
	}{identity.SourceSessionID, identity.Branch, identity.TranscriptPath, identity.CheckpointID, identity.SourceStartLine, identity.SourceEndLine})
	if err != nil {
		return "", fmt.Errorf("marshal application provenance: %w", err)
	}
	sum := sha256.Sum256(append([]byte("distill-application-provenance-v2\x00"), canonical...))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// LookupSuccess reports whether this exact successful application is already
// materialized.  It returns a copy so callers cannot mutate store state.
func (store distillApplicationReceiptStoreV2) LookupSuccess(identity distillApplicationIdentityV2) (distillApplicationReceiptV2, bool) {
	slotID, err := distillApplicationSlotIDV2(identity)
	if err != nil {
		return distillApplicationReceiptV2{}, false
	}
	receipt, ok := store.entries[slotID]
	if !ok {
		return distillApplicationReceiptV2{}, false
	}
	receiptID, err := distillApplicationReceiptIDV2(identity)
	if err != nil || receipt.ReceiptID != receiptID || receipt.Identity != identity {
		return distillApplicationReceiptV2{}, false
	}
	return cloneDistillApplicationReceiptV2(receipt), true
}

// LookupSlot returns the current receipt even when its identity is stale. It
// lets reconciliation remove old fact anchors before applying a replacement.
func (store distillApplicationReceiptStoreV2) LookupSlot(slotID string) (distillApplicationReceiptV2, bool) {
	if !validSHA256Identity(slotID) {
		return distillApplicationReceiptV2{}, false
	}
	receipt, ok := store.entries[slotID]
	if !ok {
		return distillApplicationReceiptV2{}, false
	}
	return cloneDistillApplicationReceiptV2(receipt), true
}

// PutSuccess records one completed materialization.  It replaces an existing
// receipt with the same identity, which makes retries idempotent while still
// allowing a fresh result, branch, path, checkpoint, or generation to create
// a new identity.  Failed or incomplete applications have no API path here.
func (store *distillApplicationReceiptStoreV2) PutSuccess(receipt distillApplicationReceiptV2) error {
	_, _, err := store.PutSuccessReplacing(receipt)
	return err
}

// PutSuccessReplacing is PutSuccess with the previous slot value returned to
// the caller. The previous receipt is what a materializer uses to retract
// candidate-owned fact anchors before installing a changed result.
func (store *distillApplicationReceiptStoreV2) PutSuccessReplacing(receipt distillApplicationReceiptV2) (distillApplicationReceiptV2, bool, error) {
	if store == nil {
		return distillApplicationReceiptV2{}, false, fmt.Errorf("nil application receipt store")
	}
	if store.entries == nil {
		store.entries = make(map[string]distillApplicationReceiptV2)
	}
	if err := validateDistillApplicationReceiptV2(receipt); err != nil {
		return distillApplicationReceiptV2{}, false, err
	}
	if len(store.entries) >= distillApplicationReceiptsV2MaxEntries {
		if _, replacing := store.entries[receipt.SlotID]; !replacing {
			return distillApplicationReceiptV2{}, false, fmt.Errorf("application receipt store exceeds %d entries", distillApplicationReceiptsV2MaxEntries)
		}
	}
	previous, replaced := store.entries[receipt.SlotID]
	store.entries[receipt.SlotID] = cloneDistillApplicationReceiptV2(receipt)
	return cloneDistillApplicationReceiptV2(previous), replaced, nil
}

// RecordSuccess is a convenience seam for the materializer: callers provide
// the identity and the fact/provenance identifiers, and the receipt ID is
// derived rather than trusted from caller input.
func (store *distillApplicationReceiptStoreV2) RecordSuccess(identity distillApplicationIdentityV2, factIDs []string, provenanceDigest string) (distillApplicationReceiptV2, error) {
	receiptID, err := distillApplicationReceiptIDV2(identity)
	if err != nil {
		return distillApplicationReceiptV2{}, err
	}
	factIDs = append([]string(nil), factIDs...)
	sort.Strings(factIDs)
	receipt := distillApplicationReceiptV2{
		ReceiptID: receiptID, Identity: identity,
		AppliedFactIDs:          factIDs,
		AppliedProvenanceDigest: provenanceDigest,
	}
	receipt.SlotID, err = distillApplicationSlotIDV2(identity)
	if err != nil {
		return distillApplicationReceiptV2{}, err
	}
	if err := store.PutSuccess(receipt); err != nil {
		return distillApplicationReceiptV2{}, err
	}
	return cloneDistillApplicationReceiptV2(receipt), nil
}

func loadDistillApplicationReceiptStoreV2(brainDir string) (distillApplicationReceiptStoreV2, error) {
	empty := newDistillApplicationReceiptStoreV2()
	data, present, err := readMemoryStateFile(brainDir, distillApplicationReceiptsV2Path, "application receipts", distillApplicationReceiptsV2MaxBytes)
	if err != nil {
		return empty, err
	}
	if !present {
		return empty, nil
	}
	store, err := parseDistillApplicationReceiptStoreV2(data)
	if err != nil {
		return empty, fmt.Errorf("parse application receipts: %w", err)
	}
	return store, nil
}

func saveDistillApplicationReceiptStoreV2(brainDir string, store distillApplicationReceiptStoreV2) error {
	data, err := marshalDistillApplicationReceiptStoreV2(store)
	if err != nil {
		return err
	}
	return writeBrainRelativeFileAtomic(brainDir, distillApplicationReceiptsV2Path, data, 0o600)
}

func marshalDistillApplicationReceiptStoreV2(store distillApplicationReceiptStoreV2) ([]byte, error) {
	if len(store.entries) > distillApplicationReceiptsV2MaxEntries {
		return nil, fmt.Errorf("application receipt store exceeds %d entries", distillApplicationReceiptsV2MaxEntries)
	}
	ids := make([]string, 0, len(store.entries))
	for id := range store.entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out bytes.Buffer
	write := func(line distillApplicationReceiptLineV2) error {
		data, err := json.Marshal(line)
		if err != nil {
			return err
		}
		if len(data) > distillApplicationReceiptsV2MaxLine {
			return fmt.Errorf("application receipt record exceeds %d bytes", distillApplicationReceiptsV2MaxLine)
		}
		out.Write(data)
		out.WriteByte('\n')
		return nil
	}
	if err := write(distillApplicationReceiptLineV2{Type: "header", Version: distillApplicationReceiptV2Version}); err != nil {
		return nil, err
	}
	previous := ""
	for _, id := range ids {
		if id <= previous {
			return nil, fmt.Errorf("application receipt slot IDs are not strictly ordered")
		}
		receipt := store.entries[id]
		if receipt.SlotID != id {
			return nil, fmt.Errorf("application receipt key does not match slot ID")
		}
		if err := validateDistillApplicationReceiptV2(receipt); err != nil {
			return nil, err
		}
		if err := write(distillApplicationReceiptLineV2{Type: "receipt", Receipt: &receipt}); err != nil {
			return nil, err
		}
		previous = id
	}
	if out.Len() > distillApplicationReceiptsV2MaxBytes {
		return nil, fmt.Errorf("application receipt store exceeds %d bytes", distillApplicationReceiptsV2MaxBytes)
	}
	return out.Bytes(), nil
}

func parseDistillApplicationReceiptStoreV2(data []byte) (distillApplicationReceiptStoreV2, error) {
	store := newDistillApplicationReceiptStoreV2()
	if len(data) == 0 || len(data) > distillApplicationReceiptsV2MaxBytes {
		return store, fmt.Errorf("application receipt store size is invalid")
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 1024), distillApplicationReceiptsV2MaxLine+1)
	headerSeen := false
	previousID := ""
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Bytes()
		if len(line) == 0 {
			return store, fmt.Errorf("line %d is blank", lineNo)
		}
		if len(line) > distillApplicationReceiptsV2MaxLine {
			return store, fmt.Errorf("line %d exceeds %d bytes", lineNo, distillApplicationReceiptsV2MaxLine)
		}
		var value distillApplicationReceiptLineV2
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		err := decoder.Decode(&value)
		if err == nil {
			var trailing any
			if trailingErr := decoder.Decode(&trailing); trailingErr != io.EOF {
				if trailingErr == nil {
					err = fmt.Errorf("trailing JSON value")
				} else {
					err = trailingErr
				}
			}
		}
		if err != nil {
			return store, fmt.Errorf("line %d: %w", lineNo, err)
		}
		canonical, err := json.Marshal(value)
		if err != nil || !bytes.Equal(line, canonical) {
			return store, fmt.Errorf("line %d is not canonical application receipt JSON", lineNo)
		}
		switch value.Type {
		case "header":
			if headerSeen || lineNo != 1 || value.Version != distillApplicationReceiptV2Version || value.Receipt != nil {
				return store, fmt.Errorf("line %d has an invalid application receipt header", lineNo)
			}
			headerSeen = true
		case "receipt":
			if !headerSeen || value.Version != 0 || value.Receipt == nil {
				return store, fmt.Errorf("line %d has an invalid application receipt record", lineNo)
			}
			receipt := *value.Receipt
			if err := validateDistillApplicationReceiptV2(receipt); err != nil {
				return store, fmt.Errorf("line %d: %w", lineNo, err)
			}
			if receipt.SlotID <= previousID {
				return store, fmt.Errorf("line %d application receipts are not in canonical order", lineNo)
			}
			if _, duplicate := store.entries[receipt.SlotID]; duplicate {
				return store, fmt.Errorf("line %d duplicates application slot %q", lineNo, receipt.SlotID)
			}
			if len(store.entries) >= distillApplicationReceiptsV2MaxEntries {
				return store, fmt.Errorf("application receipt store exceeds %d entries", distillApplicationReceiptsV2MaxEntries)
			}
			store.entries[receipt.SlotID] = cloneDistillApplicationReceiptV2(receipt)
			previousID = receipt.SlotID
		default:
			return store, fmt.Errorf("line %d has unknown record type %q", lineNo, value.Type)
		}
	}
	if err := scanner.Err(); err != nil {
		return store, fmt.Errorf("scan application receipts: %w", err)
	}
	if !headerSeen {
		return store, fmt.Errorf("application receipt header is missing")
	}
	return store, nil
}

func validateDistillApplicationReceiptV2(receipt distillApplicationReceiptV2) error {
	if err := validateDistillApplicationIdentityV2(receipt.Identity); err != nil {
		return err
	}
	expectedID, err := distillApplicationReceiptIDV2(receipt.Identity)
	if err != nil || receipt.ReceiptID != expectedID {
		return fmt.Errorf("application receipt ID does not match identity")
	}
	expectedSlotID, err := distillApplicationSlotIDV2(receipt.Identity)
	if err != nil || receipt.SlotID != expectedSlotID {
		return fmt.Errorf("application receipt slot ID does not match identity")
	}
	if len(receipt.AppliedFactIDs) > distillApplicationReceiptsV2MaxFactIDs {
		return fmt.Errorf("application receipt has more than %d fact IDs", distillApplicationReceiptsV2MaxFactIDs)
	}
	seen := make(map[string]struct{}, len(receipt.AppliedFactIDs))
	previousFactID := ""
	for _, id := range receipt.AppliedFactIDs {
		if err := validateDistillApplicationShortFieldV2("applied fact ID", id); err != nil {
			return err
		}
		if _, duplicate := seen[id]; duplicate {
			return fmt.Errorf("application receipt has duplicate fact ID %q", id)
		}
		if previousFactID != "" && id <= previousFactID {
			return fmt.Errorf("application receipt fact IDs are not in canonical order")
		}
		seen[id] = struct{}{}
		previousFactID = id
	}
	if !validSHA256Identity(receipt.AppliedProvenanceDigest) {
		return fmt.Errorf("applied provenance digest is invalid")
	}
	return nil
}

func validateDistillApplicationIdentityV2(identity distillApplicationIdentityV2) error {
	for name, value := range map[string]string{
		"candidate ID":          identity.CandidateID,
		"source session ID":     identity.SourceSessionID,
		"branch":                identity.Branch,
		"transcript path":       identity.TranscriptPath,
		"fact-store generation": identity.FactStoreGeneration,
	} {
		if err := validateDistillApplicationShortFieldV2(name, value); err != nil {
			return err
		}
	}
	for name, digest := range map[string]string{
		"result digest":         identity.ResultID,
		"source digest":         identity.SourceDigest,
		"fact-store generation": identity.FactStoreGeneration,
	} {
		if !validSHA256Identity(digest) {
			return fmt.Errorf("%s is invalid", name)
		}
	}
	if identity.TranscriptPath != filepath.ToSlash(filepath.Clean(filepath.FromSlash(identity.TranscriptPath))) {
		return fmt.Errorf("transcript path is not canonical")
	}
	if _, err := cleanBrainRelativePath(identity.TranscriptPath); err != nil {
		return fmt.Errorf("transcript path is invalid: %w", err)
	}
	if identity.SourceStartLine < 1 || identity.SourceEndLine < identity.SourceStartLine || identity.SourceEndLine > distillApplicationReceiptsV2MaxLineNo {
		return fmt.Errorf("source line range is invalid")
	}
	if identity.CheckpointID != "" {
		if err := validateDistillApplicationShortFieldV2("checkpoint ID", identity.CheckpointID); err != nil {
			return err
		}
	}
	return nil
}

func validateDistillApplicationShortFieldV2(name, value string) error {
	if value == "" || value != strings.TrimSpace(value) || len(value) > distillApplicationReceiptsV2MaxField || strings.ContainsRune(value, '\x00') || strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("%s is invalid", name)
	}
	return nil
}

// PruneUnseenSlots removes successful applications owned by branch/session
// whose candidate IDs are absent from the current deterministic set. An empty
// set removes every slot for that owner, including a prior non-empty result.
func (store *distillApplicationReceiptStoreV2) PruneUnseenSlots(branch, sourceSessionID string, currentCandidateIDs map[string]struct{}) (int, error) {
	removed, err := store.PruneUnseenSlotsWithReceipts(branch, sourceSessionID, currentCandidateIDs)
	return len(removed), err
}

// PruneUnseenSlotsWithReceipts is the anchor-retraction form of
// PruneUnseenSlots. It returns removed receipts before deleting their slots.
func (store *distillApplicationReceiptStoreV2) PruneUnseenSlotsWithReceipts(branch, sourceSessionID string, currentCandidateIDs map[string]struct{}) ([]distillApplicationReceiptV2, error) {
	if store == nil {
		return nil, fmt.Errorf("nil application receipt store")
	}
	if err := validateDistillApplicationShortFieldV2("branch", branch); err != nil {
		return nil, err
	}
	if err := validateDistillApplicationShortFieldV2("source session ID", sourceSessionID); err != nil {
		return nil, err
	}
	removed := make([]distillApplicationReceiptV2, 0)
	for slotID, receipt := range store.entries {
		identity := receipt.Identity
		if identity.Branch != branch || identity.SourceSessionID != sourceSessionID {
			continue
		}
		if _, keep := currentCandidateIDs[identity.CandidateID]; keep {
			continue
		}
		removed = append(removed, cloneDistillApplicationReceiptV2(receipt))
		delete(store.entries, slotID)
	}
	return removed, nil
}

// RemoveSlot deletes one receipt and returns it for candidate-owned anchor
// cleanup. Invalid slot IDs are treated as a failed lookup.
func (store *distillApplicationReceiptStoreV2) RemoveSlot(slotID string) (distillApplicationReceiptV2, bool) {
	if store == nil || !validSHA256Identity(slotID) {
		return distillApplicationReceiptV2{}, false
	}
	receipt, ok := store.entries[slotID]
	if !ok {
		return distillApplicationReceiptV2{}, false
	}
	delete(store.entries, slotID)
	return cloneDistillApplicationReceiptV2(receipt), true
}

func cloneDistillApplicationReceiptV2(receipt distillApplicationReceiptV2) distillApplicationReceiptV2 {
	receipt.AppliedFactIDs = append([]string(nil), receipt.AppliedFactIDs...)
	return receipt
}
