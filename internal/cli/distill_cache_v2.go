package cli

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// distillCandidateResultCacheV2 is deliberately separate from the legacy
// per-session fingerprint cache. It caches only a protocol-valid, attributed
// candidate result; it never stores a provider response or candidate/card text.
const (
	distillCandidateResultCacheV2Version = 2
	distillCandidateResultCacheV2Path    = factsDirName + "/distill-candidate-results-v2.ndjson"

	distillCandidateResultCacheV2MaxBytes   = 8 << 20
	distillCandidateResultCacheV2MaxEntries = 4096
	distillCandidateResultCacheV2MaxLine    = 32 << 10
	distillCandidateResultCacheV2MaxField   = 4096
)

type distillCandidateCacheIdentityV2 struct {
	// CardDigest is SHA-256 over the redacted, rendered card. It intentionally
	// does not retain the card itself.
	CardDigest         string `json:"card_digest"`
	PromptVersion      string `json:"prompt_version"`
	PromptDigest       string `json:"prompt_digest"`
	TaxonomyDigest     string `json:"taxonomy_digest"`
	AgentCommandDigest string `json:"agent_command_digest"`
	Model              string `json:"model"`
	Effort             string `json:"effort"`
	RedactionVersion   string `json:"redaction_version"`
}

// distillCandidateCachedFactV2 is the compact canonical output of the fact
// parser. It deliberately omits IDs, anchors, branches, timestamps, confidence,
// and model/provider text: those are materialization or packing concerns.
type distillCandidateCachedFactV2 struct {
	Kind  string   `json:"kind"`
	Paths []string `json:"paths"`
	Text  string   `json:"text"`
}

// distillCandidateCacheResultV2 makes a successful empty extraction explicit.
// Empty results have Empty=true and no facts; non-empty results have Empty=false
// and at least one canonical fact. Errors and incomplete output are never cacheable.
type distillCandidateCacheResultV2 struct {
	Empty bool                           `json:"empty,omitempty"`
	Facts []distillCandidateCachedFactV2 `json:"facts,omitempty"`
}

type distillCandidateCacheEntryV2 struct {
	CandidateID         string                          `json:"candidate_id"`
	SourceSessionID     string                          `json:"source_session_id"`
	SourceSessionDigest string                          `json:"source_session_digest"`
	Identity            distillCandidateCacheIdentityV2 `json:"identity"`
	Result              distillCandidateCacheResultV2   `json:"result"`
}

type distillCandidateResultCacheV2 struct {
	entries map[string]distillCandidateCacheEntryV2
}

type distillCandidateCacheLineV2 struct {
	Type    string                        `json:"type"`
	Version int                           `json:"version,omitempty"`
	Entry   *distillCandidateCacheEntryV2 `json:"entry,omitempty"`
}

func newDistillCandidateResultCacheV2() distillCandidateResultCacheV2 {
	return distillCandidateResultCacheV2{entries: make(map[string]distillCandidateCacheEntryV2)}
}

// distillCandidateSessionOwnershipDigestV2 lets a future privacy inventory
// join a tombstone by digest without needing candidate content. The session ID
// remains in the entry because current tombstone purge is keyed by session ID.
func distillCandidateSessionOwnershipDigestV2(sessionID string) string {
	sum := sha256.Sum256([]byte("distill-candidate-owner-v2\x00" + strings.TrimSpace(sessionID)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func distillCandidateCacheDigestV2(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (cache distillCandidateResultCacheV2) LookupSuccess(candidateID string, identity distillCandidateCacheIdentityV2) (distillCandidateCacheResultV2, bool) {
	if err := validateDistillCandidateCacheIdentityV2(identity); err != nil {
		return distillCandidateCacheResultV2{}, false
	}
	entry, ok := cache.entries[strings.TrimSpace(candidateID)]
	if !ok || entry.Identity != identity {
		return distillCandidateCacheResultV2{}, false
	}
	return cloneDistillCandidateCacheResultV2(entry.Result), true
}

// PutSuccess records only a protocol-valid extraction. It keeps the cache
// per-candidate even when callers later combine cards into a different pack.
func (cache *distillCandidateResultCacheV2) PutSuccess(candidateID, sourceSessionID string, identity distillCandidateCacheIdentityV2, result distillCandidateCacheResultV2) error {
	if cache == nil {
		return fmt.Errorf("nil candidate result cache")
	}
	if cache.entries == nil {
		cache.entries = make(map[string]distillCandidateCacheEntryV2)
	}
	entry := distillCandidateCacheEntryV2{
		CandidateID:         strings.TrimSpace(candidateID),
		SourceSessionID:     strings.TrimSpace(sourceSessionID),
		SourceSessionDigest: distillCandidateSessionOwnershipDigestV2(sourceSessionID),
		Identity:            identity,
		Result:              cloneDistillCandidateCacheResultV2(result),
	}
	if err := validateDistillCandidateCacheEntryV2(entry); err != nil {
		return err
	}
	if _, exists := cache.entries[entry.CandidateID]; !exists && len(cache.entries) >= distillCandidateResultCacheV2MaxEntries {
		return fmt.Errorf("candidate result cache exceeds %d entries", distillCandidateResultCacheV2MaxEntries)
	}
	cache.entries[entry.CandidateID] = entry
	return nil
}

func loadDistillCandidateResultCacheV2(brainDir string) (distillCandidateResultCacheV2, error) {
	empty := newDistillCandidateResultCacheV2()
	data, present, err := readMemoryStateFile(brainDir, distillCandidateResultCacheV2Path, "candidate result cache", distillCandidateResultCacheV2MaxBytes)
	if err != nil {
		return empty, err
	}
	if !present {
		return empty, nil
	}
	cache, err := parseDistillCandidateResultCacheV2(data)
	if err != nil {
		return empty, fmt.Errorf("parse candidate result cache: %w", err)
	}
	return cache, nil
}

func saveDistillCandidateResultCacheV2(brainDir string, cache distillCandidateResultCacheV2) error {
	data, err := marshalDistillCandidateResultCacheV2(cache)
	if err != nil {
		return err
	}
	return writeBrainRelativeFileAtomic(brainDir, distillCandidateResultCacheV2Path, data, 0o600)
}

func marshalDistillCandidateResultCacheV2(cache distillCandidateResultCacheV2) ([]byte, error) {
	if len(cache.entries) > distillCandidateResultCacheV2MaxEntries {
		return nil, fmt.Errorf("candidate result cache exceeds %d entries", distillCandidateResultCacheV2MaxEntries)
	}
	var out bytes.Buffer
	write := func(line distillCandidateCacheLineV2) error {
		data, err := json.Marshal(line)
		if err != nil {
			return err
		}
		if len(data) > distillCandidateResultCacheV2MaxLine {
			return fmt.Errorf("candidate result cache record exceeds %d bytes", distillCandidateResultCacheV2MaxLine)
		}
		out.Write(data)
		out.WriteByte('\n')
		return nil
	}
	if err := write(distillCandidateCacheLineV2{Type: "header", Version: distillCandidateResultCacheV2Version}); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(cache.entries))
	for candidateID := range cache.entries {
		ids = append(ids, candidateID)
	}
	sort.Strings(ids)
	for _, candidateID := range ids {
		entry := cache.entries[candidateID]
		if entry.CandidateID != candidateID {
			return nil, fmt.Errorf("candidate result cache key does not match entry ID")
		}
		if err := validateDistillCandidateCacheEntryV2(entry); err != nil {
			return nil, err
		}
		if err := write(distillCandidateCacheLineV2{Type: "result", Entry: &entry}); err != nil {
			return nil, err
		}
	}
	if out.Len() > distillCandidateResultCacheV2MaxBytes {
		return nil, fmt.Errorf("candidate result cache exceeds %d bytes", distillCandidateResultCacheV2MaxBytes)
	}
	return out.Bytes(), nil
}

func parseDistillCandidateResultCacheV2(data []byte) (distillCandidateResultCacheV2, error) {
	cache := newDistillCandidateResultCacheV2()
	if len(data) == 0 || len(data) > distillCandidateResultCacheV2MaxBytes {
		return cache, fmt.Errorf("cache size is invalid")
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 1024), distillCandidateResultCacheV2MaxLine+1)
	lineNo := 0
	headerSeen := false
	for scanner.Scan() {
		lineNo++
		line := scanner.Bytes()
		if len(line) == 0 {
			return cache, fmt.Errorf("line %d is blank", lineNo)
		}
		if len(line) > distillCandidateResultCacheV2MaxLine {
			return cache, fmt.Errorf("line %d exceeds %d bytes", lineNo, distillCandidateResultCacheV2MaxLine)
		}
		var lineValue distillCandidateCacheLineV2
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		err := decoder.Decode(&lineValue)
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
			return cache, fmt.Errorf("line %d: %w", lineNo, err)
		}
		switch lineValue.Type {
		case "header":
			if headerSeen || lineNo != 1 || lineValue.Version != distillCandidateResultCacheV2Version || lineValue.Entry != nil {
				return cache, fmt.Errorf("line %d has an invalid cache header", lineNo)
			}
			headerSeen = true
		case "result":
			if !headerSeen || lineValue.Version != 0 || lineValue.Entry == nil {
				return cache, fmt.Errorf("line %d has an invalid result record", lineNo)
			}
			entry := *lineValue.Entry
			if err := validateDistillCandidateCacheEntryV2(entry); err != nil {
				return cache, fmt.Errorf("line %d: %w", lineNo, err)
			}
			if _, duplicate := cache.entries[entry.CandidateID]; duplicate {
				return cache, fmt.Errorf("line %d duplicates candidate %q", lineNo, entry.CandidateID)
			}
			if len(cache.entries) >= distillCandidateResultCacheV2MaxEntries {
				return cache, fmt.Errorf("cache exceeds %d entries", distillCandidateResultCacheV2MaxEntries)
			}
			cache.entries[entry.CandidateID] = entry
		default:
			return cache, fmt.Errorf("line %d has unknown record type %q", lineNo, lineValue.Type)
		}
	}
	if err := scanner.Err(); err != nil {
		return cache, fmt.Errorf("scan cache: %w", err)
	}
	if !headerSeen {
		return cache, fmt.Errorf("cache header is missing")
	}
	return cache, nil
}

func validateDistillCandidateCacheEntryV2(entry distillCandidateCacheEntryV2) error {
	if err := validateDistillCandidateCacheShortTextV2("candidate ID", entry.CandidateID); err != nil {
		return err
	}
	if err := validateDistillCandidateCacheShortTextV2("source session ID", entry.SourceSessionID); err != nil {
		return err
	}
	if !validSHA256Identity(entry.SourceSessionDigest) || entry.SourceSessionDigest != distillCandidateSessionOwnershipDigestV2(entry.SourceSessionID) {
		return fmt.Errorf("source session digest is invalid")
	}
	if err := validateDistillCandidateCacheIdentityV2(entry.Identity); err != nil {
		return err
	}
	return validateDistillCandidateCacheResultV2(entry.Result)
}

func validateDistillCandidateCacheIdentityV2(identity distillCandidateCacheIdentityV2) error {
	for name, digest := range map[string]string{
		"card digest": identity.CardDigest, "prompt digest": identity.PromptDigest,
		"taxonomy digest": identity.TaxonomyDigest, "agent command digest": identity.AgentCommandDigest,
	} {
		if !validSHA256Identity(digest) {
			return fmt.Errorf("%s is invalid", name)
		}
	}
	for name, value := range map[string]string{
		"prompt version": identity.PromptVersion, "model": identity.Model,
		"effort": identity.Effort, "redaction version": identity.RedactionVersion,
	} {
		if err := validateDistillCandidateCacheShortTextV2(name, value); err != nil {
			return err
		}
	}
	return nil
}

func validateDistillCandidateCacheResultV2(result distillCandidateCacheResultV2) error {
	if result.Empty {
		if len(result.Facts) != 0 {
			return fmt.Errorf("empty result has facts")
		}
		return nil
	}
	if len(result.Facts) == 0 || len(result.Facts) > factsMaxPerChunk {
		return fmt.Errorf("result must contain 1-%d facts or be explicitly empty", factsMaxPerChunk)
	}
	for _, fact := range result.Facts {
		if !validFactKind(fact.Kind) {
			return fmt.Errorf("fact kind is invalid")
		}
		if len(fact.Text) == 0 || len(fact.Text) > distillFactMaxTextSize || fact.Text != redactText(fact.Text) {
			return fmt.Errorf("fact text is invalid or unredacted")
		}
		paths := normalizeFactPaths(fact.Paths)
		if len(paths) == 0 || !sameStrings(paths, fact.Paths) {
			return fmt.Errorf("fact paths are not canonical")
		}
	}
	return nil
}

func validateDistillCandidateCacheShortTextV2(name, value string) error {
	if value == "" || value != strings.TrimSpace(value) || len(value) > distillCandidateResultCacheV2MaxField || strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("%s is invalid", name)
	}
	return nil
}

func cloneDistillCandidateCacheResultV2(result distillCandidateCacheResultV2) distillCandidateCacheResultV2 {
	result.Facts = append([]distillCandidateCachedFactV2(nil), result.Facts...)
	for i := range result.Facts {
		result.Facts[i].Paths = append([]string(nil), result.Facts[i].Paths...)
	}
	return result
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
