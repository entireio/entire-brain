package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	brainBriefPostIndexTermLimit   = 6
	brainBriefPostIndexPathLimit   = 64
	brainBriefPostIndexOutputLimit = 64 * 1024
	brainBriefPostIndexTimeout     = 500 * time.Millisecond
)

var (
	brainBriefFullCommitPattern     = regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)
	brainBriefLocatorTokenPattern   = regexp.MustCompile(`(?i)[\p{L}\p{N}]{3,}|v[0-9]{1,4}`)
	brainBriefVersionLocatorPattern = regexp.MustCompile(`^v[0-9]{1,4}$`)
	errBrainBriefPostIndexTruncated = errors.New("truncated NUL-delimited git output")
)

type brainBriefPostIndexCandidate struct {
	path      string
	score     int
	benchmark bool
}

// brainBriefPromotePostIndexFiles bridges one narrow stale-index blind spot:
// files committed after the semantic snapshot cannot appear in its symbol
// results, and a clean worktree has no live changed-file overlay. The bridge is
// intentionally brief-only and task-filtered. It never broadens Live.ChangedFiles
// (which is also the deletion/restoration trust boundary).
func brainBriefPromotePostIndexFiles(
	ctx context.Context,
	runner CommandRunner,
	status brainStatusReport,
	task string,
	report *brainBriefReport,
) {
	if report == nil {
		return
	}
	editFiles, testFiles := brainBriefPostIndexFiles(ctx, runner, status, task)
	if len(editFiles) == 0 && len(testFiles) == 0 {
		return
	}
	report.LikelyEditFiles = brainBriefMergePrioritizedFiles(editFiles, report.LikelyEditFiles, 8)
	report.LikelyTestFiles = brainBriefMergePrioritizedFiles(testFiles, report.LikelyTestFiles, 6)
	report.LikelyFiles = brainBriefMergeLikelyFiles(report.LikelyEditFiles, report.LikelyTestFiles)
}

func brainBriefPostIndexFiles(ctx context.Context, runner CommandRunner, status brainStatusReport, task string) ([]string, []string) {
	if runner == nil {
		return nil, nil
	}
	repoRoot, indexed, current, ok := brainBriefPostIndexRange(status)
	if !ok {
		return nil, nil
	}
	terms := brainBriefPostIndexLocatorTerms(task)
	if len(terms) < 2 {
		return nil, nil
	}

	boundedCtx, cancel := context.WithTimeout(ctx, brainBriefPostIndexTimeout)
	defer cancel()
	if _, _, err := runner.Run(boundedCtx, repoRoot, "git", "merge-base", "--is-ancestor", indexed, current); err != nil {
		return nil, nil
	}
	paths, ok := brainBriefPostIndexDiffPaths(boundedCtx, cancel, runner, repoRoot, indexed, current, terms)
	if !ok {
		return nil, nil
	}

	repoFiles, ok := newBrainBriefRepoFileChecker(repoRoot)
	if !ok {
		return nil, nil
	}
	performanceIntent := brainBriefPerformanceIntent(task)
	candidates := make([]brainBriefPostIndexCandidate, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		clean, valid := cleanBrainBriefLikelyFile(path)
		if !valid || !repoFiles.exists(clean) {
			continue
		}
		if _, duplicate := seen[clean]; duplicate {
			continue
		}
		seen[clean] = struct{}{}
		hits := brainBriefPostIndexTermHits(clean, terms)
		benchmark := brainBriefBenchmarkPath(clean)
		score := hits
		if benchmark && !performanceIntent {
			score--
		}
		if score < 2 {
			continue
		}
		candidates = append(candidates, brainBriefPostIndexCandidate{
			path:      clean,
			score:     score,
			benchmark: benchmark,
		})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		if candidates[i].benchmark != candidates[j].benchmark {
			if performanceIntent {
				return candidates[i].benchmark
			}
			return !candidates[i].benchmark
		}
		return candidates[i].path < candidates[j].path
	})

	var editFiles, testFiles []string
	for _, candidate := range candidates {
		if brainBriefLikelyTestFile(candidate.path) {
			if len(testFiles) == 0 {
				testFiles = append(testFiles, candidate.path)
			}
			continue
		}
		if len(editFiles) == 0 {
			editFiles = append(editFiles, candidate.path)
		}
		if len(editFiles) == 1 && len(testFiles) == 1 {
			break
		}
	}
	return editFiles, testFiles
}

func brainBriefPostIndexRange(status brainStatusReport) (repoRoot, indexed, current string, ok bool) {
	if status.Semantic == nil || status.Semantic.Freshness == nil || status.Manifest == nil ||
		status.Manifest.Sources == nil || status.Manifest.Sources.Semantic == nil {
		return "", "", "", false
	}
	head, exists := status.Semantic.Freshness.Axes["head"]
	if !exists || head.State != "stale" {
		return "", "", "", false
	}
	indexed = strings.ToLower(strings.TrimSpace(head.Indexed))
	current = strings.ToLower(strings.TrimSpace(head.Current))
	liveHead := strings.ToLower(strings.TrimSpace(status.Live.Head))
	sourceCommit := strings.ToLower(strings.TrimSpace(status.Manifest.Sources.Semantic.Commit))
	if !brainBriefFullCommitPattern.MatchString(indexed) || !brainBriefFullCommitPattern.MatchString(current) ||
		len(indexed) != len(current) || indexed == current || liveHead != current || sourceCommit != indexed ||
		strings.TrimSpace(status.Repo.Root) == "" {
		return "", "", "", false
	}
	return status.Repo.Root, indexed, current, true
}

func brainBriefPostIndexLocatorTerms(task string) []string {
	type locatorToken struct {
		value      string
		start, end int
		version    bool
	}
	lowerTask := strings.ToLower(task)
	seen := map[string]struct{}{}
	tokens := make([]locatorToken, 0, brainBriefPostIndexTermLimit)
	for _, span := range brainBriefLocatorTokenPattern.FindAllStringIndex(lowerTask, -1) {
		match := lowerTask[span[0]:span[1]]
		if !brainBriefLiteralLocatorTerm(match) || brainBriefFileMatchTermStop(match) {
			continue
		}
		// Pathspec-looking task syntax is never passed through as magic, but it
		// is also poor locator material and must not displace real task terms.
		if brainBriefLocatorSyntaxField(lowerTask, span[0], span[1]) {
			continue
		}
		if _, duplicate := seen[match]; duplicate {
			continue
		}
		seen[match] = struct{}{}
		tokens = append(tokens, locatorToken{
			value:   match,
			start:   span[0],
			end:     span[1],
			version: brainBriefVersionLocatorPattern.MatchString(match),
		})
	}

	compoundNeighbor := map[string]bool{}
	for i, token := range tokens {
		if !token.version {
			continue
		}
		for _, neighborIndex := range []int{i - 1, i + 1} {
			if neighborIndex < 0 || neighborIndex >= len(tokens) || tokens[neighborIndex].version {
				continue
			}
			neighbor := tokens[neighborIndex]
			left, right := neighbor, token
			if left.start > right.start {
				left, right = right, left
			}
			if right.start == left.end+1 && brainBriefVersionCompoundSeparator(lowerTask[left.end]) {
				compoundNeighbor[neighbor.value] = true
			}
		}
	}

	versions := make([]string, 0, 1)
	compound := make([]string, 0, 1)
	ordinary := make([]string, 0, brainBriefPostIndexTermLimit)
	for _, token := range tokens {
		switch {
		case token.version:
			versions = append(versions, token.value)
		case compoundNeighbor[token.value]:
			compound = append(compound, token.value)
		default:
			ordinary = append(ordinary, token.value)
		}
	}
	// Short version locators are rare and precise. Keep them even when a long
	// task supplies more than six prose tokens, and keep their compound neighbor
	// (compact-v3) as one locator unit. When ordinary terms overflow the remaining
	// budget, retain anchors from both ends: task descriptions commonly put an
	// identifier near the start and the concrete failure near the end.
	terms := append([]string(nil), versions...)
	terms = append(terms, compound...)
	if len(terms) >= brainBriefPostIndexTermLimit {
		return terms[:brainBriefPostIndexTermLimit]
	}
	remaining := brainBriefPostIndexTermLimit - len(terms)
	if len(ordinary) <= remaining {
		return append(terms, ordinary...)
	}
	front := remaining / 2
	back := remaining - front
	terms = append(terms, ordinary[:front]...)
	terms = append(terms, ordinary[len(ordinary)-back:]...)
	return terms
}

func brainBriefLocatorSyntaxField(task string, start, end int) bool {
	fieldStart := start
	for fieldStart > 0 && !unicode.IsSpace(rune(task[fieldStart-1])) {
		fieldStart--
	}
	fieldEnd := end
	for fieldEnd < len(task) && !unicode.IsSpace(rune(task[fieldEnd])) {
		fieldEnd++
	}
	field := task[fieldStart:fieldEnd]
	return strings.Contains(field, ":(") || strings.ContainsAny(field, "[]!*?")
}

func brainBriefVersionCompoundSeparator(separator byte) bool {
	return separator == '-' || separator == '_'
}

func brainBriefLiteralLocatorTerm(term string) bool {
	if term == "" || len(term) > 64 || !utf8.ValidString(term) {
		return false
	}
	for _, r := range term {
		if !unicode.IsLetter(r) && !unicode.IsNumber(r) {
			return false
		}
	}
	return true
}

func brainBriefPostIndexDiffPaths(
	ctx context.Context,
	cancel context.CancelFunc,
	runner CommandRunner,
	repoRoot, indexed, current string,
	terms []string,
) ([]string, bool) {
	if len(terms) < 2 || len(terms) > brainBriefPostIndexTermLimit {
		return nil, false
	}
	for _, term := range terms {
		if !brainBriefLiteralLocatorTerm(term) {
			return nil, false
		}
	}
	streamer, ok := runner.(CommandStreamer)
	if !ok {
		return nil, false
	}
	args := []string{
		"-c", "core.quotepath=false", "diff", "--no-ext-diff", "--no-textconv",
		"--name-only", "-z", "--diff-filter=AMR", indexed, current, "--",
	}
	for _, term := range terms {
		// The term contains letters/numbers only. glob magic is fixed here, not
		// accepted from the task, so neither pathspec injection nor exclusions
		// can cross this boundary. **/ also matches files below any repository
		// directory; the top-level form covers root files explicitly.
		args = append(args,
			":(top,glob,icase)*"+term+"*",
			":(top,glob,icase)**/*"+term+"*",
		)
	}
	stream, err := streamer.Stream(ctx, repoRoot, "git", args...)
	if err != nil {
		return nil, false
	}
	defer stream.Close()

	scanner := bufio.NewScanner(stream.Stdout())
	scanner.Split(brainBriefScanNUL)
	scanner.Buffer(make([]byte, 1024), brainBriefPostIndexOutputLimit+1)
	paths := make([]string, 0, min(16, brainBriefPostIndexPathLimit))
	bytesRead := 0
	oversized := false
	for scanner.Scan() {
		value := scanner.Text()
		bytesRead += len(value) + 1
		if bytesRead > brainBriefPostIndexOutputLimit {
			oversized = true
			cancel()
			break
		}
		// OR pathspecs intentionally admit one-term names so two locator terms
		// can be spread across directories and the basename. Do not let that
		// one-term noise consume the candidate-count safety cap; the byte and
		// deadline caps above still bound all raw output.
		if brainBriefPostIndexTermHits(value, terms) < 2 {
			continue
		}
		if len(paths) >= brainBriefPostIndexPathLimit {
			oversized = true
			cancel()
			break
		}
		paths = append(paths, value)
	}
	if oversized {
		_ = stream.Close()
		_, _ = stream.Wait()
		return nil, false
	}
	if scanner.Err() != nil {
		cancel()
		_ = stream.Close()
		_, _ = stream.Wait()
		return nil, false
	}
	if _, err := stream.Wait(); err != nil || ctx.Err() != nil {
		return nil, false
	}
	return paths, true
}

func brainBriefScanNUL(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if i := bytes.IndexByte(data, 0); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF {
		if len(data) == 0 {
			return 0, nil, nil
		}
		// A successful `git diff -z` always terminates every path with NUL.
		// Treat a truncated/malformed stream as an error via Scanner.
		return 0, nil, errBrainBriefPostIndexTruncated
	}
	return 0, nil, nil
}

func brainBriefPostIndexTermHits(path string, terms []string) int {
	normalized := strings.ToLower(filepath.ToSlash(path))
	base := filepath.Base(normalized)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	segments := strings.FieldsFunc(normalized, func(r rune) bool {
		return r == '/' || r == '\\' || r == '.' || r == '-' || r == '_'
	})
	hits := 0
	baseHits := 0
	for _, term := range terms {
		if strings.Contains(base, term) {
			baseHits++
			hits++
			continue
		}
		for _, segment := range segments {
			if strings.Contains(segment, term) {
				hits++
				break
			}
		}
	}
	// A directory-only match is too broad on a long stale range. Require at
	// least one task locator in the basename, then allow a second locator from
	// a module/package directory (for example src/auth/token.go).
	if baseHits == 0 {
		return 0
	}
	return hits
}

func brainBriefBenchmarkPath(path string) bool {
	lower := strings.ToLower(filepath.ToSlash(path))
	base := filepath.Base(lower)
	return strings.Contains(base, "_bench") || strings.Contains(base, "benchmark") ||
		strings.Contains(lower, "/benchmarks/") || strings.HasPrefix(lower, "benchmarks/")
}

func brainBriefPerformanceIntent(task string) bool {
	for _, term := range brainBriefLocatorTokenPattern.FindAllString(strings.ToLower(task), -1) {
		switch {
		case strings.HasPrefix(term, "allocat"), strings.HasPrefix(term, "bench"),
			strings.HasPrefix(term, "optimiz"), term == "faster", term == "latency",
			term == "memory", term == "performance", term == "speed", term == "throughput":
			return true
		}
	}
	return false
}
