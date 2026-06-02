package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	historyDirName        = "history"
	historyIndexFileName  = "index.json"
	historyIndexPath      = historyDirName + "/" + historyIndexFileName
	historyMaxLineBytes   = 1024 * 1024
	historyMaxRecords     = 80000
	historyMaxKindRecords = 20000
	historyMaxScanFiles   = 500
	historyMaxScanBytes   = int64(512 * 1024 * 1024)
)

type historySourceManifest struct {
	GeneratedAt time.Time `json:"generated_at"`
	IndexPath   string    `json:"index_path"`
	Records     int       `json:"records"`
	Decisions   int       `json:"decisions"`
	Learnings   int       `json:"learnings"`
	Validations int       `json:"validations"`
	ToolCalls   int       `json:"tool_calls"`
	Warnings    []string  `json:"warnings,omitempty"`
}

type historyIndex struct {
	GeneratedAt time.Time       `json:"generated_at"`
	Records     []historyRecord `json:"records"`
	Warnings    []string        `json:"warnings,omitempty"`
}

type historyRecord struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`
	Path    string   `json:"path"`
	Line    int      `json:"line"`
	Summary string   `json:"summary"`
	Terms   []string `json:"terms,omitempty"`
}

type historyFragment struct {
	Text   string
	Source string
}

type historySessionFile struct {
	Path     string
	SortTime time.Time
	Size     int64
}

func newHistoryIndexCommand(opts Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "history-index [path]",
		Short: "Build the local session history decision/rationale index",
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
		return fmt.Errorf("history-index requires a local repository path: %s", target)
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}
	source, err := writeBrainHistoryIndexAndSource(storage.BrainDir, opts.Now().UTC())
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "indexed %d history records: %s\n", source.Records, filepath.Join(storage.BrainDir, filepath.FromSlash(source.IndexPath)))
	return nil
}

func writeBrainHistoryIndexAndSource(outputDir string, now time.Time) (*historySourceManifest, error) {
	index, source, err := buildBrainHistoryIndex(outputDir, now)
	if err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	if err := writeFileAtomic(filepath.Join(outputDir, filepath.FromSlash(historyIndexPath)), data, 0o600); err != nil {
		return nil, fmt.Errorf("write history index: %w", err)
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

func buildBrainHistoryIndex(outputDir string, now time.Time) (historyIndex, *historySourceManifest, error) {
	sessionsRoot := filepath.Join(outputDir, exportSessionsDirectory)
	index := historyIndex{GeneratedAt: now}
	if _, err := os.Stat(sessionsRoot); err != nil {
		if os.IsNotExist(err) {
			return index, nil, errors.New("session history missing; run `entire brain export` first")
		}
		return index, nil, err
	}
	files, err := collectHistorySessionFiles(sessionsRoot, &index.Warnings)
	if err != nil {
		return index, nil, err
	}
	kindCounts := map[string]int{}
	kindTruncated := map[string]bool{}
	seenDecisions := map[string]struct{}{}
	totalTruncated := false
	scannedFiles := 0
	var scannedBytes int64
	scanBudgetWarned := false
	for _, file := range files {
		if len(index.Records) >= historyMaxRecords {
			if !totalTruncated {
				index.Warnings = append(index.Warnings, "history index truncated at record limit")
				totalTruncated = true
			}
			break
		}
		if scannedFiles > 0 && (scannedFiles >= historyMaxScanFiles || scannedBytes+file.Size > historyMaxScanBytes) {
			if !scanBudgetWarned {
				index.Warnings = append(index.Warnings, fmt.Sprintf("history index scanned newest %d session files (%d bytes) and skipped older sessions at scan budget", scannedFiles, scannedBytes))
				scanBudgetWarned = true
			}
			break
		}
		scannedFiles++
		scannedBytes += file.Size
		records, scanErr := scanHistoryFile(outputDir, file.Path)
		if scanErr != nil {
			index.Warnings = append(index.Warnings, scanErr.Error())
			continue
		}
		for _, record := range records {
			if record.Kind == "decision" {
				dedupeKey := normalizeHistorySearchText(record.Summary)
				if _, ok := seenDecisions[dedupeKey]; ok {
					continue
				}
				seenDecisions[dedupeKey] = struct{}{}
			}
			if kindCounts[record.Kind] >= historyMaxKindRecords {
				if !kindTruncated[record.Kind] {
					index.Warnings = append(index.Warnings, fmt.Sprintf("history index truncated %s records at kind limit", record.Kind))
					kindTruncated[record.Kind] = true
				}
				continue
			}
			if len(index.Records) >= historyMaxRecords {
				if !totalTruncated {
					index.Warnings = append(index.Warnings, "history index truncated at record limit")
					totalTruncated = true
				}
				continue
			}
			index.Records = append(index.Records, record)
			kindCounts[record.Kind]++
		}
	}
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
		GeneratedAt: now,
		IndexPath:   historyIndexPath,
		Records:     len(index.Records),
		Warnings:    append([]string(nil), index.Warnings...),
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
		}
	}
	return index, source, nil
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
			Path:     path,
			SortTime: historySessionSortTime(path, info.ModTime()),
			Size:     info.Size(),
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
		if !historyLineMayContainIndexedContent(text) {
			continue
		}
		for _, fragment := range extractHistoryFragments(text, allowRawText) {
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

func extractHistoryFragments(line string, allowRawText bool) []historyFragment {
	var obj map[string]any
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		if !allowRawText {
			return nil
		}
		return []historyFragment{{Text: line, Source: "text"}}
	}
	return extractHistoryJSONFragments(obj)
}

func extractHistoryJSONFragments(obj map[string]any) []historyFragment {
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
		return nil
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
		return nil
	default:
		return nil
	}
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
				fragments = append(fragments, historyFragment{Text: strings.TrimSpace(name + " " + compactJSONValue(block["input"])), Source: historyToolCallSource(name)})
			case "tool_result":
				continue
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

func classifyHistoryFragment(fragment historyFragment) []string {
	lower := strings.ToLower(fragment.Text)
	narrative := isHistoryNarrativeSource(fragment.Source)
	tool := strings.HasPrefix(fragment.Source, "tool_call")
	validationTool := tool && !strings.Contains(fragment.Source, "apply_patch")
	kinds := map[string]struct{}{}
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
		"attribution", "basecommit", "tool", "test", "fallback",
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
	value = strings.ToLower(value)
	var b strings.Builder
	lastSpace := true
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
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

func loadBrainHistoryIndex(brainDir string, source *historySourceManifest) (historyIndex, error) {
	if source == nil || source.IndexPath == "" {
		return historyIndex{}, errors.New("history index missing; run `entire brain history-index` or `entire brain refresh --history-index`")
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
