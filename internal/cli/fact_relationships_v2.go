package cli

// Phase 3B relationship observations are local-only, compact advisory state.
// This store intentionally lives beside v2 receipts rather than facts/<branch>
// so normal fact publish/sync/review flows cannot mistake it for executable
// reconciliation input.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/ashtom/entire-brain/internal/factmerge"
)

const (
	distillRelationshipStoreV2Version    = 1
	distillRelationshipStoreV2Path       = factsDirName + "/distill-v2/relationships.ndjson"
	distillRelationshipStoreV2MaxBytes   = 4 << 20
	distillRelationshipStoreV2MaxEntries = 4096
	distillRelationshipStoreV2MaxLine    = 32 << 10
	distillRelationshipStoreV2MaxField   = 4096
)

type distillRelationshipStoreV2 struct {
	entries map[string]factmerge.RelationshipProposal
}

type distillRelationshipStoreLineV2 struct {
	Type     string                          `json:"type"`
	Version  int                             `json:"version,omitempty"`
	Relation *factmerge.RelationshipProposal `json:"relationship,omitempty"`
}

func newDistillRelationshipStoreV2() distillRelationshipStoreV2 {
	return distillRelationshipStoreV2{entries: make(map[string]factmerge.RelationshipProposal)}
}

func (store *distillRelationshipStoreV2) Put(proposal factmerge.RelationshipProposal) error {
	if store == nil {
		return fmt.Errorf("nil relationship store")
	}
	if err := validateDistillRelationshipProposalV2(proposal); err != nil {
		return err
	}
	if store.entries == nil {
		store.entries = make(map[string]factmerge.RelationshipProposal)
	}
	if len(store.entries) >= distillRelationshipStoreV2MaxEntries {
		if _, replacing := store.entries[proposal.ID]; !replacing {
			return fmt.Errorf("relationship store exceeds %d entries", distillRelationshipStoreV2MaxEntries)
		}
	}
	if existing, ok := store.entries[proposal.ID]; ok {
		proposal.Owners = factmerge.UnionRelationshipOwners(existing.Owners, proposal.Owners)
		if err := validateDistillRelationshipProposalV2(proposal); err != nil {
			return err
		}
	}
	store.entries[proposal.ID] = cloneDistillRelationshipProposalV2(proposal)
	return nil
}

// RemoveOwner removes one candidate ownership slot across all relationships;
// observations with no remaining evidence disappear. This is the lifecycle
// operation callers use before replacing, force-replaying, or pruning a result.
func (store *distillRelationshipStoreV2) RemoveOwner(owner factmerge.RelationshipOwner) (int, error) {
	if store == nil {
		return 0, fmt.Errorf("nil relationship store")
	}
	if err := validateDistillRelationshipOwnerV2(owner); err != nil {
		return 0, err
	}
	removed := 0
	for id, proposal := range store.entries {
		owners, changed := factmerge.RemoveRelationshipOwner(proposal.Owners, owner)
		if !changed {
			continue
		}
		removed++
		if len(owners) == 0 {
			delete(store.entries, id)
			continue
		}
		proposal.Owners = owners
		store.entries[id] = proposal
	}
	return removed, nil
}

func loadDistillRelationshipStoreV2(brainDir string) (distillRelationshipStoreV2, error) {
	empty := newDistillRelationshipStoreV2()
	data, present, err := readMemoryStateFile(brainDir, distillRelationshipStoreV2Path, "relationships", distillRelationshipStoreV2MaxBytes)
	if err != nil {
		return empty, err
	}
	if !present {
		return empty, nil
	}
	store, err := parseDistillRelationshipStoreV2(data)
	if err != nil {
		return empty, fmt.Errorf("parse relationships: %w", err)
	}
	return store, nil
}

func saveDistillRelationshipStoreV2(brainDir string, store distillRelationshipStoreV2) error {
	data, err := marshalDistillRelationshipStoreV2(store)
	if err != nil {
		return err
	}
	return writeBrainRelativeFileAtomic(brainDir, distillRelationshipStoreV2Path, data, 0o600)
}

func marshalDistillRelationshipStoreV2(store distillRelationshipStoreV2) ([]byte, error) {
	if len(store.entries) > distillRelationshipStoreV2MaxEntries {
		return nil, fmt.Errorf("relationship store exceeds %d entries", distillRelationshipStoreV2MaxEntries)
	}
	ids := make([]string, 0, len(store.entries))
	for id := range store.entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out bytes.Buffer
	write := func(line distillRelationshipStoreLineV2) error {
		data, err := json.Marshal(line)
		if err != nil {
			return err
		}
		if len(data) > distillRelationshipStoreV2MaxLine {
			return fmt.Errorf("relationship record exceeds %d bytes", distillRelationshipStoreV2MaxLine)
		}
		out.Write(data)
		out.WriteByte('\n')
		return nil
	}
	if err := write(distillRelationshipStoreLineV2{Type: "header", Version: distillRelationshipStoreV2Version}); err != nil {
		return nil, err
	}
	previous := ""
	for _, id := range ids {
		if id <= previous {
			return nil, fmt.Errorf("relationship IDs are not strictly ordered")
		}
		proposal := store.entries[id]
		if proposal.ID != id {
			return nil, fmt.Errorf("relationship key does not match relationship ID")
		}
		if err := validateDistillRelationshipProposalV2(proposal); err != nil {
			return nil, err
		}
		if err := write(distillRelationshipStoreLineV2{Type: "relationship", Relation: &proposal}); err != nil {
			return nil, err
		}
		previous = id
	}
	if out.Len() > distillRelationshipStoreV2MaxBytes {
		return nil, fmt.Errorf("relationship store exceeds %d bytes", distillRelationshipStoreV2MaxBytes)
	}
	return out.Bytes(), nil
}

func parseDistillRelationshipStoreV2(data []byte) (distillRelationshipStoreV2, error) {
	store := newDistillRelationshipStoreV2()
	if len(data) == 0 || len(data) > distillRelationshipStoreV2MaxBytes {
		return store, fmt.Errorf("relationship store size is invalid")
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 1024), distillRelationshipStoreV2MaxLine+1)
	headerSeen, lineNo, previousID := false, 0, ""
	for scanner.Scan() {
		lineNo++
		line := scanner.Bytes()
		if len(line) == 0 || len(line) > distillRelationshipStoreV2MaxLine {
			return store, fmt.Errorf("line %d has invalid size", lineNo)
		}
		var value distillRelationshipStoreLineV2
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
			return store, fmt.Errorf("line %d is not canonical relationship JSON", lineNo)
		}
		switch value.Type {
		case "header":
			if headerSeen || lineNo != 1 || value.Version != distillRelationshipStoreV2Version || value.Relation != nil {
				return store, fmt.Errorf("line %d has invalid relationship header", lineNo)
			}
			headerSeen = true
		case "relationship":
			if !headerSeen || value.Version != 0 || value.Relation == nil {
				return store, fmt.Errorf("line %d has invalid relationship record", lineNo)
			}
			proposal := *value.Relation
			if err := validateDistillRelationshipProposalV2(proposal); err != nil {
				return store, fmt.Errorf("line %d: %w", lineNo, err)
			}
			if proposal.ID <= previousID || len(store.entries) >= distillRelationshipStoreV2MaxEntries {
				return store, fmt.Errorf("line %d relationship records are not in canonical order or exceed limit", lineNo)
			}
			store.entries[proposal.ID] = cloneDistillRelationshipProposalV2(proposal)
			previousID = proposal.ID
		default:
			return store, fmt.Errorf("line %d has unknown record type %q", lineNo, value.Type)
		}
	}
	if err := scanner.Err(); err != nil {
		return store, fmt.Errorf("scan relationships: %w", err)
	}
	if !headerSeen {
		return store, fmt.Errorf("relationship header is missing")
	}
	return store, nil
}

func validateDistillRelationshipProposalV2(proposal factmerge.RelationshipProposal) error {
	if err := factmerge.ValidateRelationshipProposal(proposal); err != nil {
		return err
	}
	for _, value := range append(append([]string{proposal.ID, proposal.Branch, proposal.Kind, proposal.Subject.Kind, proposal.Subject.TopLevel, proposal.Subject.StrongLocus}, proposal.FactIDs...), relationshipOwnerFieldsV2(proposal.Owners)...) {
		if err := validateDistillRelationshipFieldV2(value); err != nil {
			return err
		}
	}
	return nil
}

func relationshipOwnerFieldsV2(owners []factmerge.RelationshipOwner) []string {
	fields := make([]string, 0, len(owners)*2)
	for _, owner := range owners {
		fields = append(fields, owner.CandidateID, owner.SourceSessionID)
	}
	return fields
}

func validateDistillRelationshipOwnerV2(owner factmerge.RelationshipOwner) error {
	if err := validateDistillRelationshipFieldV2(owner.CandidateID); err != nil {
		return err
	}
	return validateDistillRelationshipFieldV2(owner.SourceSessionID)
}

func validateDistillRelationshipFieldV2(value string) error {
	if len(value) == 0 || len(value) > distillRelationshipStoreV2MaxField || value != strings.TrimSpace(value) || strings.ContainsAny(value, "\x00\r\n") {
		return fmt.Errorf("relationship field is invalid")
	}
	return nil
}

func cloneDistillRelationshipProposalV2(proposal factmerge.RelationshipProposal) factmerge.RelationshipProposal {
	proposal.FactIDs = append([]string(nil), proposal.FactIDs...)
	proposal.Owners = append([]factmerge.RelationshipOwner(nil), proposal.Owners...)
	return proposal
}
