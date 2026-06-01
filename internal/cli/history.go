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

	"github.com/spf13/cobra"
)

const (
	historyDirName       = "history"
	historyIndexFileName = "index.json"
	historyIndexPath     = historyDirName + "/" + historyIndexFileName
	historyMaxLineBytes  = 1024 * 1024
	historyMaxRecords    = 2000
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
	err := filepath.WalkDir(sessionsRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			index.Warnings = append(index.Warnings, err.Error())
			return nil
		}
		if d.IsDir() {
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".jsonl" && ext != ".json" && ext != ".md" && ext != ".txt" {
			return nil
		}
		records, scanErr := scanHistoryFile(outputDir, path)
		if scanErr != nil {
			index.Warnings = append(index.Warnings, scanErr.Error())
			return nil
		}
		index.Records = append(index.Records, records...)
		if len(index.Records) >= historyMaxRecords {
			index.Records = index.Records[:historyMaxRecords]
			index.Warnings = append(index.Warnings, "history index truncated at record limit")
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return index, nil, err
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
	for scanner.Scan() {
		lineNumber++
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		for _, kind := range classifyHistoryLine(text) {
			records = append(records, historyRecord{
				ID:      historyRecordID(rel, lineNumber, kind, text),
				Kind:    kind,
				Path:    rel,
				Line:    lineNumber,
				Summary: truncateString(cleanHistorySummary(text), 700),
				Terms:   historyTerms(text),
			})
		}
	}
	return records, scanner.Err()
}

func classifyHistoryLine(text string) []string {
	lower := strings.ToLower(text)
	kinds := map[string]struct{}{}
	if containsAny(lower, "decision", "decided", "we chose", "we choose", "instead of", "keep prompt plus local validation") {
		kinds["decision"] = struct{}{}
	}
	if containsAny(lower, "learned", "root cause", "turns out", "lesson", "failure mode") {
		kinds["learning"] = struct{}{}
	}
	if containsAny(lower, "go test", "pytest", "npm test", "validation", "regression test", "hidden validation") {
		kinds["validation"] = struct{}{}
	}
	if containsAny(lower, "tool_use", "function_call", "apply_patch", "exec_command", "\"cmd\"", "entire brain", "git grep") {
		kinds["tool_call"] = struct{}{}
	}
	out := make([]string, 0, len(kinds))
	for kind := range kinds {
		out = append(out, kind)
	}
	sort.Strings(out)
	return out
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
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
	}
	var terms []string
	for _, candidate := range candidates {
		if strings.Contains(lower, candidate) {
			terms = append(terms, candidate)
		}
	}
	return terms
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
