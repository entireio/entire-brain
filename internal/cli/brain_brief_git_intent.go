package cli

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	brainBriefGitIntentTermLimit   = 6
	brainBriefGitIntentCommitLimit = 16
	brainBriefGitIntentPathLimit   = 64
	brainBriefGitIntentOutputLimit = 64 * 1024
	brainBriefGitIntentTimeout     = 100 * time.Millisecond
)

type brainBriefGitIntentCommit struct {
	hash    string
	subject string
	score   int
}

type brainBriefGitIntentPath struct {
	path  string
	score int
}

// brainBriefPromoteGitIntentFile closes a narrow operational-code blind spot.
// Semantic providers often expose only a file record for shell/config files (or
// do not parse them at all), while retained sessions need not put their paths in
// the top history window. A strongly matching commit subject is useful local
// provenance for those files, but only after the normal brief has failed to
// produce either a concrete action or a strong operational path candidate.
//
// The overlay is deliberately read-only and fail-closed: it searches current
// ancestry only, accepts one current regular operational/config file, and never
// treats task text as a revision, pathspec, or regular expression.
func brainBriefPromoteGitIntentFile(
	ctx context.Context,
	runner CommandRunner,
	repoRoot, task string,
	report *brainBriefReport,
) bool {
	if report == nil || runner == nil || len(report.ActionChecklist) != 0 {
		return false
	}
	terms := brainBriefGitIntentTerms(task)
	if len(terms) < 2 || !brainBriefGitIntentOperationalTask(task, terms) ||
		brainBriefGitIntentHasStrongCandidate(*report, terms) {
		return false
	}
	path, ok := brainBriefGitIntentFile(ctx, runner, repoRoot, task, terms)
	if !ok || path == "" {
		return false
	}
	report.LikelyEditFiles = brainBriefGitIntentMergeEdit(path, report.LikelyEditFiles)
	report.LikelyFiles = brainBriefMergeLikelyFiles(report.LikelyEditFiles, report.LikelyTestFiles)
	return true
}

func brainBriefGitIntentTerms(task string) []string {
	terms := brainBriefPostIndexLocatorTerms(task)
	if len(terms) > brainBriefGitIntentTermLimit {
		terms = terms[:brainBriefGitIntentTermLimit]
	}
	out := make([]string, 0, len(terms))
	for _, term := range terms {
		if brainBriefLiteralLocatorTerm(term) && len(term) <= 40 {
			out = append(out, term)
		}
	}
	return out
}

func brainBriefGitIntentOperationalTask(task string, terms []string) bool {
	for _, term := range terms {
		switch {
		case strings.HasPrefix(term, "install"), strings.HasPrefix(term, "config"),
			strings.HasPrefix(term, "workflow"), strings.HasPrefix(term, "compiler"),
			strings.HasPrefix(term, "build"), strings.HasPrefix(term, "packag"),
			strings.HasPrefix(term, "releas"), strings.HasPrefix(term, "hook"),
			strings.HasPrefix(term, "manifest"), strings.HasPrefix(term, "toolchain"),
			strings.HasPrefix(term, "deprecat"), strings.HasPrefix(term, "environment"),
			term == "cgo", term == "flags", term == "lane", term == "macos", term == "darwin":
			return true
		}
	}
	// The shared locator intentionally ignores two-character tokens. Keep the
	// common standalone CI intent without relaxing the safe Git term boundary.
	for _, word := range strings.FieldsFunc(strings.ToLower(task), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	}) {
		if word == "ci" {
			return true
		}
	}
	return false
}

func brainBriefGitIntentHasStrongCandidate(report brainBriefReport, terms []string) bool {
	for i, path := range report.LikelyEditFiles {
		clean, ok := cleanBrainBriefLikelyFile(path)
		if !ok {
			clean, ok = brainBriefGitIntentCleanOperationalPath(path)
		}
		if !ok {
			continue
		}
		hits, domainHits := brainBriefGitIntentTextHits(clean, terms)
		if (i == 0 && hits >= 2 && domainHits >= 2) ||
			(hits >= 3 && domainHits >= 2) {
			return true
		}
	}
	return false
}

func brainBriefGitIntentFile(
	ctx context.Context,
	runner CommandRunner,
	repoRoot, task string,
	terms []string,
) (string, bool) {
	if strings.TrimSpace(repoRoot) == "" || len(terms) < 2 || len(terms) > brainBriefGitIntentTermLimit {
		return "", false
	}
	for _, term := range terms {
		if !brainBriefLiteralLocatorTerm(term) || len(term) > 40 {
			return "", false
		}
	}
	streamer, ok := runner.(CommandStreamer)
	if !ok {
		return "", false
	}

	boundedCtx, cancel := context.WithTimeout(ctx, brainBriefGitIntentTimeout)
	defer cancel()
	logArgs := []string{
		"-c", "core.quotepath=false", "log", "--no-merges",
		"--max-count=" + strconv.Itoa(brainBriefGitIntentCommitLimit), "--regexp-ignore-case", "--fixed-strings",
		"--format=%H%x00%s", "-z",
	}
	grepCount := 0
	for _, term := range terms {
		// Generic work words such as "test" can fill the sixteen-commit
		// window before the domain-specific historical fix is reached. They
		// still contribute to final subject scoring, but only domain terms
		// select the bounded candidate window.
		if brainBriefGitIntentGenericTerm(term) {
			continue
		}
		// The value is letters/numbers only and --fixed-strings is set before
		// every pattern. Prefixing it inside --grep= prevents option, revision,
		// pathspec, and regex interpretation of task-controlled text.
		logArgs = append(logArgs, "--grep="+term)
		grepCount++
	}
	if grepCount == 0 {
		return "", false
	}
	logArgs = append(logArgs, "HEAD", "--")
	logOutput, ok := brainBriefGitIntentRead(boundedCtx, cancel, streamer, repoRoot, logArgs, brainBriefGitIntentOutputLimit)
	if !ok {
		return "", false
	}
	commits, ok := brainBriefGitIntentParseCommits(logOutput, terms)
	if !ok {
		return "", false
	}
	commit, ok := brainBriefGitIntentBestCommit(commits)
	if !ok {
		return "", false
	}

	remaining := brainBriefGitIntentOutputLimit - len(logOutput)
	if remaining <= 0 {
		return "", false
	}
	pathArgs := []string{
		"-c", "core.quotepath=false", "diff-tree", "--no-commit-id",
		"--name-only", "-z", "--diff-filter=AMR", "-r", commit.hash, "--",
	}
	pathOutput, ok := brainBriefGitIntentRead(boundedCtx, cancel, streamer, repoRoot, pathArgs, remaining)
	if !ok {
		return "", false
	}
	paths, ok := brainBriefGitIntentParsePaths(pathOutput)
	if !ok {
		return "", false
	}
	return brainBriefGitIntentBestPath(repoRoot, task, terms, paths)
}

func brainBriefGitIntentRead(
	ctx context.Context,
	cancel context.CancelFunc,
	streamer CommandStreamer,
	repoRoot string,
	args []string,
	limit int,
) ([]byte, bool) {
	if limit <= 0 || ctx.Err() != nil {
		return nil, false
	}
	stream, err := streamer.Stream(ctx, repoRoot, "git", args...)
	if err != nil {
		return nil, false
	}
	defer stream.Close()
	output, readErr := io.ReadAll(io.LimitReader(stream.Stdout(), int64(limit)+1))
	if readErr != nil || len(output) > limit {
		cancel()
		_ = stream.Close()
		_, _ = stream.Wait()
		return nil, false
	}
	if _, err := stream.Wait(); err != nil || ctx.Err() != nil {
		return nil, false
	}
	return output, true
}

func brainBriefGitIntentParseCommits(output []byte, terms []string) ([]brainBriefGitIntentCommit, bool) {
	if len(output) == 0 {
		return nil, true
	}
	if output[len(output)-1] != 0 || !utf8.Valid(output) {
		return nil, false
	}
	fields := bytes.Split(output[:len(output)-1], []byte{0})
	if len(fields)%2 != 0 || len(fields)/2 > brainBriefGitIntentCommitLimit {
		return nil, false
	}
	commits := make([]brainBriefGitIntentCommit, 0, len(fields)/2)
	seen := make(map[string]struct{}, len(fields)/2)
	for i := 0; i < len(fields); i += 2 {
		hash := strings.ToLower(string(fields[i]))
		subject := string(fields[i+1])
		if !brainBriefFullCommitPattern.MatchString(hash) || subject == "" ||
			strings.IndexFunc(subject, unicode.IsControl) >= 0 {
			return nil, false
		}
		if _, duplicate := seen[hash]; duplicate {
			return nil, false
		}
		seen[hash] = struct{}{}
		hits, domainHits := brainBriefGitIntentTextHits(subject, terms)
		if hits < 2 || domainHits == 0 {
			continue
		}
		commits = append(commits, brainBriefGitIntentCommit{
			hash:    hash,
			subject: subject,
			score:   hits*100 + domainHits,
		})
	}
	return commits, true
}

func brainBriefGitIntentBestCommit(commits []brainBriefGitIntentCommit) (brainBriefGitIntentCommit, bool) {
	if len(commits) == 0 {
		return brainBriefGitIntentCommit{}, false
	}
	best := commits[0]
	ambiguous := false
	for _, candidate := range commits[1:] {
		switch {
		case candidate.score > best.score:
			best = candidate
			ambiguous = false
		case candidate.score == best.score:
			ambiguous = true
		}
	}
	return best, !ambiguous
}

func brainBriefGitIntentParsePaths(output []byte) ([]string, bool) {
	if len(output) == 0 {
		return nil, true
	}
	if output[len(output)-1] != 0 || !utf8.Valid(output) {
		return nil, false
	}
	fields := bytes.Split(output[:len(output)-1], []byte{0})
	if len(fields) > brainBriefGitIntentPathLimit {
		return nil, false
	}
	paths := make([]string, 0, len(fields))
	seen := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		path := string(field)
		if path == "" || strings.IndexFunc(path, unicode.IsControl) >= 0 {
			return nil, false
		}
		clean, ok := cleanBrainBriefRepoRelativePath(path)
		if !ok || clean != filepath.ToSlash(path) {
			return nil, false
		}
		if _, duplicate := seen[clean]; duplicate {
			return nil, false
		}
		seen[clean] = struct{}{}
		paths = append(paths, clean)
	}
	return paths, true
}

func brainBriefGitIntentBestPath(repoRoot, task string, terms, paths []string) (string, bool) {
	repoFiles, ok := newBrainBriefRepoFileChecker(repoRoot)
	if !ok {
		return "", false
	}
	testIntent := brainBriefGitIntentTestIntent(task)
	candidates := make([]brainBriefGitIntentPath, 0, len(paths))
	for _, path := range paths {
		clean, valid := brainBriefGitIntentCleanOperationalPath(path)
		if !valid || clean != path || !repoFiles.exists(clean) {
			continue
		}
		hits, domainHits := brainBriefGitIntentTextHits(clean, terms)
		test := brainBriefGitIntentTestPath(clean)
		score := hits*10 + domainHits
		if test && !testIntent {
			score -= 3
		}
		candidates = append(candidates, brainBriefGitIntentPath{path: clean, score: score})
	}
	if len(candidates) == 0 {
		return "", false
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return candidates[i].path < candidates[j].path
	})
	if len(candidates) > 1 && candidates[0].score == candidates[1].score {
		return "", false
	}
	return candidates[0].path, true
}

func brainBriefGitIntentTextHits(text string, terms []string) (int, int) {
	tokens := brainBriefTaskWordPattern.FindAllString(strings.ToLower(text), -1)
	hits, domainHits := 0, 0
	for _, term := range terms {
		matched := false
		for _, token := range tokens {
			if brainBriefGitIntentTokenMatch(term, token) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		hits++
		if !brainBriefGitIntentGenericTerm(term) {
			domainHits++
		}
	}
	return hits, domainHits
}

func brainBriefGitIntentTokenMatch(term, token string) bool {
	if term == token {
		return true
	}
	if len(term) < 5 || len(token) < 5 {
		return false
	}
	return strings.HasPrefix(term, token) || strings.HasPrefix(token, term) ||
		strings.HasSuffix(term, token) || strings.HasSuffix(token, term)
}

func brainBriefGitIntentGenericTerm(term string) bool {
	switch {
	case strings.HasPrefix(term, "fix"), strings.HasPrefix(term, "repair"),
		strings.HasPrefix(term, "fail"), strings.HasPrefix(term, "error"),
		strings.HasPrefix(term, "issue"), strings.HasPrefix(term, "problem"),
		strings.HasPrefix(term, "chang"), strings.HasPrefix(term, "updat"),
		strings.HasPrefix(term, "ensur"), strings.HasPrefix(term, "prevent"),
		strings.HasPrefix(term, "preserv"), strings.HasPrefix(term, "support"),
		strings.HasPrefix(term, "test"), term == "code", term == "file", term == "command":
		return true
	default:
		return false
	}
}

func brainBriefGitIntentOperationalFile(path string) bool {
	lower := strings.ToLower(filepath.ToSlash(path))
	ext := strings.ToLower(filepath.Ext(lower))
	if strings.HasPrefix(lower, ".github/workflows/") {
		return ext == ".yml" || ext == ".yaml"
	}
	if strings.HasPrefix(lower, "scripts/") {
		return ext == ".sh" || ext == ".bash" || ext == ".zsh" || ext == ".py"
	}
	if strings.HasPrefix(lower, "test/") || strings.HasPrefix(lower, "tests/") {
		return ext == ".sh" || ext == ".bash" || ext == ".zsh" || ext == ".py"
	}
	if strings.Contains(lower, "/") {
		return false
	}
	switch lower {
	case "mise.toml", "entire-plugin.yml", "entire-plugin.yaml", "pyproject.toml", "cargo.toml":
		return true
	default:
		return false
	}
}

func brainBriefGitIntentCleanOperationalPath(path string) (string, bool) {
	if path == "" || strings.TrimSpace(path) != path || strings.IndexFunc(path, unicode.IsControl) >= 0 {
		return "", false
	}
	clean, ok := cleanBrainBriefRepoRelativePath(path)
	if !ok || clean != filepath.ToSlash(path) || brainBriefGitIntentExcludedPath(clean) ||
		!brainBriefGitIntentOperationalFile(clean) {
		return "", false
	}
	return clean, true
}

func brainBriefGitIntentMergeEdit(primary string, fallback []string) []string {
	out := make([]string, 0, min(1+len(fallback), 8))
	seen := make(map[string]struct{}, cap(out))
	if clean, ok := brainBriefGitIntentCleanOperationalPath(primary); ok {
		out = append(out, clean)
		seen[clean] = struct{}{}
	}
	for _, path := range fallback {
		clean, ok := cleanBrainBriefLikelyFile(path)
		if !ok {
			clean, ok = brainBriefGitIntentCleanOperationalPath(path)
			if !ok {
				continue
			}
		}
		if _, duplicate := seen[clean]; duplicate {
			continue
		}
		seen[clean] = struct{}{}
		out = append(out, clean)
		if len(out) >= 8 {
			break
		}
	}
	return out
}

func brainBriefGitIntentExcludedPath(path string) bool {
	lower := strings.ToLower(filepath.ToSlash(path))
	for _, segment := range strings.Split(lower, "/") {
		switch segment {
		case "vendor", "node_modules", "dist", "build", "coverage", "generated", "third_party":
			return true
		}
	}
	base := filepath.Base(lower)
	return strings.Contains(base, ".generated.") || strings.Contains(base, "_generated.") ||
		base == "package-lock.json" || base == "pnpm-lock.yaml" || base == "yarn.lock"
}

func brainBriefGitIntentTestIntent(task string) bool {
	for _, term := range brainBriefTaskWordPattern.FindAllString(strings.ToLower(task), -1) {
		if strings.HasPrefix(term, "test") || strings.HasPrefix(term, "verif") ||
			strings.HasPrefix(term, "validat") || strings.HasPrefix(term, "regression") {
			return true
		}
	}
	return false
}

func brainBriefGitIntentTestPath(path string) bool {
	lower := strings.ToLower(filepath.ToSlash(path))
	base := filepath.Base(lower)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	return brainBriefLikelyTestFile(lower) || strings.HasPrefix(stem, "test_") ||
		strings.HasSuffix(stem, "_test")
}
