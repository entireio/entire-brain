package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// semanticSummary is the authoritative trailing record emitted by entire-graph
// after the header and every file/symbol/relation/external record. The
// streaming header is intentionally lean; aggregate metadata lives here and
// overrides the header (see mergeSemanticSummary). Unknown shapes for stats and
// profile limits are preserved verbatim via json.RawMessage so Brain does not
// need to track every provider detail.
type semanticSummary struct {
	RecordType              string            `json:"record_type"`
	SchemaVersion           string            `json:"schema_version,omitempty"`
	Provider                string            `json:"provider,omitempty"`
	ProviderVersion         string            `json:"provider_version,omitempty"`
	Languages               []string          `json:"languages,omitempty"`
	LanguageTiers           map[string]string `json:"language_tiers,omitempty"`
	Capabilities            []string          `json:"capabilities,omitempty"`
	Profile                 string            `json:"profile,omitempty"`
	RelationSet             []string          `json:"relation_set,omitempty"`
	SkippedRelationFamilies []string          `json:"skipped_relation_families,omitempty"`
	Completeness            json.RawMessage   `json:"completeness,omitempty"`
	ProfileLimits           json.RawMessage   `json:"profile_limits,omitempty"`
	Stats                   json.RawMessage   `json:"stats,omitempty"`
	Warnings                []semanticWarning `json:"warnings,omitempty"`
	PartialFailures         []semanticWarning `json:"partial_failures,omitempty"`
}

// semanticStreamCounts tracks records observed while streaming. These are the
// "what came over the wire (after filtering)" counters used for progress and
// audit; the persisted manifest counts live in semanticCounts.
type semanticStreamCounts struct {
	Files           int
	Symbols         int
	Relations       int
	Externals       int
	Warnings        int
	PartialFailures int
	Unknown         int
	Dropped         int // malformed records skipped under tolerant ingest
}

// maxDroppedSemanticRecords bounds how many malformed records a tolerant ingest
// will skip before treating the stream as fundamentally broken and failing. A few
// stray corrupt lines should not fail a whole index; a flood almost certainly
// means the input is not a real snapshot.
const maxDroppedSemanticRecords = 1000

// semanticStrictIngest reports whether a single malformed record must abort the
// whole ingest (the historical behavior). It is off by default — one bad record
// from the untrusted entire-graph stream should not fail the entire index — and
// re-enabled with ENTIRE_BRAIN_STRICT_INGEST for validation/debugging.
func semanticStrictIngest() bool {
	return envBool("ENTIRE_BRAIN_STRICT_INGEST")
}

type semanticStreamResult struct {
	header        semanticHeader
	summary       *semanticSummary
	counts        semanticCounts
	stream        semanticStreamCounts
	extraWarnings []semanticWarning // synthesized warnings, e.g. unknown record types
	haveHeader    bool
}

// semanticStreamProgressInterval controls how often progress is reported while
// streaming records. It is a var so tests can lower it.
var semanticStreamProgressInterval = 5000

type semanticStreamScanConfig struct {
	ignore   brainIgnore
	repoDir  string
	progress func(phase string)
	// counts reports the running record tallies alongside the phase label, so a
	// caller can draw a determinate bar instead of an indeterminate spinner.
	// The label alone cannot serve that purpose: parsing numbers back out of a
	// human sentence is exactly the kind of coupling that breaks the next time
	// the sentence is reworded.
	counts func(files, symbols, relations int)
	// onRecord is invoked after each record line is processed with the record's
	// record_type. Production wires this to the inactivity watchdog and progress
	// reporting; tests use it to assert per-record (incremental) processing.
	onRecord func(recordType string)
}

// scanSemanticStream reads NDJSON records from r one line at a time, filters and
// sanitizes them, and writes the accepted records to out. It never holds the
// full snapshot in memory: each record is decoded, written, and discarded. The
// first record is the (lean) header; a trailing summary record, when present,
// is captured and re-emitted. Unknown and external records are preserved
// verbatim so future provider record types remain forward compatible.
//
// Filtering of relations by ignored endpoint IDs is single pass and therefore
// relies on the provider's ordering guarantee that symbol/file records stream
// before the relations that reference them. Endpoint paths encoded in relation
// IDs are always honored regardless of ordering.
func scanSemanticStream(r io.Reader, out io.Writer, cfg semanticStreamScanConfig) (semanticStreamResult, error) {
	var res semanticStreamResult
	scanner := newSemanticScanner(r)

	// Header: first non-empty line.
	for scanner.Scan() {
		text := bytes.TrimSpace(scanner.Bytes())
		if len(text) == 0 {
			continue
		}
		var header semanticHeader
		if err := json.Unmarshal(text, &header); err != nil {
			return res, fmt.Errorf("parse semantic snapshot header: %w", err)
		}
		header.RepoRoot = ""
		header.Warnings = sanitizeSemanticWarnings(cfg.ignore.FilterWarnings(header.Warnings), cfg.repoDir)
		header.PartialFailures = sanitizeSemanticWarnings(cfg.ignore.FilterWarnings(header.PartialFailures), cfg.repoDir)
		headerLine, err := json.Marshal(header)
		if err != nil {
			return res, fmt.Errorf("encode filtered semantic snapshot header: %w", err)
		}
		if err := writeNDJSONLine(out, headerLine); err != nil {
			return res, err
		}
		res.header = header
		res.haveHeader = true
		break
	}
	if err := scanner.Err(); err != nil {
		return res, err
	}
	if !res.haveHeader {
		// Empty stdout: let the caller decide whether this is a provider failure
		// (non-zero exit with diagnostics) or genuinely empty output.
		return res, nil
	}

	ignoredIDs := make(map[string]struct{})
	files := make(map[string]struct{})
	unknownTypes := make(map[string]struct{})
	strict := semanticStrictIngest()
	var firstDropErr error
	// dropRecord centralizes the malformed-line policy. In strict mode it returns
	// the error so the caller aborts; otherwise it counts the drop and signals the
	// caller to skip the line — unless drops exceed the cap, at which point the
	// stream is treated as broken and the error is returned.
	dropRecord := func(err error) (skip bool, fatal error) {
		if strict {
			return false, err
		}
		res.stream.Dropped++
		if firstDropErr == nil {
			firstDropErr = err
		}
		if res.stream.Dropped > maxDroppedSemanticRecords {
			return false, fmt.Errorf("semantic snapshot has too many malformed records (>%d); aborting: %w", maxDroppedSemanticRecords, err)
		}
		return true, nil
	}
	line := 1
	for scanner.Scan() {
		line++
		text := bytes.TrimSpace(scanner.Bytes())
		if len(text) == 0 {
			continue
		}
		// The scanner reuses its buffer, so copy before retaining/writing.
		raw := append([]byte(nil), text...)
		var probe struct {
			RecordType string `json:"record_type"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			if skip, fatal := dropRecord(fmt.Errorf("parse semantic snapshot line %d: %w", line, err)); fatal != nil {
				return res, fatal
			} else if skip {
				continue
			}
		}

		switch probe.RecordType {
		case "file", "symbol", "relation":
			var record semanticRecord
			if err := json.Unmarshal(raw, &record); err != nil {
				if skip, fatal := dropRecord(fmt.Errorf("parse semantic snapshot line %d: %w", line, err)); fatal != nil {
					return res, fatal
				} else if skip {
					continue
				}
			}
			// Path-safety validation is a security boundary, not a parse-tolerance
			// concern: a record whose path escapes the repo is a hostile signal and
			// must always abort the ingest, even in tolerant mode.
			if err := validateSemanticRecordPath(&record); err != nil {
				return res, fmt.Errorf("parse semantic snapshot line %d: %w", line, err)
			}
			if cfg.ignore.Ignored(record.semanticPath()) {
				if record.ID != "" {
					ignoredIDs[record.ID] = struct{}{}
				}
				break // skip ignored file/symbol record
			}
			if record.RecordType == "relation" && relationEndpointIgnored(record, cfg.ignore, ignoredIDs) {
				break
			}
			switch record.RecordType {
			case "file":
				if p := record.semanticPath(); p != "" {
					files[p] = struct{}{}
				}
				res.stream.Files++
			case "symbol":
				if p := record.semanticPath(); p != "" {
					files[p] = struct{}{}
				}
				res.counts.Symbols++
				res.stream.Symbols++
			case "relation":
				res.counts.Relations++
				res.stream.Relations++
			}
			encoded, err := json.Marshal(record)
			if err != nil {
				return res, fmt.Errorf("encode semantic snapshot line %d: %w", line, err)
			}
			if err := writeNDJSONLine(out, encoded); err != nil {
				return res, err
			}
		case "summary":
			var summary semanticSummary
			if err := json.Unmarshal(raw, &summary); err != nil {
				if skip, fatal := dropRecord(fmt.Errorf("parse semantic snapshot summary (line %d): %w", line, err)); fatal != nil {
					return res, fatal
				} else if skip {
					continue
				}
			}
			summary.Warnings = sanitizeSemanticWarnings(cfg.ignore.FilterWarnings(summary.Warnings), cfg.repoDir)
			summary.PartialFailures = sanitizeSemanticWarnings(cfg.ignore.FilterWarnings(summary.PartialFailures), cfg.repoDir)
			res.summary = &summary
			encoded, err := json.Marshal(summary)
			if err != nil {
				return res, fmt.Errorf("encode semantic snapshot summary (line %d): %w", line, err)
			}
			if err := writeNDJSONLine(out, encoded); err != nil {
				return res, err
			}
		case "external":
			res.stream.Externals++
			if err := writeNDJSONLine(out, raw); err != nil {
				return res, err
			}
		default:
			// Unknown future record type: preserve it verbatim so the snapshot
			// stays forward compatible, and record a machine-readable warning.
			res.stream.Unknown++
			unknownTypes[probe.RecordType] = struct{}{}
			if err := writeNDJSONLine(out, raw); err != nil {
				return res, err
			}
		}

		if cfg.onRecord != nil {
			cfg.onRecord(probe.RecordType)
		}
		if semanticStreamProgressInterval > 0 && (line-1)%semanticStreamProgressInterval == 0 {
			if cfg.progress != nil {
				cfg.progress(fmt.Sprintf("parsing sources (%d files, %d symbols, %d relations)", len(files), res.counts.Symbols, res.counts.Relations))
			}
			if cfg.counts != nil {
				cfg.counts(len(files), res.counts.Symbols, res.counts.Relations)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return res, err
	}

	res.counts.Files = len(files)
	res.extraWarnings = unknownRecordWarnings(unknownTypes)
	if res.stream.Dropped > 0 {
		detail := fmt.Sprintf("%d malformed record(s) skipped", res.stream.Dropped)
		if firstDropErr != nil {
			detail += "; first: " + firstDropErr.Error()
		}
		res.extraWarnings = append(res.extraWarnings, semanticWarning{
			Code:     "provider_malformed_record_dropped",
			Severity: "warning",
			Effect:   "records skipped; index may be incomplete",
			Detail:   detail,
		})
	}
	if res.summary != nil {
		res.stream.Warnings = len(res.summary.Warnings)
		res.stream.PartialFailures = len(res.summary.PartialFailures)
	} else {
		res.stream.Warnings = len(res.header.Warnings)
		res.stream.PartialFailures = len(res.header.PartialFailures)
	}
	return res, nil
}

func relationEndpointIgnored(record semanticRecord, ignore brainIgnore, ignoredIDs map[string]struct{}) bool {
	if p := semanticEndpointPath(record.FromID); p != "" && ignore.Ignored(p) {
		return true
	}
	if p := semanticEndpointPath(record.ToID); p != "" && ignore.Ignored(p) {
		return true
	}
	if _, ok := ignoredIDs[record.FromID]; ok {
		return true
	}
	if _, ok := ignoredIDs[record.ToID]; ok {
		return true
	}
	return false
}

func unknownRecordWarnings(unknownTypes map[string]struct{}) []semanticWarning {
	if len(unknownTypes) == 0 {
		return nil
	}
	types := make([]string, 0, len(unknownTypes))
	for t := range unknownTypes {
		types = append(types, t)
	}
	sort.Strings(types)
	warnings := make([]semanticWarning, 0, len(types))
	for _, t := range types {
		warnings = append(warnings, semanticWarning{
			Code:     "provider_unknown_record_type",
			Severity: "warning",
			Effect:   "record preserved verbatim for forward compatibility",
			Detail:   fmt.Sprintf("unknown semantic record_type %q", t),
		})
	}
	return warnings
}

// resetSnapshotTempFile rewinds the snapshot temp file and the rolling hash so
// the provider snapshot can be retried (e.g. without --ignore-file) without
// leaking the partial first attempt.
func resetSnapshotTempFile(f *os.File, hasher hash.Hash) error {
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("reset semantic snapshot temp file: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("reset semantic snapshot temp file: %w", err)
	}
	hasher.Reset()
	return nil
}

func writeNDJSONLine(out io.Writer, line []byte) error {
	if _, err := out.Write(line); err != nil {
		return err
	}
	if _, err := out.Write([]byte{'\n'}); err != nil {
		return err
	}
	return nil
}

// streamSemanticSnapshot invokes the semantic provider and streams its NDJSON
// stdout through scanSemanticStream into out. stdout is never buffered in full;
// stderr is captured separately into a bounded buffer. Cancellation flows
// through ctx, with a configurable overall deadline and an inactivity timeout so
// large repositories are not killed by a fixed short timeout while a hung
// provider still aborts.
func streamSemanticSnapshot(ctx context.Context, runner CommandRunner, repoDir string, indexOpts semanticIndexOptions, ignoreFiles []string, ignore brainIgnore, out io.Writer) (semanticStreamResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	args := []string{"graph", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network"}
	for _, path := range ignoreFiles {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		args = append(args, "--ignore-file", path)
	}
	if indexOpts.worktree {
		args = append(args, "--worktree")
	}
	if profile := strings.TrimSpace(indexOpts.profile); profile != "" {
		args = append(args, "--profile", profile)
	}

	overall := indexOpts.timeout
	if overall <= 0 {
		overall = semanticSnapshotTimeout
	}
	inactivity := indexOpts.inactivityTimeout
	if inactivity <= 0 {
		inactivity = semanticSnapshotInactivityTimeout
	}

	runCtx, cancel := context.WithTimeout(ctx, overall)
	defer cancel()

	var inactivityFired atomic.Bool
	activity := make(chan struct{}, 1)

	cfg := semanticStreamScanConfig{
		ignore:   ignore,
		repoDir:  repoDir,
		progress: indexOpts.progress,
		counts:   indexOpts.progressCounts,
		onRecord: func(string) {
			select {
			case activity <- struct{}{}:
			default:
			}
		},
	}

	// Semantic indexing requires a streaming runner so the production path is
	// always memory-bounded: stdout is consumed record-by-record and never
	// buffered in full. The production ExecRunner and the test fake both
	// implement CommandStreamer; a runner without it is a programming error.
	streamer, ok := runner.(CommandStreamer)
	if !ok {
		return semanticStreamResult{}, errors.New("semantic provider snapshot requires a streaming command runner (CommandStreamer)")
	}

	if inactivity > 0 {
		go watchSemanticInactivity(runCtx, inactivity, activity, cancel, &inactivityFired)
	}

	stream, err := streamer.Stream(runCtx, repoDir, indexOpts.graphBinary, args...)
	if err != nil {
		return semanticStreamResult{}, fmt.Errorf("semantic provider snapshot failed: %w", err)
	}
	defer stream.Close()

	res, scanErr := scanSemanticStream(stream.Stdout(), out, cfg)
	if scanErr != nil {
		// Drain remaining stdout so Wait does not block on a full pipe.
		_, _ = io.Copy(io.Discard, stream.Stdout())
	}
	stderr, waitErr := stream.Wait()

	if cerr := semanticStreamContextError(runCtx, ctx, overall, inactivity, &inactivityFired); cerr != nil {
		return res, cerr
	}
	if waitErr != nil {
		return res, providerSnapshotWaitError(waitErr, stderr, nil)
	}
	if scanErr != nil {
		return res, scanErr
	}
	if !res.haveHeader {
		return res, errors.New("semantic provider snapshot produced no output")
	}
	return res, nil
}

// semanticStreamContextError maps a cancelled run context to a descriptive
// error, distinguishing inactivity, the overall deadline, and external
// cancellation. It returns nil when the run context was not cancelled.
func semanticStreamContextError(runCtx, parent context.Context, overall, inactivity time.Duration, inactivityFired *atomic.Bool) error {
	if parent.Err() != nil {
		return fmt.Errorf("semantic provider snapshot canceled: %w", parent.Err())
	}
	if runCtx.Err() == nil {
		return nil
	}
	if inactivityFired.Load() {
		return fmt.Errorf("semantic provider snapshot stalled: no output for %s", inactivity)
	}
	return fmt.Errorf("semantic provider snapshot timed out after %s", overall)
}

func providerSnapshotWaitError(err error, stderr, stdout []byte) error {
	detail := strings.TrimSpace(string(stderr))
	if detail == "" {
		detail = strings.TrimSpace(string(stdout))
	}
	if detail != "" {
		return fmt.Errorf("semantic provider snapshot failed: %w: %s", err, truncateAgentWarning(detail))
	}
	return fmt.Errorf("semantic provider snapshot failed: %w", err)
}

func watchSemanticInactivity(ctx context.Context, d time.Duration, activity <-chan struct{}, cancel context.CancelFunc, fired *atomic.Bool) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-activity:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(d)
		case <-timer.C:
			fired.Store(true)
			cancel()
			return
		}
	}
}

// mergeSemanticSummary folds the authoritative summary record into the lean
// header. Summary fields override the header for aggregate metadata; identity
// fields (commit/tree/repo_key/schema) are only filled when the header omits
// them so the lean header remains the source of truth for identity.
func mergeSemanticSummary(header *semanticHeader, s *semanticSummary) {
	if s == nil {
		return
	}
	if len(s.Languages) > 0 {
		header.Languages = s.Languages
	}
	if len(s.LanguageTiers) > 0 {
		header.LanguageTiers = s.LanguageTiers
	}
	if len(s.Capabilities) > 0 {
		header.Capabilities = s.Capabilities
	}
	// The streaming header is lean; the summary is authoritative for aggregate
	// warning/failure metadata and overrides the header.
	header.Warnings = s.Warnings
	header.PartialFailures = s.PartialFailures
	if header.SchemaVersion == "" {
		header.SchemaVersion = s.SchemaVersion
	}
	if header.Provider == "" {
		header.Provider = s.Provider
	}
	if header.ProviderVersion == "" {
		header.ProviderVersion = s.ProviderVersion
	}
	if s.Profile != "" {
		header.Profile = s.Profile
	}
	if len(s.RelationSet) > 0 {
		header.RelationSet = s.RelationSet
	}
	if len(s.SkippedRelationFamilies) > 0 {
		header.SkippedRelationFamilies = s.SkippedRelationFamilies
	}
	if len(s.Completeness) > 0 {
		header.Completeness = s.Completeness
	}
	if len(s.ProfileLimits) > 0 {
		header.ProfileLimits = s.ProfileLimits
	}
	if len(s.Stats) > 0 {
		header.Stats = s.Stats
	}
}
