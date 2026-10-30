package cli

// Legacy artifact helpers are compiled only for migration and compatibility
// fixtures. Production Brain never builds or copies a code graph.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
)

// semanticIndexStageDeadline stops an index at a stage boundary when its caller's
// deadline has already passed.
//
// A deadline that is only consulted after the work finishes is not a deadline.
// brain_index_repository advertises a 60 second cap and returned at ~113 seconds,
// because nothing between "verify the provider" and "write the manifest" ever
// asked whether it was still wanted -- the cap chose the error, the work chose
// the wall clock. Checking at each boundary, plus inside the store build's own
// row loop (buildSemanticGeneration), means an expired index stops at the next
// boundary instead of running to completion and then being thrown away.
//
// The stage name is in the error so a caller reading a debug log can see WHERE
// the time went, not just that time ran out.
func semanticIndexStageDeadline(ctx context.Context, stage string) error {
	if ctx == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("semantic index stopped after %s: %w", stage, err)
	}
	return nil
}

func semanticSnapshotFileCount(raw []byte) int {
	files := map[string]struct{}{}
	scanner := newSemanticScanner(bytes.NewReader(raw))
	if !scanner.Scan() {
		return 0
	}
	for scanner.Scan() {
		text := bytes.TrimSpace(scanner.Bytes())
		if len(text) == 0 {
			continue
		}
		var record semanticRecord
		if err := json.Unmarshal(text, &record); err != nil {
			continue
		}
		switch record.RecordType {
		case "file", "symbol":
			if path := record.SemanticPath(); path != "" {
				files[path] = struct{}{}
			}
		}
	}
	return len(files)
}

// validateLiveSemanticHeader checks the header the brain is about to persist:
// that it names a commit and a tree, and that both agree with the live
// repository.
//
// It deliberately does NOT check repo_key. scanSemanticStream has already
// rewritten header.RepoKey to the brain's canonical storage key, so comparing it
// against that same storage key here compared a value with itself and could not
// fail for any input -- protection in appearance only. The two places where a
// repo_key is genuinely untrusted each have a real check:
//
//	validateSemanticProviderRepoKey   the spelling the provider stamped
//	readSemanticSnapshotSummary /     a key read back from an on-disk artifact
//	validateImportedBundle            (another brain's bundle, an older snapshot)
func validateLiveSemanticHeader(header semanticHeader, commit, tree, repoDir string, allowWorktreeTree bool) error {
	if header.Commit == "" {
		return fmt.Errorf("semantic snapshot header missing commit%s", semanticProviderRefusalContext(header, repoDir))
	}
	if header.Tree == "" {
		return fmt.Errorf("semantic snapshot header missing tree%s", semanticProviderRefusalContext(header, repoDir))
	}
	if header.RepoKey == "" {
		return fmt.Errorf("semantic snapshot header missing repo_key%s", semanticProviderRefusalContext(header, repoDir))
	}
	if header.Commit != "" && commit != "" && header.Commit != commit {
		return fmt.Errorf("semantic snapshot commit %q does not match HEAD %q", header.Commit, commit)
	}
	if header.Tree != "" && tree != "" && header.Tree != tree && !allowWorktreeTree {
		return fmt.Errorf("semantic snapshot tree %q does not match HEAD tree %q", header.Tree, tree)
	}
	return nil
}

func filterSemanticSnapshot(raw []byte, ignore brainIgnore, repoDir string) (semanticHeader, semanticCounts, []byte, error) {
	scanner := newSemanticScanner(bytes.NewReader(raw))
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return semanticHeader{}, semanticCounts{}, nil, err
		}
		return semanticHeader{}, semanticCounts{}, nil, errors.New("semantic snapshot missing header")
	}
	var header semanticHeader
	if err := json.Unmarshal(scanner.Bytes(), &header); err != nil {
		return semanticHeader{}, semanticCounts{}, nil, fmt.Errorf("parse semantic snapshot header: %w", err)
	}
	header.RepoRoot = ""
	header.Warnings, header.PartialFailures = ignore.filterIgnoredWarningLists(header.Warnings, header.PartialFailures)
	header.Warnings = sanitizeSemanticWarnings(header.Warnings, repoDir)
	header.PartialFailures = sanitizeSemanticWarnings(header.PartialFailures, repoDir)
	headerLine, marshalErr := json.Marshal(header)
	if marshalErr != nil {
		return semanticHeader{}, semanticCounts{}, nil, fmt.Errorf("encode filtered semantic snapshot header: %w", marshalErr)
	}
	ignoredIDs := make(map[string]bool)
	line := 1
	for scanner.Scan() {
		line++
		text := bytes.TrimSpace(scanner.Bytes())
		if len(text) == 0 {
			continue
		}
		var record semanticRecord
		if err := json.Unmarshal(text, &record); err != nil {
			return semanticHeader{}, semanticCounts{}, nil, fmt.Errorf("parse semantic snapshot line %d: %w", line, err)
		}
		if err := validateSemanticRecordPath(&record); err != nil {
			return semanticHeader{}, semanticCounts{}, nil, fmt.Errorf("parse semantic snapshot line %d: %w", line, err)
		}
		if record.ID != "" && record.SemanticPath() != "" {
			ignoredIDs[record.ID] = ignoredIDs[record.ID] || ignore.Ignored(record.SemanticPath())
		}
	}
	if err := scanner.Err(); err != nil {
		return semanticHeader{}, semanticCounts{}, nil, err
	}
	var filtered bytes.Buffer
	filtered.Write(headerLine)
	filtered.WriteByte('\n')
	var counts semanticCounts
	scanner = newSemanticScanner(bytes.NewReader(raw))
	if !scanner.Scan() {
		return semanticHeader{}, semanticCounts{}, nil, errors.New("semantic snapshot missing header")
	}
	line = 1
	files := make(map[string]struct{})
	for scanner.Scan() {
		line++
		text := bytes.TrimSpace(scanner.Bytes())
		if len(text) == 0 {
			continue
		}
		recordRaw := append([]byte(nil), text...)
		var record semanticRecord
		if err := json.Unmarshal(recordRaw, &record); err != nil {
			return semanticHeader{}, semanticCounts{}, nil, fmt.Errorf("parse semantic snapshot line %d: %w", line, err)
		}
		if err := validateSemanticRecordPath(&record); err != nil {
			return semanticHeader{}, semanticCounts{}, nil, fmt.Errorf("parse semantic snapshot line %d: %w", line, err)
		}
		switch record.RecordType {
		case "file", "symbol", "relation":
			if ignore.Ignored(record.SemanticPath()) {
				continue
			}
			if record.RecordType == "relation" && relationEndpointIgnored(record, header.RepoKey, ignore, ignoredIDs) {
				continue
			}
			switch record.RecordType {
			case "file":
				if path := record.SemanticPath(); path != "" {
					files[path] = struct{}{}
				}
			case "symbol":
				if path := record.SemanticPath(); path != "" {
					files[path] = struct{}{}
				}
				counts.Symbols++
			case "relation":
				counts.Relations++
			}
			filteredRecord, marshalErr := json.Marshal(record)
			if marshalErr != nil {
				return semanticHeader{}, semanticCounts{}, nil, fmt.Errorf("encode semantic snapshot line %d: %w", line, marshalErr)
			}
			filtered.Write(filteredRecord)
			filtered.WriteByte('\n')
		case "summary":
			// The authoritative summary overrides the lean header for aggregate
			// metadata. Preserve it (sanitized) and fold it into the returned
			// header so re-ingestion paths report summary-accurate metadata.
			var summary semanticSummary
			if err := json.Unmarshal(recordRaw, &summary); err != nil {
				return semanticHeader{}, semanticCounts{}, nil, fmt.Errorf("parse semantic snapshot summary (line %d): %w", line, err)
			}
			keptWarnings, keptFailures := ignore.filterIgnoredWarningLists(summary.Warnings, summary.PartialFailures)
			summary.Warnings = sanitizeSemanticWarnings(keptWarnings, repoDir)
			summary.PartialFailures = sanitizeSemanticWarnings(keptFailures, repoDir)
			mergeSemanticSummary(&header, &summary)
			encoded, marshalErr := json.Marshal(summary)
			if marshalErr != nil {
				return semanticHeader{}, semanticCounts{}, nil, fmt.Errorf("encode semantic snapshot summary (line %d): %w", line, marshalErr)
			}
			filtered.Write(encoded)
			filtered.WriteByte('\n')
		default:
			// external + unknown future record types: preserve verbatim so the
			// re-filtered snapshot stays forward compatible.
			filtered.Write(recordRaw)
			filtered.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		return semanticHeader{}, semanticCounts{}, nil, err
	}
	counts.Files = len(files)
	return header, counts, filtered.Bytes(), nil
}

func semanticFileContentHash(repoDir, path string) string {
	if path == "" {
		return ""
	}
	clean, err := validateSemanticProviderPath(path)
	if err != nil {
		return ""
	}
	if err := base.RejectSymlinkPathComponents(repoDir, filepath.FromSlash(clean)); err != nil {
		return ""
	}
	f, err := os.Open(filepath.Join(repoDir, filepath.FromSlash(clean)))
	if err != nil {
		return ""
	}
	defer f.Close()
	hasher := sha256.New()
	limited := io.LimitReader(f, semanticParseCacheMaxFile+1)
	n, err := io.Copy(hasher, limited)
	if err != nil || n > semanticParseCacheMaxFile {
		return ""
	}
	sum := hasher.Sum(nil)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// semanticCompletenessAxis classifies partial provider failures. Intentional
// provider policy skips remain recorded but do not count against completeness.
// Of the remaining failures, exclusively benign parse errors under the tolerance
// fraction keep the axis ok; anything else (a non-parse failure code, an
// uncountable file set, or too many parse errors) degrades the brain.
func semanticCompletenessAxis(source *semanticSourceManifest) staleAxis {
	failures := len(source.PartialFailures)
	completenessFailures := 0
	parseErrors := 0
	intentionalSkips := 0
	for _, failure := range source.PartialFailures {
		if semanticIntentionalSkipCode(failure.Code) {
			intentionalSkips++
			continue
		}
		completenessFailures++
		if failure.Code == semanticParseErrorCode {
			parseErrors++
		}
	}
	if failures > 0 && source.Files <= 0 {
		return staleAxis{State: "degraded", Detail: strconv.Itoa(failures) + " partial failures"}
	}
	if intentionalSkips > 0 && source.Files > 0 {
		unparsedFiles := min(intentionalSkips, source.Files)
		parsedFiles := source.Files - unparsedFiles
		if parsedFiles < unparsedFiles {
			return staleAxis{State: "unsafe", Detail: fmt.Sprintf("%d/%d files were intentionally skipped by semantic provider", unparsedFiles, source.Files)}
		}
		if parsedFiles > 0 && source.Symbols == 0 {
			return staleAxis{State: "degraded", Detail: "semantic provider emitted zero symbols for parsed files"}
		}
	}
	if completenessFailures == 0 {
		return staleAxis{State: "ok"}
	}
	if parseErrors == completenessFailures && source.Files > 0 {
		fraction := float64(parseErrors) / float64(source.Files)
		if fraction < semanticParseErrorTolerance {
			return staleAxis{State: "ok", Detail: fmt.Sprintf("%d/%d files had tolerated parse errors", parseErrors, source.Files)}
		}
		return staleAxis{State: "degraded", Detail: fmt.Sprintf("%d partial failures in %.0f%% of files", parseErrors, fraction*100)}
	}
	return staleAxis{State: "degraded", Detail: strconv.Itoa(failures) + " partial failures"}
}
