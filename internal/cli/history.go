package cli

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"
)

const (
	historyDirName           = "history"
	historyIndexFileName     = "index.json"
	historyIndexPath         = historyDirName + "/" + historyIndexFileName
	historyScanCacheFileName = "scan-cache.json.gz"
	historyScanCachePath     = historyDirName + "/" + historyScanCacheFileName
	historyMaxLineBytes      = 1024 * 1024
	// historyScanCacheVersion gates reuse of cached per-file scan results.
	// Bump it whenever the history record extraction/classification logic
	// changes so stale cached records are discarded on the next refresh.
	// v2: index user prompts as "request" records.
	// v3: stop EXTRACTING request records during scan (the v2 indexing) — they were
	// excluded from general ranking (measured noise) and the only surfaces that
	// could query them (inspect requests / brain_history kind=requests) were
	// removed. The request-kind gates/filtering are kept as defensive support for
	// stale indexes; bumping discards per-file scan caches that still hold request
	// records so they stop reappearing on refresh.
	// v4: RE-introduce request extraction (wrapper-filtered, via
	// transcriptUserText). Two Phase 2 consumers reopened the v3 closed
	// question with new evidence: `brief --handoff` needs each session's
	// opening request and the history eval's midtask stratum mines follow-up
	// questions as queries. The v3 noise concern is unchanged and unaffected:
	// both rankers still exclude kind=request from general ranking.
	// v5: extract experimental conversation "exchange" records (conversation.go)
	// during the scan; older caches do not contain them.
	historyScanCacheVersion = 5
)

type historySourceManifest struct {
	GeneratedAt         time.Time `json:"generated_at"`
	IndexPath           string    `json:"index_path"`
	SessionsFingerprint string    `json:"sessions_fingerprint,omitempty"`
	Records             int       `json:"records"`
	Decisions           int       `json:"decisions"`
	Learnings           int       `json:"learnings"`
	Validations         int       `json:"validations"`
	ToolCalls           int       `json:"tool_calls"`
	CodeFacts           int       `json:"code_facts"`
	Exchanges           int       `json:"exchanges,omitempty"`
	IncompleteExchanges int       `json:"incomplete_exchanges,omitempty"`
	Warnings            []string  `json:"warnings,omitempty"`
}

type historyIndex struct {
	GeneratedAt time.Time       `json:"generated_at"`
	Records     []historyRecord `json:"records"`
	Warnings    []string        `json:"warnings,omitempty"`
}

type historyRecord struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`
	Branch  string   `json:"branch,omitempty"`
	Path    string   `json:"path"`
	Line    int      `json:"line"`
	Summary string   `json:"summary"`
	Terms   []string `json:"terms,omitempty"`

	// Experimental conversation-exchange extension (kind "exchange", see
	// conversation.go). All fields are additive and omitted for classic
	// records, so the established history JSON is byte-identical for them.
	// Summary holds the bounded deterministic search projection — the only
	// conversation body stored in the index; full text lives only in the
	// exported transcript and is re-parsed by get.
	SessionID   string `json:"session_id,omitempty"`
	Agent       string `json:"agent,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"` // RFC3339 session time
	EndLine     int    `json:"end_line,omitempty"`   // inclusive 1-based range end
	TurnOrdinal int    `json:"turn_ordinal,omitempty"`
	// RangeIncomplete is set when the exact source range could not be proven
	// (range_complete=false in the contract; inverted so omitempty works).
	RangeIncomplete     bool     `json:"range_incomplete,omitempty"`
	ContentRole         string   `json:"content_role,omitempty"` // "historical_evidence"
	ProjectionTruncated bool     `json:"projection_truncated,omitempty"`
	SourceDigest        string   `json:"source_digest,omitempty"` // transcript digest at extraction
	RequestDigest       string   `json:"request_digest,omitempty"`
	IdentityDegraded    bool     `json:"identity_degraded,omitempty"`
	ToolNames           []string `json:"tool_names,omitempty"`
}

type scoredHistoryRecord struct {
	Record historyRecord
	Score  int
	Order  int
}

type historyFragment struct {
	Text   string
	Source string
}

type historySessionFile struct {
	Path        string
	SortTime    time.Time
	Size        int64
	ModUnixNano int64
}

// historyScanCache memoizes the parsed history records for each session
// transcript so a refresh only re-scans files whose size or mtime changed.
// Session transcripts are content-stable across refreshes (the export step
// reuses unchanged transcripts via the export cursor), so the (size, mtime)
// pair is a reliable change signal and lets the index skip both the I/O and
// the parse for the vast majority of files on every run.
type historyScanCache struct {
	Version int                              `json:"version"`
	Files   map[string]historyScanCacheEntry `json:"files"`
}

type historyScanCacheEntry struct {
	Size        int64           `json:"size"`
	ModUnixNano int64           `json:"mod_unix_nano"`
	Records     []historyRecord `json:"records"`
	// IncompleteExchanges preserves the per-file diagnostic (exchanges opened
	// with no visible assistant narrative) across cache reuse.
	IncompleteExchanges int `json:"incomplete_exchanges,omitempty"`
}

func newHistoryIndexCommand(opts Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "history [path]",
		Short: "Rebuild the decision/rationale history index from exported session transcripts",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := "."
			if opts.Env.RepoRoot != "" {
				target = opts.Env.RepoRoot
			}
			if len(args) == 1 {
				target = args[0]
			}
			return runHistoryIndex(cmd.Context(), cmd, opts, target)
		},
	}
	return cmd
}

func runHistoryIndex(ctx context.Context, cmd *cobra.Command, opts Options, target string) error {
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return err
	}
	if !local {
		return fmt.Errorf("refresh history requires a local repository path: %s", target)
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}
	source, err := writeBrainHistoryIndexAndSource(storage.BrainDir, opts.Now().UTC(), nil)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "indexed %d history records: %s\n", source.Records, filepath.Join(storage.BrainDir, filepath.FromSlash(source.IndexPath)))
	// History vectors ride the history stage (standalone here, and the same
	// sequence inside the full `refresh`): only under the ENTIRE_BRAIN_EMBEDDER
	// opt-in, see history_vec.go for the gate and sizing rationale.
	if strings.TrimSpace(os.Getenv("ENTIRE_BRAIN_EMBEDDER")) != "" {
		e := historySemanticEmbedder(defaultEmbedder())
		store, storeOK := historyVectorStoreFor(storage.BrainDir, e)
		switch {
		case e == nil:
			fmt.Fprintln(cmd.OutOrStdout(), "history vectors: skipped (embed server unavailable)")
		case !storeOK:
			fmt.Fprintln(cmd.OutOrStdout(), "history vectors: skipped (requires the brain_cgo build)")
		default:
			index, ierr := loadBrainHistoryIndex(storage.BrainDir, source)
			if ierr != nil {
				return ierr
			}
			// Flush-batch progress to stderr: the first sync of a large repo
			// embeds every record and can run for hours — silence would read
			// as a hang.
			added, dropped, total, serr := syncHistoryVectors(store, index, e, func(done, totalNew int) {
				fmt.Fprintf(cmd.ErrOrStderr(), "history vectors: %d/%d new %s embedded\n", done, totalNew, pluralUnit("record", totalNew))
			})
			if serr != nil {
				return serr
			}
			fmt.Fprintf(cmd.OutOrStdout(), "history vectors: %d embedded, %d pruned (%d total)\n", added, dropped, total)
		}
	}
	return nil
}

// historyIndexProgress reports incremental progress while the history index is
// rebuilt. done counts session files processed so far out of total. It is
// optional; pass nil when no progress reporting is needed.
type historyIndexProgress func(done, total int)

func writeBrainHistoryIndexAndSource(outputDir string, now time.Time, progress historyIndexProgress) (*historySourceManifest, error) {
	var source *historySourceManifest
	err := withBrainWriteLock(outputDir, func() error {
		var runErr error
		source, runErr = writeBrainHistoryIndexAndSourceLocked(outputDir, now, progress)
		return runErr
	})
	return source, err
}

func writeBrainHistoryIndexAndSourceLocked(outputDir string, now time.Time, progress historyIndexProgress) (*historySourceManifest, error) {
	index, source, err := buildBrainHistoryIndex(outputDir, now, progress)
	if err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	if err := writeBrainRelativeFileAtomic(outputDir, historyIndexPath, data, 0o600); err != nil {
		return nil, fmt.Errorf("write history index: %w", err)
	}
	// Build the derived BM25 index alongside its truth so the search/query verbs
	// do not pay a first-query rebuild. Best-effort: the query path rebuilds it
	// lazily on any failure, so this must never fail the refresh.
	if db, ftsErr := openHistoryFTSLocked(outputDir, index); ftsErr == nil {
		_ = db.Close()
	}
	manifest, err := loadBrainManifest(outputDir)
	if err != nil {
		return nil, err
	}
	if manifest.Sources == nil {
		manifest.Sources = &brainSources{}
	}
	manifest.Sources.History = source
	if manifest.GeneratedAt.IsZero() {
		manifest.GeneratedAt = now
	}
	if err := writeBrainManifestAndReadme(outputDir, *manifest); err != nil {
		return nil, err
	}
	return source, nil
}

func buildBrainHistoryIndex(outputDir string, now time.Time, progress historyIndexProgress) (historyIndex, *historySourceManifest, error) {
	sessionsRoot := filepath.Join(outputDir, exportSessionsDirectory)
	index := historyIndex{GeneratedAt: now}
	if _, err := os.Stat(sessionsRoot); err != nil {
		if os.IsNotExist(err) {
			return index, nil, errors.New("session history missing; run `entire brain refresh sessions` first")
		}
		return index, nil, err
	}
	files, err := collectHistorySessionFiles(sessionsRoot, &index.Warnings)
	if err != nil {
		return index, nil, err
	}

	prevCache := loadHistoryScanCache(outputDir)
	newCache := historyScanCache{Version: historyScanCacheVersion, Files: make(map[string]historyScanCacheEntry, len(files))}
	manifest, _ := loadBrainManifest(outputDir)
	branchByPath := historyBranchByTranscriptPath(manifest)
	sessionByPath := historySessionByTranscriptPath(manifest)
	repoKey := ""
	if manifest != nil {
		repoKey = manifest.RepoKey
	}

	total := len(files)
	if progress != nil {
		progress(0, total)
	}
	seenDecisions := map[string]struct{}{}
	incompleteExchanges := 0
	for i, file := range files {
		rel, relErr := filepath.Rel(outputDir, file.Path)
		if relErr != nil {
			rel = file.Path
		}
		rel = filepath.ToSlash(rel)

		var records []historyRecord
		var incomplete int
		if cached, ok := prevCache.Files[rel]; ok && cached.Size == file.Size && cached.ModUnixNano == file.ModUnixNano {
			records = cached.Records
			incomplete = cached.IncompleteExchanges
		} else {
			scanned, scanErr := scanHistoryFile(outputDir, file.Path)
			if scanErr != nil {
				index.Warnings = append(index.Warnings, scanErr.Error())
				if progress != nil {
					progress(i+1, total)
				}
				continue
			}
			records = scanned
			// Conversation exchanges are an additive experimental projection; an
			// extraction failure downgrades to classic records with a warning
			// rather than dropping the whole file.
			conversationScan, convErr := scanConversationTranscript(file.Path)
			if convErr != nil {
				index.Warnings = append(index.Warnings, fmt.Sprintf("conversation exchanges skipped for %s: %v", rel, convErr))
			} else {
				records = append(records, conversationExchangeRecords(rel, conversationScan)...)
				incomplete = conversationScan.Incomplete
			}
		}
		records = annotateHistoryRecordBranches(records, rel, branchByPath)
		records = annotateConversationIdentity(records, rel, repoKey, sessionByPath)
		incompleteExchanges += incomplete
		// Only files that scanned cleanly (or were reused) are cached; a file
		// that errored is left out so the next refresh retries it.
		newCache.Files[rel] = historyScanCacheEntry{Size: file.Size, ModUnixNano: file.ModUnixNano, Records: records, IncompleteExchanges: incomplete}

		for _, record := range records {
			if record.Kind == "decision" {
				dedupeKey := normalizeHistorySearchText(record.Summary)
				if _, ok := seenDecisions[dedupeKey]; ok {
					continue
				}
				seenDecisions[dedupeKey] = struct{}{}
			}
			index.Records = append(index.Records, record)
		}
		if progress != nil {
			progress(i+1, total)
		}
	}
	saveHistoryScanCache(outputDir, newCache)
	sort.Slice(index.Records, func(i, j int) bool {
		if index.Records[i].Kind != index.Records[j].Kind {
			return index.Records[i].Kind < index.Records[j].Kind
		}
		if index.Records[i].Path != index.Records[j].Path {
			return index.Records[i].Path < index.Records[j].Path
		}
		return index.Records[i].Line < index.Records[j].Line
	})
	source := &historySourceManifest{
		GeneratedAt:         now,
		IndexPath:           historyIndexPath,
		SessionsFingerprint: brainSessionsFingerprint(outputDir),
		Records:             len(index.Records),
		IncompleteExchanges: incompleteExchanges,
		Warnings:            append([]string(nil), index.Warnings...),
	}
	for _, record := range index.Records {
		switch record.Kind {
		case "decision":
			source.Decisions++
		case "learning":
			source.Learnings++
		case "validation":
			source.Validations++
		case "tool_call":
			source.ToolCalls++
		case "code_fact":
			source.CodeFacts++
		case conversationKind:
			source.Exchanges++
		}
	}
	return index, source, nil
}

// historySessionByTranscriptPath maps each exported transcript path to its
// session manifest entry so exchange records can carry session identity.
func historySessionByTranscriptPath(manifest *exportManifest) map[string]exportSession {
	out := map[string]exportSession{}
	if manifest == nil || manifest.Sources == nil || manifest.Sources.Sessions == nil {
		return out
	}
	for _, session := range manifest.Sources.Sessions.Sessions {
		rel := filepath.ToSlash(strings.TrimSpace(session.TranscriptPath))
		if rel == "" {
			continue
		}
		out[rel] = session
	}
	return out
}

// annotateConversationIdentity fills the manifest-derived identity of exchange
// records: session id, agent, session time, and the stable conversation: ID.
// Like branch annotation it only fills empty fields, so records reused from the
// scan cache keep their first-computed identity.
func annotateConversationIdentity(records []historyRecord, rel, repoKey string, sessionByPath map[string]exportSession) []historyRecord {
	needsAnnotation := false
	for i := range records {
		if records[i].Kind == conversationKind && records[i].ID == "" {
			needsAnnotation = true
			break
		}
	}
	if !needsAnnotation {
		return records
	}
	session, hasSession := sessionByPath[rel]
	out := append([]historyRecord(nil), records...)
	for i := range out {
		if out[i].Kind != conversationKind || out[i].ID != "" {
			continue
		}
		sessionID := ""
		if hasSession {
			sessionID = strings.TrimSpace(session.SessionID)
			if out[i].Agent == "" {
				out[i].Agent = session.Agent
			}
			if out[i].CreatedAt == "" && !session.CreatedAt.IsZero() {
				out[i].CreatedAt = session.CreatedAt.UTC().Format(time.RFC3339)
			}
		}
		out[i].SessionID = sessionID
		id, degraded := conversationExchangeID(repoKey, sessionID, out[i].TurnOrdinal, out[i].RequestDigest, out[i].SourceDigest)
		out[i].ID = id
		out[i].IdentityDegraded = degraded
	}
	return out
}

func historyBranchByTranscriptPath(manifest *exportManifest) map[string]string {
	out := map[string]string{}
	if manifest == nil || manifest.Sources == nil || manifest.Sources.Sessions == nil {
		return out
	}
	defaultBranch := strings.TrimSpace(manifest.Sources.Sessions.DefaultBranch)
	if defaultBranch == "" {
		defaultBranch = strings.TrimSpace(manifest.DefaultBranch)
	}
	if defaultBranch == "" {
		defaultBranch = distillDefaultBranch
	}
	for _, session := range manifest.Sources.Sessions.Sessions {
		rel := filepath.ToSlash(strings.TrimSpace(session.TranscriptPath))
		if rel == "" {
			continue
		}
		branch := strings.TrimSpace(session.Branch)
		if branch == "" {
			branch = defaultBranch
		}
		out[rel] = branch
	}
	return out
}

func annotateHistoryRecordBranches(records []historyRecord, rel string, branchByPath map[string]string) []historyRecord {
	branch := historyRecordBranchForPath(rel, branchByPath)
	if branch == "" {
		return records
	}
	out := append([]historyRecord(nil), records...)
	for i := range out {
		if out[i].Branch == "" {
			out[i].Branch = branch
		}
	}
	return out
}

func historyRecordBranchForPath(path string, branchByPath map[string]string) string {
	rel := filepath.ToSlash(strings.TrimSpace(path))
	if rel == "" {
		return ""
	}
	if branch := strings.TrimSpace(branchByPath[rel]); branch != "" {
		return branch
	}
	if strings.HasPrefix(rel, "sessions/branches/") {
		rest := strings.TrimPrefix(rel, "sessions/branches/")
		if idx := strings.Index(rest, "/"); idx > 0 {
			return rest[:idx]
		}
	}
	if strings.HasPrefix(rel, "sessions/") {
		rest := strings.TrimPrefix(rel, "sessions/")
		if idx := strings.Index(rest, "/"); idx > 0 {
			return rest[:idx]
		}
	}
	return ""
}

// loadHistoryScanCache reads the per-file scan cache. It always returns a
// usable (non-nil map) value: any read/parse error or a version mismatch
// yields an empty cache so the index simply rebuilds from scratch.
// The cache stores every indexed record for every session, which at scale is
// hundreds of megabytes of JSON. It is gzip-compressed on disk to bound the
// footprint (the record text compresses heavily).
func loadHistoryScanCache(outputDir string) historyScanCache {
	empty := historyScanCache{Version: historyScanCacheVersion, Files: map[string]historyScanCacheEntry{}}
	data, err := os.ReadFile(filepath.Join(outputDir, filepath.FromSlash(historyScanCachePath)))
	if err != nil {
		return empty
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return empty
	}
	defer gz.Close()
	// Bound the decompressed stream so a gzip bomb cannot exhaust memory; an
	// over-cap or truncated stream simply rebuilds the cache from scratch.
	limited := io.LimitReader(gz, semanticSnapshotMaxBytes())
	var cache historyScanCache
	if err := json.NewDecoder(limited).Decode(&cache); err != nil {
		return empty
	}
	if cache.Version != historyScanCacheVersion || cache.Files == nil {
		return empty
	}
	return cache
}

// saveHistoryScanCache persists the per-file scan cache. Failures are
// non-fatal: a missing or unwritable cache only costs a full rebuild next time.
func saveHistoryScanCache(outputDir string, cache historyScanCache) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if err := json.NewEncoder(gz).Encode(cache); err != nil {
		return
	}
	if err := gz.Close(); err != nil {
		return
	}
	_ = writeBrainRelativeFileAtomic(outputDir, historyScanCachePath, buf.Bytes(), 0o600)
}

func brainSessionsFingerprint(outputDir string) string {
	manifest, err := loadBrainManifest(outputDir)
	if err != nil || manifest.Sources == nil || manifest.Sources.Sessions == nil {
		return ""
	}
	return sessionSourceFingerprint(manifest.Sources.Sessions)
}

func sessionSourceFingerprint(source *sessionSourceManifest) string {
	if source == nil {
		return ""
	}
	type fingerprintSession struct {
		SessionID        string `json:"session_id"`
		LatestCheckpoint string `json:"latest_checkpoint_id"`
		Branch           string `json:"branch,omitempty"`
		TranscriptPath   string `json:"transcript_path"`
	}
	payload := struct {
		DefaultBranch      string               `json:"default_branch,omitempty"`
		TranscriptMode     string               `json:"transcript_mode"`
		Scope              string               `json:"scope"`
		LatestCheckpointID string               `json:"latest_checkpoint_id,omitempty"`
		Sessions           []fingerprintSession `json:"sessions"`
	}{
		DefaultBranch:      source.DefaultBranch,
		TranscriptMode:     source.TranscriptMode,
		Scope:              source.Scope,
		LatestCheckpointID: source.LatestCheckpointID,
		Sessions:           make([]fingerprintSession, 0, len(source.Sessions)),
	}
	for _, session := range source.Sessions {
		payload.Sessions = append(payload.Sessions, fingerprintSession{
			SessionID:        session.SessionID,
			LatestCheckpoint: session.LatestCheckpoint,
			Branch:           session.Branch,
			TranscriptPath:   session.TranscriptPath,
		})
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func collectHistorySessionFiles(sessionsRoot string, warnings *[]string) ([]historySessionFile, error) {
	var files []historySessionFile
	err := filepath.WalkDir(sessionsRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			*warnings = append(*warnings, err.Error())
			return nil
		}
		if d.IsDir() {
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".jsonl" && ext != ".json" && ext != ".md" && ext != ".txt" {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			*warnings = append(*warnings, statErr.Error())
			return nil
		}
		files = append(files, historySessionFile{
			Path:        path,
			SortTime:    historySessionSortTime(path, info.ModTime()),
			Size:        info.Size(),
			ModUnixNano: info.ModTime().UnixNano(),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool {
		if !files[i].SortTime.Equal(files[j].SortTime) {
			return files[i].SortTime.After(files[j].SortTime)
		}
		return files[i].Path < files[j].Path
	})
	return files, nil
}

func historySessionSortTime(path string, fallback time.Time) time.Time {
	base := filepath.Base(path)
	if len(base) >= len("20060102T150405Z") {
		if parsed, err := time.Parse("20060102T150405Z", base[:len("20060102T150405Z")]); err == nil {
			return parsed
		}
	}
	return fallback
}

func scanHistoryFile(outputDir, path string) ([]historyRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rel, _ := filepath.Rel(outputDir, path)
	rel = filepath.ToSlash(rel)
	// Document-form transcripts (e.g. opencode: one pretty-printed JSON
	// document) have no individually parseable lines, so the line scanner
	// below indexes nothing from them. Probe the first line the same way
	// distill does and route them through the shared document parser instead.
	if records, isDocument, err := scanDocumentHistoryFile(f, rel); err != nil {
		return nil, err
	} else if isDocument {
		return records, nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil { // rewind the probe read
		return nil, err
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), historyMaxLineBytes)
	var records []historyRecord
	lineNumber := 0
	allowRawText := filepath.Ext(path) == ".md" || filepath.Ext(path) == ".txt"
	for scanner.Scan() {
		lineNumber++
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		// Apply the cheap keyword fast-path BEFORE the JSON decode: most transcript
		// lines are irrelevant to indexing, so on a large history this skips the
		// per-line Unmarshal for the majority of records.
		if !historyLineMayContainIndexedContent(text) {
			continue
		}
		// Decode at most once for the narrative pass. Only transcript records are
		// JSON objects, so skip the decode for raw .md/.txt lines that can't be one
		// (cheap leading-'{' check); those fall through to the raw-text path.
		var fragments []historyFragment
		parsedOK := false
		if strings.HasPrefix(text, "{") {
			var obj map[string]any
			if json.Unmarshal([]byte(text), &obj) == nil {
				fragments = extractHistoryJSONFragments(obj)
				parsedOK = true
			}
		}
		if !parsedOK && allowRawText {
			fragments = []historyFragment{{Text: text, Source: "text"}}
		}
		for _, fragment := range fragments {
			fragment.Text = strings.TrimSpace(fragment.Text)
			if fragment.Text == "" {
				continue
			}
			for _, kind := range classifyHistoryFragment(fragment) {
				records = append(records, historyRecord{
					ID:      historyRecordID(rel, lineNumber, kind, fragment.Text),
					Kind:    kind,
					Path:    rel,
					Line:    lineNumber,
					Summary: truncateString(cleanHistorySummary(fragment.Text), 700),
					Terms:   historyTerms(fragment.Text),
				})
			}
		}
	}
	return records, scanner.Err()
}

// scanDocumentHistoryFile detects and indexes a document-form transcript (see
// parseDocumentConversation). isDocument=false means the file is line-oriented
// (or not a transcript at all) and the caller's line scanner should handle it
// after rewinding past the probe read. Mirrors the JSONL indexing policy:
// assistant text is narrative, user turns are skipped; record lines anchor to
// the document line each message object opens on.
func scanDocumentHistoryFile(f *os.File, rel string) (records []historyRecord, isDocument bool, err error) {
	probe := make([]byte, 4096)
	n, readErr := f.Read(probe)
	if readErr != nil && readErr != io.EOF {
		return nil, false, readErr
	}
	firstLine, _, _ := strings.Cut(strings.TrimSpace(string(probe[:n])), "\n")
	if !strings.HasPrefix(firstLine, "{") || json.Valid([]byte(firstLine)) {
		return nil, false, nil // JSONL or non-JSON: the line scanner's job
	}
	data, err := safeReadAll(io.MultiReader(bytes.NewReader(probe[:n]), f), maxDocumentTranscriptBytes, "document transcript "+rel)
	if err != nil {
		return nil, false, err
	}
	messages, ok := parseDocumentConversation(string(data))
	if !ok {
		return nil, false, nil
	}
	for _, message := range messages {
		if message.Role != "assistant" || message.Text == "" {
			continue
		}
		fragment := historyFragment{Text: message.Text, Source: "assistant_message"}
		for _, kind := range classifyHistoryFragment(fragment) {
			records = append(records, historyRecord{
				ID:      historyRecordID(rel, message.Line, kind, fragment.Text),
				Kind:    kind,
				Path:    rel,
				Line:    message.Line,
				Summary: truncateString(cleanHistorySummary(fragment.Text), 700),
				Terms:   historyTerms(fragment.Text),
			})
		}
	}
	return records, true, nil
}

// extractHistoryJSONFragments returns the indexable fragments of one
// transcript record: the kind-specific extraction (decisions, validations,
// tool calls, …) plus — when the record is a real user turn — a
// "user_prompt" fragment that classifies as a request record.
//
// Request records were extracted in scan-cache v2, removed in v3 ("measured
// noise in general ranking, and no surface queried them"), and re-introduced
// in v4: two Phase 2 consumers now need them — `brief --handoff` (a session's
// opening request) and the history eval's midtask stratum (follow-up
// questions as queries). The v3 noise concern remains addressed the way it
// always was: both rankers exclude kind=request from general ranking; the
// records exist for trajectory surfaces, not retrieval.
func extractHistoryJSONFragments(obj map[string]any) []historyFragment {
	fragments := extractHistoryRecordFragments(obj)
	// transcriptUserText handles all four transcript dialects (codex
	// response_item/event_msg, Claude user, pi message); wrapper injections
	// (environment context, caveats) are filtered with the same predicate
	// every other request consumer uses.
	if text := strings.TrimSpace(transcriptUserText(obj)); text != "" && !isWrapperRequest(text) {
		fragments = append(fragments, historyFragment{Text: text, Source: "user_prompt"})
	}
	return fragments
}

func extractHistoryRecordFragments(obj map[string]any) []historyFragment {
	recordType := jsonString(obj["type"])
	payload := jsonMap(obj["payload"])
	switch recordType {
	case "session_meta", "turn_context", "permission-mode":
		return nil
	case "agent_message":
		return stringFragment(obj["message"], "assistant_message")
	case "event_msg":
		return extractCodexEventFragments(payload)
	case "response_item":
		return extractCodexResponseFragments(payload)
	case "assistant":
		return extractClaudeMessageFragments(obj, "assistant")
	case "user":
		return extractClaudeToolResultFragments(obj)
	case "message":
		// pi wraps every turn as {"type":"message","message":{role,content}}.
		// Without this case the default branch's jsonString(obj["message"]) is
		// "" for a map, so pi sessions indexed to nothing (the same blind spot
		// distillConversationText covers for distill). Mirror the Claude
		// routing: assistant text is narrative, toolResult content can carry
		// code facts, user turns are skipped (as for codex user_message).
		message := jsonMap(obj["message"])
		switch jsonString(message["role"]) {
		case "assistant":
			return extractContentFragments(message["content"], "assistant_message")
		case "toolResult":
			return extractToolResultFacts(distillTextBlocks(message["content"]))
		default:
			return nil
		}
	case "progress":
		return nil
	default:
		if message := jsonString(obj["message"]); message != "" {
			return []historyFragment{{Text: message, Source: "message"}}
		}
		return nil
	}
}

func extractCodexEventFragments(payload map[string]any) []historyFragment {
	switch jsonString(payload["type"]) {
	case "agent_message":
		return stringFragment(payload["message"], "assistant_message")
	case "user_message":
		return nil
	case "task_complete":
		return stringFragment(payload["last_agent_message"], "final_answer")
	case "exec_command_end":
		command := compactJSONValue(payload["command"])
		return []historyFragment{{Text: command, Source: "tool_call:exec_command"}}
	case "patch_apply_end":
		parts := []string{"apply_patch", jsonString(payload["stdout"]), jsonString(payload["stderr"]), compactJSONValue(payload["changes"])}
		return []historyFragment{{Text: strings.Join(parts, " "), Source: "tool_call:apply_patch"}}
	default:
		return nil
	}
}

func extractCodexResponseFragments(payload map[string]any) []historyFragment {
	switch jsonString(payload["type"]) {
	case "message":
		role := jsonString(payload["role"])
		if role != "assistant" {
			return nil
		}
		return extractContentFragments(payload["content"], role+"_message")
	case "function_call":
		name := jsonString(payload["name"])
		return []historyFragment{{Text: strings.TrimSpace(name + " " + compactJSONValue(payload["arguments"])), Source: historyToolCallSource(name)}}
	case "custom_tool_call":
		name := jsonString(payload["name"])
		return []historyFragment{{Text: strings.TrimSpace(name + " " + compactJSONValue(payload["input"])), Source: historyToolCallSource(name)}}
	case "function_call_output":
		return extractToolResultFacts(jsonString(payload["output"]))
	default:
		return nil
	}
}

func extractClaudeToolResultFragments(obj map[string]any) []historyFragment {
	message := jsonMap(obj["message"])
	if len(message) == 0 {
		return nil
	}
	content, ok := message["content"].([]any)
	if !ok {
		return nil
	}
	var fragments []historyFragment
	for _, item := range content {
		block := jsonMap(item)
		if jsonString(block["type"]) != "tool_result" {
			continue
		}
		fragments = append(fragments, extractToolResultFacts(firstNonEmptyString(block["content"], block["text"]))...)
	}
	return fragments
}

func extractClaudeMessageFragments(obj map[string]any, role string) []historyFragment {
	message := jsonMap(obj["message"])
	if len(message) == 0 {
		return nil
	}
	return extractContentFragments(message["content"], role+"_message")
}

func extractContentFragments(content any, source string) []historyFragment {
	switch value := content.(type) {
	case string:
		return []historyFragment{{Text: value, Source: source}}
	case []any:
		var fragments []historyFragment
		for _, item := range value {
			block := jsonMap(item)
			if len(block) == 0 {
				if text := fmt.Sprint(item); strings.TrimSpace(text) != "" {
					fragments = append(fragments, historyFragment{Text: text, Source: source})
				}
				continue
			}
			blockType := jsonString(block["type"])
			switch blockType {
			case "tool_use":
				name := jsonString(block["name"])
				text := strings.TrimSpace(name + " " + compactJSONValue(block["input"]))
				fragments = append(fragments, historyFragment{Text: text, Source: historyToolCallSource(name)})
				fragments = append(fragments, extractToolResultFacts(text)...)
			case "tool_result":
				fragments = append(fragments, extractToolResultFacts(firstNonEmptyString(block["content"], block["text"]))...)
			default:
				if text := firstNonEmptyString(block["text"], block["input_text"], block["output_text"], block["content"]); text != "" {
					fragments = append(fragments, historyFragment{Text: text, Source: source})
				}
			}
		}
		return fragments
	default:
		return nil
	}
}

func extractToolResultFacts(text string) []historyFragment {
	text = strings.TrimSpace(text)
	if text == "" || !historyTextHasCodeFactSignal(text) {
		return nil
	}
	lines := strings.Split(text, "\n")
	seen := map[string]struct{}{}
	var fragments []historyFragment
	lastEnd := -1
	for i, line := range lines {
		if i < lastEnd {
			continue
		}
		if !historyLineHasCodeFactSignal(line) {
			continue
		}
		start := max(0, i-2)
		end := min(len(lines), i+4)
		snippet := cleanHistoryCodeFactSnippet(cleanHistorySummary(strings.Join(lines[start:end], " ")))
		if snippet == "" {
			continue
		}
		if _, ok := seen[snippet]; ok {
			continue
		}
		seen[snippet] = struct{}{}
		fragments = append(fragments, historyFragment{Text: snippet, Source: "tool_result_fact"})
		lastEnd = end
		if len(fragments) >= 8 {
			break
		}
	}
	return fragments
}

func cleanHistoryCodeFactSnippet(value string) string {
	value = strings.TrimSpace(strings.TrimPrefix(value, "content:"))
	if value == "" {
		return ""
	}
	return truncateString(value, 4000)
}

func historyTextHasCodeFactSignal(text string) bool {
	lower := strings.ToLower(text)
	return containsAny(lower,
		"attributionbasecommit", "realignattributionbase", "human_added", "human added",
		"resolve transcript path", "resolvetranscriptpath", "transcriptpath", "state.transcriptpath",
		"reresolvestonestedlayout", "re-resolved path", "subsequent reads use",
		"brainignore", ".brainignore", ".github", "github workflow", "workflow/tooling",
		"seed-agent", "seed agent", "schema contract", "bare auth",
	)
}

func historyLineHasCodeFactSignal(line string) bool {
	return historyTextHasCodeFactSignal(line)
}

func classifyHistoryFragment(fragment historyFragment) []string {
	lower := strings.ToLower(fragment.Text)
	narrative := isHistoryNarrativeSource(fragment.Source)
	tool := strings.HasPrefix(fragment.Source, "tool_call")
	validationTool := tool && !strings.Contains(fragment.Source, "apply_patch")
	kinds := map[string]struct{}{}
	if fragment.Source == "user_prompt" {
		// Request records: excluded from general ranking (measured noise);
		// consumed by trajectory surfaces (handoff, midtask eval mining).
		return []string{"request"}
	}
	if fragment.Source == "tool_result_fact" {
		kinds["code_fact"] = struct{}{}
	}
	if narrative && isDecisionFragment(fragment.Source, lower) {
		kinds["decision"] = struct{}{}
	}
	if narrative && containsAny(lower, "learned", "root cause", "turns out", "lesson", "failure mode", "the issue", "the bug", "found that") {
		kinds["learning"] = struct{}{}
	}
	if (narrative || validationTool) && containsAny(lower, "go test", "pytest", "npm test", "mise run", "validation", "regression test", "hidden validation", "tests pass", "validated with", "ran tests") {
		kinds["validation"] = struct{}{}
	}
	if narrative && isArchitectureFragment(lower) {
		kinds["architecture"] = struct{}{}
	}
	if tool || (fragment.Source == "text" && containsAny(lower, "tool_use", "function_call", "apply_patch", "exec_command", "\"cmd\"", "entire brain", "git grep", "bash ", "read ", "write ")) {
		kinds["tool_call"] = struct{}{}
	}
	out := make([]string, 0, len(kinds))
	for kind := range kinds {
		out = append(out, kind)
	}
	sort.Strings(out)
	return out
}

func isHistoryNarrativeSource(source string) bool {
	switch source {
	case "assistant_message", "final_answer", "message", "text":
		return true
	default:
		return false
	}
}

func historyToolCallSource(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return "tool_call"
	}
	return "tool_call:" + name
}

func historyLineMayContainIndexedContent(text string) bool {
	lower := strings.ToLower(text)
	if containsAny(lower, `"type":"session_meta"`, `"type": "session_meta"`, `"type":"turn_context"`, `"type": "turn_context"`, `"type":"permission-mode"`, `"type": "permission-mode"`) {
		return false
	}
	return containsAny(lower,
		"decision", "decided", "we chose", "we choose", "instead of", "rationale",
		"learned", "root cause", "turns out", "lesson", "failure mode", "found that",
		"go test", "pytest", "npm test", "mise run", "validation", "regression test",
		"tests pass", "validated with", "ran tests",
		"architecture", "boundary", "boundaries", "data flow", "contract", "invariant",
		"module", "package", "command surface", "dispatch", "integration", "workflow",
		"tool_use", "function_call", "custom_tool_call", "apply_patch", "exec_command", `"cmd"`,
		"must ", "must not", "should ", "should not", "keep ", "preserve ", "restore ",
		"compatibility", "because", "fix ", "fixed ", "implemented ", "updated ",
		"changed ", "added ", "removed ", "avoid ", "fallback", "source of truth",
		"attributionbasecommit", "realignattributionbase", "human_added", "human added",
		"resolvetranscriptpath", "transcriptpath", "state.transcriptpath", "reresolvestonestedlayout",
		".github", "brainignore", ".brainignore", "github workflow", "seed-agent", "schema contract",
	)
}

func isDecisionFragment(source, lower string) bool {
	if isDecisionProgressFragment(lower) {
		return false
	}
	if containsAny(lower,
		"decision:", "decision -", "decision was", "decision is", "decided",
		"we chose", "we choose", "chose to", "choose to", "rationale",
		"tradeoff", "trade-off", "preferred", "prefer ",
	) {
		return true
	}
	if source != "assistant_message" && source != "final_answer" {
		return false
	}
	return containsAny(lower,
		"must preserve", "must keep", "must not", "should preserve", "should keep",
		"contract", "invariant", "source of truth", "compatibility contract",
		"stable contract", "api contract", "behavioral contract",
	)
}

func isDecisionProgressFragment(lower string) bool {
	return hasAnyPrefix(lower,
		"i’m reading", "i'm reading",
		"i’m checking", "i'm checking",
		"i’m adding", "i'm adding",
		"i’ll treat", "i'll treat",
		"i’ll make", "i'll make",
		"i have enough context",
		"let me ",
		"here is the final plan",
		"server is up",
		"net ",
	)
}

func isArchitectureFragment(lower string) bool {
	return containsAny(lower,
		"architecture", "boundary", "boundaries", "data flow", "contract", "invariant",
		"module", "package", "command surface", "dispatch", "integration", "workflow",
	)
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func hasAnyPrefix(value string, prefixes ...string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

func historyRecordID(path string, line int, kind, text string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s\x00%s", path, line, kind, text)))
	return "history:" + hex.EncodeToString(sum[:12])
}

func cleanHistorySummary(text string) string {
	text = strings.ReplaceAll(text, "\\n", " ")
	text = strings.Join(strings.Fields(text), " ")
	return text
}

func historyTerms(text string) []string {
	lower := strings.ToLower(text)
	candidates := []string{
		"decision", "rationale", "validation", "go test", "apply_patch", "exec_command",
		"semantic", "brief", "stale", "checkpoint", "transcript", "schema", "env",
		"provenance", "bundle", "sha256", "review", "hook", "plugin", "workspace",
		"architecture", "contract", "invariant", "manual commit", "manual_commit",
		"attribution", "basecommit", "attributionbasecommit", "realignattributionbase",
		"human_added", "transcriptpath", "resolvetranscriptpath", "reresolvestonestedlayout",
		"github workflow", ".github", "brainignore", "tool", "test", "fallback",
	}
	var terms []string
	for _, candidate := range candidates {
		if strings.Contains(lower, candidate) || strings.Contains(normalizeHistorySearchText(lower), normalizeHistorySearchText(candidate)) {
			terms = append(terms, candidate)
		}
	}
	return terms
}

func jsonMap(value any) map[string]any {
	m, _ := value.(map[string]any)
	return m
}

func jsonString(value any) string {
	s, _ := value.(string)
	return s
}

func stringFragment(value any, source string) []historyFragment {
	text := jsonString(value)
	if text == "" {
		return nil
	}
	return []historyFragment{{Text: text, Source: source}}
}

func firstNonEmptyString(values ...any) string {
	for _, value := range values {
		if text := jsonString(value); strings.TrimSpace(text) != "" {
			return text
		}
	}
	return ""
}

func compactJSONValue(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(data)
	}
}

func normalizeHistorySearchText(value string) string {
	var b strings.Builder
	runes := []rune(value)
	lastSpace := true
	for i, r := range runes {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if !lastSpace && historyCamelBoundary(runes, i) {
				b.WriteByte(' ')
			}
			b.WriteRune(unicode.ToLower(r))
			lastSpace = false
			continue
		}
		if !lastSpace {
			b.WriteByte(' ')
			lastSpace = true
		}
	}
	return strings.TrimSpace(b.String())
}

func historyCamelBoundary(runes []rune, index int) bool {
	if index <= 0 || index >= len(runes) {
		return false
	}
	current := runes[index]
	previous := runes[index-1]
	if !unicode.IsLetter(current) || !(unicode.IsLetter(previous) || unicode.IsDigit(previous)) || !unicode.IsUpper(current) {
		return false
	}
	if unicode.IsLower(previous) || unicode.IsDigit(previous) {
		return true
	}
	if unicode.IsUpper(previous) && index+1 < len(runes) && unicode.IsLower(runes[index+1]) {
		return true
	}
	return false
}

func historyTextMatchesQuery(text, query string) bool {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return false
	}
	text = strings.ToLower(text)
	if strings.Contains(text, query) {
		return true
	}
	normalizedText := normalizeHistorySearchText(text)
	normalizedQuery := normalizeHistorySearchText(query)
	if normalizedQuery == "" {
		return false
	}
	if strings.Contains(normalizedText, normalizedQuery) {
		return true
	}
	return false
}

func historyRecordMatchesQuery(record historyRecord, query string) bool {
	return historyTextMatchesQuery(record.Summary+" "+strings.Join(record.Terms, " ")+" "+record.Path, query)
}

// historyRecordTimestamp recovers the session capture time encoded in the
// exported transcript path (sessions/<branch>/YYYYMMDDThhmmssZ_...jsonl) so
// callers can order records by recency and report when a record was produced.
// Records sourced from non-session paths (seed/manifest) have no timestamp.
func historyRecordTimestamp(path string) (time.Time, bool) {
	base := filepath.Base(filepath.FromSlash(path))
	if len(base) < 16 {
		return time.Time{}, false
	}
	stamp := base[:16]
	parsed, err := time.Parse("20060102T150405Z", stamp)
	if err != nil {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

// historyRecordMatchedTerms reports which query tokens and identifiers actually
// appear in a record. Returning these makes a match explainable to an agent and
// turns a zero/low-signal result into an actionable "these terms hit, these did
// not" diagnostic instead of an opaque miss.
func historyRecordMatchedTerms(record historyRecord, query string) []string {
	recordText := normalizeHistorySearchText(record.Summary + " " + strings.Join(record.Terms, " ") + " " + record.Path)
	recordRaw := strings.ToUpper(record.Summary + " " + strings.Join(record.Terms, " ") + " " + record.Path)
	seen := map[string]struct{}{}
	var matched []string
	for _, term := range historyQueryTerms(query) {
		if strings.Contains(recordText, term) {
			if _, ok := seen[term]; !ok {
				seen[term] = struct{}{}
				matched = append(matched, term)
			}
		}
	}
	for _, identifier := range historyIdentifierQueryTerms(query) {
		if strings.Contains(recordRaw, identifier) {
			key := strings.ToLower(identifier)
			if _, ok := seen[key]; !ok {
				seen[key] = struct{}{}
				matched = append(matched, identifier)
			}
		}
	}
	return matched
}

func rankHistoryRecords(index historyIndex, kind, query string, limit int) []historyRecord {
	scored := rankHistoryRecordsScored(index, kind, query, limit, 0)
	records := make([]historyRecord, 0, len(scored))
	for _, item := range scored {
		records = append(records, item.Record)
	}
	return records
}

// rankHistoryRecordsScored ranks records and returns the scores so callers can
// surface why a record matched. minMatches relaxes the term-coverage threshold
// (0 = strict default, 1 = best-effort partial matching).
func rankHistoryRecordsScored(index historyIndex, kind, query string, limit, minMatches int) []scoredHistoryRecord {
	if limit <= 0 {
		return nil
	}
	allowed := historyInspectKinds(kind)
	scored := make([]scoredHistoryRecord, 0, min(len(index.Records), limit))
	seen := map[string]struct{}{}
	for i, record := range index.Records {
		if len(allowed) > 0 {
			if _, ok := allowed[record.Kind]; !ok {
				continue
			}
		} else if historyGeneralRankingHiddenKind(record.Kind) {
			// Keep user-prompt and conversation-exchange records out of general
			// ranking (requests are measured noise; exchanges are opt-in via the
			// explicit conversation source only).
			continue
		}
		score := historyRecordQueryScoreMin(record, query, minMatches)
		if score == 0 {
			continue
		}
		score += historyInspectKindPreference(kind, record.Kind)
		matchKey := normalizeHistorySearchText(record.Summary)
		if _, ok := seen[matchKey]; ok {
			continue
		}
		seen[matchKey] = struct{}{}
		scored = append(scored, scoredHistoryRecord{Record: record, Score: score, Order: i})
	}
	sort.Slice(scored, func(i, j int) bool {
		left, right := scored[i], scored[j]
		if left.Score != right.Score {
			return left.Score > right.Score
		}
		// Among equally-scored records, prefer the most recent session so
		// "what is the current state of X" surfaces the latest decision first
		// instead of an arbitrary older one.
		leftTime, leftOK := historyRecordTimestamp(left.Record.Path)
		rightTime, rightOK := historyRecordTimestamp(right.Record.Path)
		if leftOK && rightOK && !leftTime.Equal(rightTime) {
			return leftTime.After(rightTime)
		}
		if leftOK != rightOK {
			return leftOK
		}
		if historyKindRank(left.Record.Kind) != historyKindRank(right.Record.Kind) {
			return historyKindRank(left.Record.Kind) > historyKindRank(right.Record.Kind)
		}
		if left.Record.Path != right.Record.Path {
			return left.Record.Path > right.Record.Path
		}
		if left.Record.Line != right.Record.Line {
			return left.Record.Line < right.Record.Line
		}
		return left.Order < right.Order
	})
	if len(scored) > limit {
		scored = scored[:limit]
	}
	return scored
}

func historyRecordQueryScore(record historyRecord, query string) int {
	return historyRecordQueryScoreMin(record, query, 0)
}

// historyRecordQueryScoreMin scores a record against a query. minMatches
// overrides the default term-coverage threshold: pass 0 for the strict default
// (precise specialist lookups) or 1 to accept best-effort partial matches for
// natural-language questions that would otherwise return nothing.
func historyRecordQueryScoreMin(record historyRecord, query string, minMatches int) int {
	score := 0
	if historyRecordMatchesQuery(record, query) {
		score += 200
	}
	terms := historyQueryTerms(query)
	identifiers := historyIdentifierQueryTerms(query)
	if len(terms) == 0 && len(identifiers) == 0 {
		return score
	}
	recordText := normalizeHistorySearchText(record.Summary + " " + strings.Join(record.Terms, " ") + " " + record.Path)
	recordRaw := strings.ToUpper(record.Summary + " " + strings.Join(record.Terms, " ") + " " + record.Path)
	recordTerms := normalizeHistorySearchText(strings.Join(record.Terms, " "))
	matches := 0
	for _, term := range terms {
		if strings.Contains(recordText, term) {
			matches++
			score += 10
			if strings.Contains(recordTerms, term) {
				score += 5
			}
		}
	}
	identifierMatches := 0
	for _, identifier := range identifiers {
		if strings.Contains(recordRaw, identifier) {
			identifierMatches++
			score += 90
		}
	}
	effectiveMatches := matches + identifierMatches
	requiredTermCount := len(terms)
	if requiredTermCount == 0 {
		requiredTermCount = len(identifiers)
	}
	required := historyRequiredQueryMatches(requiredTermCount)
	if minMatches > 0 && minMatches < required {
		required = minMatches
	}
	if effectiveMatches < required {
		if score < 200 {
			return 0
		}
		return score + historyKindRank(record.Kind)
	}
	score += effectiveMatches * 2
	if len(terms) > 0 && matches >= len(terms) {
		score += 30
	}
	score += historyRecordEvidenceScore(record, matches, identifierMatches)
	score += historyKindRank(record.Kind)
	return score
}

func historyRecordEvidenceScore(record historyRecord, termMatches, identifierMatches int) int {
	text := record.Summary + " " + strings.Join(record.Terms, " ") + " " + record.Path
	lower := strings.ToLower(text)
	score := 0
	pathLower := strings.ToLower(record.Path)
	if pathLower == "manifest.json" || strings.HasPrefix(pathLower, "seed/") {
		score -= 200
	}
	if record.Kind == "code_fact" {
		score += 35
	}
	if containsAny(lower, "diff --git", "+++ b/", "--- a/", "@@", "apply_patch", "unified_diff") {
		score += 40
	}
	if containsAny(lower, "packages/", "internal/", "cmd/", "src/", "apps/", ".go", ".ts", ".tsx", ".js", ".py", ".rs") {
		score += 20
	}
	if containsAny(lower, "metadata.step", "invalid_type", "metadata values must be strings", "metadata value") {
		score += 80
	}
	if containsAny(lower, "stopped chaining", "self-contained perception", "self-contained browser", "previous_response_id") &&
		containsAny(lower, "agentic", "browser", "chrome") {
		score += 90
	}
	if containsAny(lower, "openai docs", "responses api supports", "same-thread continuity", "conversations api") &&
		!containsAny(lower, "agentic", "browser", "chrome") {
		score -= 60
	}
	if identifierMatches > 0 {
		score += 20 * identifierMatches
		if containsAny(lower, "diff --git", "+++ b/", "apply_patch", "unified_diff") {
			score += 25
		}
	}
	if termMatches > 1 && containsAny(lower, "test", "validation", "regression", "contract", "invariant") {
		score += 12
	}
	if containsAny(lower, "start by running:", "entire doctor", "entire status --detailed", "entire session current", "entire checkpoint list") {
		score -= 120
	}
	if record.Kind == "tool_call" && !containsAny(lower, "apply_patch", "diff --git", "unified_diff") {
		score -= 20
	}
	if len(record.Summary) > 2500 && !containsAny(lower, "diff --git", "+++ b/", "apply_patch", "unified_diff") {
		score -= 25
	}
	return score
}

func historyInspectKindPreference(queryKind, recordKind string) int {
	switch queryKind {
	case "architecture":
		switch recordKind {
		case "architecture":
			return 120
		case "decision":
			return 80
		case "learning":
			return 40
		default:
			return 0
		}
	default:
		return 0
	}
}

func historyRequiredQueryMatches(termCount int) int {
	switch {
	case termCount <= 1:
		return 1
	case termCount <= 3:
		return 2
	default:
		return 3
	}
}

// historyGeneralRankingHiddenKind reports the record kinds every general
// (non-kind-scoped) history surface must skip: user-prompt requests (measured
// ranking noise) and conversation exchanges (returned only when the caller
// explicitly selects the conversation source; excluded from history vectors in
// Phase 1 — no semantic exchange arm exists yet).
func historyGeneralRankingHiddenKind(kind string) bool {
	return kind == "request" || kind == conversationKind
}

func historyKindRank(kind string) int {
	switch kind {
	case "decision":
		return 40
	case "request":
		return 34
	case "architecture":
		return 32
	case "code_fact":
		return 30
	case "learning":
		return 26
	case "validation":
		return 18
	case "tool_call":
		return 4
	default:
		return 1
	}
}

func historyQueryTerms(query string) []string {
	normalized := normalizeHistorySearchText(query)
	seen := map[string]struct{}{}
	var terms []string
	for _, term := range strings.Fields(normalized) {
		if historyQueryStopword(term) {
			continue
		}
		if len(term) < 3 && !historyShortQueryTerm(term) {
			continue
		}
		if _, ok := seen[term]; ok {
			continue
		}
		seen[term] = struct{}{}
		terms = append(terms, term)
	}
	return terms
}

// historyQueryAllTerms returns the union of word tokens and identifier tokens a
// query reduces to after stopword removal. Surfacing these lets an agent see
// exactly what the brain searched for, so a thin or empty result is debuggable
// (e.g. an over-specific phrase reduced to one weak term) instead of opaque.
func historyQueryAllTerms(query string) []string {
	seen := map[string]struct{}{}
	var all []string
	for _, term := range historyQueryTerms(query) {
		if _, ok := seen[term]; !ok {
			seen[term] = struct{}{}
			all = append(all, term)
		}
	}
	for _, identifier := range historyIdentifierQueryTerms(query) {
		key := strings.ToLower(identifier)
		if _, ok := seen[key]; !ok {
			seen[key] = struct{}{}
			all = append(all, identifier)
		}
	}
	return all
}

func historyIdentifierQueryTerms(query string) []string {
	seen := map[string]struct{}{}
	var terms []string
	for _, candidate := range strings.FieldsFunc(query, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_')
	}) {
		candidate = strings.Trim(candidate, "_")
		if candidate == "" {
			continue
		}
		upper := strings.ToUpper(candidate)
		if !strings.Contains(upper, "_") && !historyIdentifierLike(candidate) {
			continue
		}
		if historyQueryStopword(strings.ToLower(upper)) {
			continue
		}
		if _, ok := seen[upper]; ok {
			continue
		}
		seen[upper] = struct{}{}
		terms = append(terms, upper)
	}
	return terms
}

func historyIdentifierLike(candidate string) bool {
	if len(candidate) < 3 {
		return false
	}
	hasUpper := false
	hasLower := false
	hasDigit := false
	hasInternalUpper := false
	for i, r := range candidate {
		switch {
		case unicode.IsUpper(r):
			hasUpper = true
			if i > 0 {
				hasInternalUpper = true
			}
		case unicode.IsLower(r):
			hasLower = true
		case unicode.IsDigit(r):
			hasDigit = true
		}
	}
	if hasUpper && !hasLower {
		return true
	}
	if hasInternalUpper && hasLower {
		return true
	}
	return hasDigit && (hasUpper || hasLower)
}

func historyShortQueryTerm(term string) bool {
	switch term {
	case "go", "ci", "pr", "ui", "id", "lc":
		return true
	default:
		return false
	}
}

// genericQueryStopwords are generic English fillers and generic task verbs with
// no retrieval signal. It is the SINGLE source of truth shared by every query
// tokenizer in the package — history/semantic specialist search
// (historyQueryStopword) and brain-brief filename matching
// (brainBriefFileMatchTerms) — so the common list never drifts between the two.
// Each call site layers only its own domain-specific noise words on top; those
// legitimately differ (e.g. "regression"/"preserve" are noise for free-text
// history search but valid filename-match terms like regression.go for brief).
var genericQueryStopwords = map[string]bool{
	"and": true, "are": true, "before": true, "but": true, "can": true,
	"did": true, "does": true, "fix": true, "for": true, "from": true,
	"has": true, "have": true, "how": true, "into": true, "its": true,
	"make": true, "must": true, "need": true, "new": true, "not": true,
	"run": true, "runs": true, "should": true, "than": true, "that": true,
	"the": true, "then": true, "this": true, "use": true, "via": true,
	"want": true, "what": true, "when": true, "where": true, "which": true,
	"why": true, "with": true, "would": true,
}

func historyQueryStopword(term string) bool {
	if genericQueryStopwords[term] {
		return true
	}
	// History/semantic-search-specific noise words layered on the generic set:
	// natural-language filler plus domain verbs/nouns ("regression", "preserve",
	// "restore", ...) that carry no signal for a free-text record search.
	switch term {
	case "a", "an", "as", "at", "be", "been", "by", "current", "do",
		"during", "existing", "in", "is", "it", "keep", "local", "of", "on",
		"only", "or", "other", "previous", "prior", "preserve", "regression",
		"restore", "same", "task", "to", "without":
		return true
	case "behavior", "entire", "fresh", "parent", "setting", "value", "values":
		return true
	case "done", "over", "instead", "we", "our", "could", "about":
		return true
	default:
		return false
	}
}

func loadBrainHistoryIndex(brainDir string, source *historySourceManifest) (historyIndex, error) {
	if source == nil || source.IndexPath == "" {
		return historyIndex{}, errors.New("history index missing; run `entire brain refresh`")
	}
	clean, err := validateHistoryIndexPath(source.IndexPath)
	if err != nil {
		return historyIndex{}, err
	}
	if err := rejectSymlinkPathComponents(brainDir, clean); err != nil {
		return historyIndex{}, err
	}
	data, err := os.ReadFile(filepath.Join(brainDir, clean))
	if err != nil {
		return historyIndex{}, err
	}
	var index historyIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return historyIndex{}, err
	}
	return index, nil
}

func validateHistoryIndexPath(indexPath string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(indexPath))
	cleanSlash := filepath.ToSlash(clean)
	if cleanSlash != indexPath {
		return "", fmt.Errorf("history index_path must be canonical: %s", indexPath)
	}
	if strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(clean) || cleanSlash != historyIndexPath {
		return "", fmt.Errorf("history index_path is unsafe: %s", indexPath)
	}
	return clean, nil
}
