package cli

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Regression Radar: turn the brain from a passive librarian into an active reviewer.
// Given a query (task description + failing identifiers) it compares the CURRENT working
// tree against what the session history asserts the code looked like, and flags two narrow,
// high-precision shapes of regression:
//
//	changed: history asserts `<id> + "<literal>"` (or a `..` range) at a locus, but the
//	         current tree keeps the distinctive operand while the identifier was swapped out
//	         (e.g. scopeBaseRef+"..HEAD"  ->  "master..HEAD").
//	deleted: history asserts an assignment whose target contains <id> (e.g.
//	         `state.TranscriptPath = resolved`) and that assignment is now absent EVERYWHERE in
//	         the candidate files, though the identifier still lives in the tree.
//
// It reads RAW sessions (inspectBrainRawText), not the index, because the indexed Summary
// collapses code to prose; the precise invariant survives only in raw text. Presence is checked
// GLOBALLY across the candidate files (an assignment that simply lives in a different file is not
// a regression) — a finding is emitted only when history asserts something the current code does
// not contain anywhere.

const (
	regressionMaxFileBytes = 1 << 20
	regressionMaxAnomalies = 50
)

type regressionAnomaly struct {
	File       string  `json:"file"`
	Line       int     `json:"line,omitempty"`
	Kind       string  `json:"kind"` // "changed" | "deleted"
	Current    string  `json:"current"`
	Expected   string  `json:"expected"`
	Identifier string  `json:"identifier"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
	Evidence   string  `json:"evidence"`
}

type regressionReport struct {
	SchemaVersion int                 `json:"schema_version"` // shares reviewReportSchemaVersion; see docs/diffless_review_seam.md
	GeneratedAt   time.Time           `json:"generated_at"`
	Query         string              `json:"query"`
	RepoPath      string              `json:"repo_path"`
	BrainPath     string              `json:"brain_path"`
	Anomalies     []regressionAnomaly `json:"anomalies"`
	Scanned       int                 `json:"scanned_files"`
	Warnings      []string            `json:"warnings,omitempty"`
}

type regressionDetectorOptions struct {
	limit            int
	json             bool
	includeDeletions bool // deletions are higher-recall but noisier (no recency data); opt-in
	locationOnly     bool // emit file:line only, NOT expected/current — so a fair A/B can't paste the answer
}

type changeSignal struct {
	id      string // lowercased identifier
	operand string // distinctive operand, lowercased+despaced (e.g. `..head`)
	raw     string // human-readable asserted expression
	ev      string // session path:line
}

type deleteSignal struct {
	id     string
	target string // normalized LHS + assignment op (e.g. `state.transcriptpath=`)
	rhs    string // despaced right-hand side token (e.g. `resolved`) — used to locate the home file
	raw    string
	ev     string
}

type candFile struct {
	clean    string
	lines    []string
	norm     []string
	semantic bool // surfaced by the semantic index (a primary locus), not just named in a session
	rank     int  // semantic rank (lower = stronger locus)
	isTest   bool
}

func regressionIsComment(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "//") || strings.HasPrefix(t, "*") || strings.HasPrefix(t, "/*") ||
		strings.HasPrefix(t, "#") || strings.HasPrefix(t, "<!--")
}

func regressionIsTestPath(p string) bool {
	l := strings.ToLower(p)
	return strings.Contains(l, "_test.") || strings.Contains(l, ".test.") || strings.Contains(l, ".spec.") ||
		strings.Contains(l, "/test/") || strings.Contains(l, "/tests/") || strings.Contains(l, "/testdata/")
}

var regressionCodeFilePattern = regexp.MustCompile(`[\w./-]+\.(?:go|ts|tsx|js|jsx|py|rs|java|kt|swift|c|cc|cpp|h|hpp)`)

func regressionDespace(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == ' ' || r == '\t' {
			continue
		}
		b.WriteRune(r)
	}
	return strings.ToLower(b.String())
}

// regressionExtractSignals pulls change + delete signals for one identifier from a (space-
// collapsed) history excerpt using identifier-anchored regexes — tight enough that line-number
// noise and common tokens don't leak in.
func regressionExtractSignals(id, excerpt, ev string) ([]changeSignal, []deleteSignal) {
	q := regexp.QuoteMeta(id)
	// Known recall limits (documented, not silently dropped): literals >80 chars are skipped, an
	// all-lowercase identifier is never extracted as a query id upstream, and a literal that embeds
	// the id (e.g. `foo+"foobar"`) won't match. These bound recall, not precision.
	changeRe := regexp.MustCompile(`(?i)` + q + `\s*\+?\s*(?:"([^"]{2,80})"|([A-Za-z0-9_]*\.\.[A-Za-z0-9_]+))`)
	assignRe := regexp.MustCompile(`(?i)((?:[A-Za-z0-9_]+\.)*[A-Za-z0-9_]*` + q + `[A-Za-z0-9_]*)\s*(:?=)\s*([A-Za-z0-9_."'\[\]()+\-* ]{1,48})`)

	var changes []changeSignal
	seenC := map[string]struct{}{}
	for _, m := range changeRe.FindAllStringSubmatch(excerpt, -1) {
		operand := m[1]
		if operand == "" {
			operand = m[2]
		}
		operand = regressionDespace(operand)
		if len(operand) < 4 {
			continue
		}
		if _, ok := seenC[operand]; ok {
			continue
		}
		seenC[operand] = struct{}{}
		changes = append(changes, changeSignal{id: id, operand: operand, raw: strings.TrimSpace(m[0]), ev: ev})
	}

	var deletes []deleteSignal
	seenD := map[string]struct{}{}
	for _, m := range assignRe.FindAllStringSubmatch(excerpt, -1) {
		lhs := regressionDespace(m[1])
		op := m[2]
		if !strings.Contains(lhs, id) {
			continue
		}
		// Only field/state assignments (`a.b = c`) are persistent invariants whose absence is a
		// regression; `:=` locals are ephemeral, so their absence from a file is meaningless.
		if op != "=" || !strings.Contains(lhs, ".") {
			continue
		}
		target := lhs + op
		if _, ok := seenD[target]; ok {
			continue
		}
		seenD[target] = struct{}{}
		rhsTok := strings.TrimSpace(m[3])
		if i := strings.IndexAny(rhsTok, " (.,"); i > 0 {
			rhsTok = rhsTok[:i]
		}
		if rhsTok == "" {
			continue
		}
		raw := strings.TrimSpace(m[1]) + " " + op + " " + rhsTok
		deletes = append(deletes, deleteSignal{id: id, target: target, rhs: strings.ToLower(rhsTok), raw: raw, ev: ev})
	}
	return changes, deletes
}

// regressionScanHistory walks the raw brain (sessions + exports) once and extracts change/delete
// signals for the given identifiers from EVERY matching line — not a capped top-N — so rare but
// precise invariants are not lost. Bounded by a generous file cap and a distinct-signal cap.
func regressionScanHistory(brainDir string, ids []string) ([]changeSignal, []deleteSignal, map[string]struct{}) {
	const (
		maxFiles       = 20000
		maxLineBytes   = 4 * 1024 * 1024
		maxSignalsEach = 400
	)
	lowIDs := make([]string, 0, len(ids))
	for _, id := range ids {
		lowIDs = append(lowIDs, strings.ToLower(id))
	}
	var changes []changeSignal
	var deletes []deleteSignal
	files := map[string]struct{}{}
	seenC := map[string]struct{}{}
	seenD := map[string]struct{}{}
	scanned := 0
	_ = filepath.WalkDir(brainDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if n := d.Name(); n == semanticDirName || n == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if scanned >= maxFiles || (len(seenC) >= maxSignalsEach && len(seenD) >= maxSignalsEach) {
			return filepath.SkipAll
		}
		switch filepath.Ext(path) {
		case ".jsonl", ".json", ".md", ".txt":
		default:
			return nil
		}
		rel, _ := filepath.Rel(brainDir, path)
		relSlash := filepath.ToSlash(rel)
		if relSlash == "manifest.json" || strings.HasPrefix(relSlash, "seed/") {
			return nil
		}
		f, openErr := os.Open(path)
		if openErr != nil {
			return nil
		}
		defer f.Close()
		scanned++
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
		lineNo := 0
		for sc.Scan() {
			lineNo++
			line := sc.Text()
			low := strings.ToLower(line)
			matched := false
			for _, id := range lowIDs {
				if !strings.Contains(low, id) {
					continue
				}
				matched = true
				ex := normalizeHistoryTextForExcerpt(line)
				ev := relSlash + ":" + strconv.Itoa(lineNo)
				c, dl := regressionExtractSignals(id, ex, ev)
				for _, s := range c {
					if _, ok := seenC[s.id+"|"+s.operand]; !ok {
						seenC[s.id+"|"+s.operand] = struct{}{}
						changes = append(changes, s)
					}
				}
				for _, s := range dl {
					k := s.target + "|" + s.rhs
					if _, ok := seenD[k]; !ok {
						seenD[k] = struct{}{}
						deletes = append(deletes, s)
					}
				}
			}
			if matched {
				for _, fn := range regressionCodeFilePattern.FindAllString(line, -1) {
					files[strings.TrimLeft(fn, "+-/ ")] = struct{}{}
				}
			}
		}
		return nil
	})
	return changes, deletes, files
}

func detectRegressionAnomalies(brainDir, repoRoot string, semSource *semanticSourceManifest, query string, limit int, includeDeletions bool) ([]regressionAnomaly, int, []string) {
	var warnings []string
	ids := brainBriefRawHistoryQueries(query)
	if len(ids) == 0 {
		return []regressionAnomaly{}, 0, []string{"no code identifiers found in query; pass the failing symbols/terms"}
	}

	candidateFiles := map[string]struct{}{}
	semanticRank := map[string]int{} // clean path -> best (lowest) semantic rank; lower = stronger locus
	for _, rawID := range ids {
		id := strings.ToLower(rawID)
		if semSource != nil {
			if syms, _, err := semanticContextFacts(brainDir, semSource, id, 8, 0); err == nil {
				for rank, s := range syms {
					if s.FilePath == "" {
						continue
					}
					candidateFiles[s.FilePath] = struct{}{}
					c := filepath.Clean(s.FilePath)
					if r, ok := semanticRank[c]; !ok || rank < r {
						semanticRank[c] = rank
					}
				}
			}
		}
	}
	// Targeted history scan: walk the raw sessions once, keeping only signal-bearing lines (so a
	// rare invariant isn't lost behind thousands of plain identifier mentions, as a capped
	// generic scan would). Also collects the code files those sessions name.
	changes, deletes, namedFiles := regressionScanHistory(brainDir, ids)
	for f := range namedFiles {
		candidateFiles[f] = struct{}{}
	}
	if len(changes) == 0 && len(deletes) == 0 {
		return []regressionAnomaly{}, 0, append(warnings, "history holds no precise code assertions for these identifiers")
	}

	// Load all candidate files once.
	var files []candFile
	for file := range candidateFiles {
		clean := filepath.Clean(file)
		if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			continue
		}
		full := filepath.Join(repoRoot, clean)
		if err := rejectSymlinkPathComponents(repoRoot, clean); err != nil {
			continue // per-component symlink guard, matching the brain's other repo reads
		}
		info, err := os.Stat(full)
		if err != nil || info.IsDir() || info.Size() > regressionMaxFileBytes {
			continue
		}
		data, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		lines := strings.Split(string(data), "\n")
		norm := make([]string, len(lines))
		for i, ln := range lines {
			norm[i] = regressionDespace(ln)
		}
		rank, isSem := semanticRank[clean]
		if !isSem {
			rank = 1 << 30
		}
		files = append(files, candFile{clean: clean, lines: lines, norm: norm, semantic: isSem, rank: rank, isTest: regressionIsTestPath(clean)})
	}
	if len(files) == 0 {
		return []regressionAnomaly{}, 0, append(warnings, "no current files to verify against (semantic index missing?)")
	}
	// Deterministic, locus-prioritized order: implementation before tests, then by semantic rank
	// (the strongest fix-site first), then path — so a finding lands on the actual fix site, not a
	// random file that happens to share a common operand like `..HEAD`.
	sort.SliceStable(files, func(i, j int) bool {
		if files[i].isTest != files[j].isTest {
			return !files[i].isTest
		}
		if files[i].rank != files[j].rank {
			return files[i].rank < files[j].rank
		}
		return files[i].clean < files[j].clean
	})

	// "invariant still holds" check — must use the SAME comment/test filter as the emit loop, or a
	// stale comment/test copy of `operand+id` would falsely suppress a real regression (false negative).
	anyLineBoth := func(a, b string) bool {
		for _, f := range files {
			if f.isTest {
				continue
			}
			for i, n := range f.norm {
				if regressionIsComment(f.lines[i]) {
					continue
				}
				if strings.Contains(n, a) && strings.Contains(n, b) {
					return true
				}
			}
		}
		return false
	}
	anyHas := func(needle string) bool {
		for _, f := range files {
			for _, n := range f.norm {
				if strings.Contains(n, needle) {
					return true
				}
			}
		}
		return false
	}

	var anomalies []regressionAnomaly

	for _, c := range dedupeChanges(changes) {
		if anyLineBoth(c.operand, c.id) {
			continue // invariant still holds somewhere
		}
		// Likely-rename guard: if the operand is still concatenated to SOME identifier
		// (`<ident> + "...operand..."`, structure preserved), the invariant is intact under a renamed
		// symbol, not regressed — only flag when the operand now sits in a bare/hardcoded literal.
		renameRe := regexp.MustCompile(`[a-z_][a-z0-9_]*\s*\+\s*"[^"]*` + regexp.QuoteMeta(c.operand))
		for _, f := range files { // locate the swap: operand present, identifier gone
			if f.isTest {
				continue // a test's `..HEAD` is not the regressed implementation site
			}
			found := false
			for i, n := range f.norm {
				if regressionIsComment(f.lines[i]) {
					continue // prose mentions of a range like `main...HEAD` are not the regression
				}
				if strings.Contains(n, c.operand) && !strings.Contains(n, c.id) {
					if renameRe.MatchString(strings.ToLower(f.lines[i])) {
						continue // operand still concatenated to an identifier → benign rename, not a regression
					}
					anomalies = append(anomalies, regressionAnomaly{
						File: f.clean, Line: i + 1, Kind: "changed",
						Current: strings.TrimSpace(f.lines[i]), Expected: c.raw,
						Identifier: c.id, Confidence: 0.8, Evidence: c.ev,
						Reason: fmt.Sprintf("suspected regression: history asserts %q; this line keeps %q but dropped %q (could also be a rename — verify)", c.raw, c.operand, c.id),
					})
					found = true
					break
				}
			}
			if found {
				break
			}
		}
	}

	for _, d := range dedupeDeletes(deletes) {
		if !includeDeletions {
			break // deletions are higher-recall but noisier (benign churn) — opt-in only
		}
		// The SPECIFIC assignment (target + its RHS) must be absent everywhere — a different
		// assignment to the same field in another file is not this regression.
		if anyHas(d.target+d.rhs) || !anyHas(d.id) {
			continue
		}
		// Locate the home file: prefer one that also holds the RHS token (the natural site of
		// the assignment), else the first file where the identifier appears.
		home, line, cur := regressionHomeFile(files, d.id, d.rhs)
		if home == "" {
			continue
		}
		anomalies = append(anomalies, regressionAnomaly{
			File: home, Line: line, Kind: "deleted",
			Current: cur, Expected: d.raw,
			Identifier: d.id, Confidence: 0.65, Evidence: d.ev,
			Reason: fmt.Sprintf("history asserts `%s` but that assignment is absent everywhere in the current tree", d.raw),
		})
	}

	anomalies = regressionDedupeRank(anomalies)
	if limit > 0 && len(anomalies) > limit {
		anomalies = anomalies[:limit]
	}
	if len(anomalies) > regressionMaxAnomalies {
		anomalies = anomalies[:regressionMaxAnomalies]
	}
	if anomalies == nil {
		anomalies = []regressionAnomaly{}
	}
	return anomalies, len(files), warnings
}

// regressionHomeFile returns the best (file, line, text) to attribute a deletion to: a file
// containing both the identifier and the RHS token, else the first file holding the identifier.
func regressionHomeFile(files []candFile, id, rhs string) (string, int, string) {
	if rhs != "" {
		for _, f := range files {
			if !fileHasNorm(f.norm, rhs) {
				continue
			}
			for i, n := range f.norm {
				if strings.Contains(n, id) {
					return f.clean, i + 1, strings.TrimSpace(f.lines[i])
				}
			}
		}
	}
	for _, f := range files {
		for i, n := range f.norm {
			if strings.Contains(n, id) {
				return f.clean, i + 1, strings.TrimSpace(f.lines[i])
			}
		}
	}
	return "", 0, "(absent)"
}

func fileHasNorm(norm []string, needle string) bool {
	for _, n := range norm {
		if strings.Contains(n, needle) {
			return true
		}
	}
	return false
}

func dedupeChanges(in []changeSignal) []changeSignal {
	seen := map[string]struct{}{}
	var out []changeSignal
	for _, c := range in {
		k := c.id + "|" + c.operand
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, c)
	}
	return out
}

func dedupeDeletes(in []deleteSignal) []deleteSignal {
	seen := map[string]struct{}{}
	var out []deleteSignal
	for _, d := range in {
		k := d.target + "|" + d.rhs
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, d)
	}
	return out
}

func regressionDedupeRank(in []regressionAnomaly) []regressionAnomaly {
	seen := map[string]struct{}{}
	var out []regressionAnomaly
	for _, a := range in {
		key := a.File + "|" + a.Kind + "|" + regressionDespace(a.Expected)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, a)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Confidence != out[j].Confidence {
			return out[i].Confidence > out[j].Confidence
		}
		return out[i].File < out[j].File
	})
	return out
}

func runRegressionDetect(ctx context.Context, cmd *cobra.Command, opts Options, ro regressionDetectorOptions, query string) error {
	if ro.limit <= 0 {
		ro.limit = 20
	}
	target := agentSurfaceTarget(opts, nil)
	status, err := buildBrainStatusReport(ctx, opts, target)
	if err != nil {
		return err
	}
	var semSource *semanticSourceManifest
	if status.Manifest != nil && status.Manifest.Sources != nil {
		semSource = status.Manifest.Sources.Semantic
	}
	anomalies, scanned, warnings := detectRegressionAnomalies(status.Brain.Path, status.Repo.Root, semSource, query, ro.limit, ro.includeDeletions)
	if ro.locationOnly {
		// Hand only the suspected location, not the fix — so a fair A/B measures detection, not
		// the agent pasting a harness-computed `expected`.
		for i := range anomalies {
			anomalies[i].Expected = ""
			anomalies[i].Current = ""
			anomalies[i].Reason = "suspected regression site (location only)"
		}
	}
	report := regressionReport{
		SchemaVersion: reviewReportSchemaVersion,
		GeneratedAt:   opts.Now().UTC(),
		Query:         query,
		RepoPath:      status.Repo.Root,
		BrainPath:     status.Brain.Path,
		Anomalies:     anomalies,
		Scanned:       scanned,
		Warnings:      warnings,
	}
	if ro.json {
		return writeJSON(cmd, report)
	}
	out := cmd.OutOrStdout()
	if len(anomalies) == 0 {
		fmt.Fprintf(out, "No suspected regressions for %q (scanned %d files).\n", query, scanned)
		for _, w := range warnings {
			fmt.Fprintf(out, "  note: %s\n", w)
		}
		return nil
	}
	fmt.Fprintf(out, "Suspected regressions for %q:\n", query)
	for _, a := range anomalies {
		fmt.Fprintf(out, "  [%s, conf %.2f] %s:%d\n    expected: %s\n    current:  %s\n    why: %s\n    evidence: %s\n",
			a.Kind, a.Confidence, a.File, a.Line, a.Expected, a.Current, a.Reason, a.Evidence)
	}
	return nil
}

func newInspectRegressionsCommand(opts Options) *cobra.Command {
	ro := regressionDetectorOptions{limit: 20}
	cmd := &cobra.Command{
		Use:   "regressions <query>",
		Short: "Flag suspected regressions by comparing current code to session history",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRegressionDetect(cmd.Context(), cmd, opts, ro, args[0])
		},
	}
	cmd.Flags().IntVar(&ro.limit, "limit", 20, "Maximum suspected regressions to return")
	cmd.Flags().BoolVar(&ro.json, "json", false, "Emit machine-readable JSON")
	cmd.Flags().BoolVar(&ro.includeDeletions, "include-deletions", false, "Also flag deleted assignments (higher recall, noisier)")
	cmd.Flags().BoolVar(&ro.locationOnly, "location-only", false, "Emit only the suspected file:line, not the expected/current values")
	return cmd
}

// ---- Diff-less review: the versioned contract `entire review` / `labs investigate` are INTENDED to
//      consume (cross-repo, NOT yet wired) ----
//
// This is the machine contract, not a human verb (the human review surface is `entire review` in the
// cli; this command is hidden — see root.go). When wired, the cli's `entire review` WOULD gain a
// diff-less mode that shells `entire-brain review <query> --json`, checks reviewReport.schema_version,
// and folds these findings into the review prompt — reviewing the working tree against the brain's
// memory instead of a branch-vs-base diff (the graceful upgrade entire-brain gets from entire-sem).
// That cli mode is prototyped on a held branch, NOT landed. Full contract + consumer design:
// docs/diffless_review_seam.md.
//
// Consumer status (cross-repo, in entireio/cli — NOT in this repo, do not claim either is shipped):
//   - `entire review`            : prototyped on a held branch off Peyton's review redesign; NOT landed.
//   - `entire labs investigate`  : DESIGNED, NOT WIRED.
//
// TODO(diffless-seam): wire `entire labs investigate` to consume this contract — fold the suspected
// regressions for the investigation topic into the per-turn shared context so every brainstorming
// agent sees them. No diff concept there, so it fires whenever the brain is installed. Design lives in
// docs/diffless_review_seam.md; the cli-side hook is prototyped on the held branch, not landed here.

// reviewReportSchemaVersion is the contract version `entire review` is intended to bind to (the
// consumer is not yet landed; see above). Bump on any breaking change to reviewReport / reviewFinding
// (field rename/removal/semantics).
const reviewReportSchemaVersion = 1

type reviewFinding struct {
	Severity   string  `json:"severity"` // high (reserved) | medium (changed) | low (deleted)
	File       string  `json:"file"`
	Line       int     `json:"line,omitempty"`
	Title      string  `json:"title"`
	Detail     string  `json:"detail"`
	Evidence   string  `json:"evidence"`
	Confidence float64 `json:"confidence"`
}

type reviewReport struct {
	SchemaVersion int             `json:"schema_version"`
	GeneratedAt   time.Time       `json:"generated_at"`
	Mode          string          `json:"mode"`
	Query         string          `json:"query"`
	RepoPath      string          `json:"repo_path"`
	BrainPath     string          `json:"brain_path"`
	Summary       string          `json:"summary"`
	Findings      []reviewFinding `json:"findings"`
	Warnings      []string        `json:"warnings,omitempty"`
}

// regressionSeverity maps detector confidence to a review severity. Every finding the current
// heuristic detector emits is *suspected* — `changed` (0.8) is precise but rename-ambiguous and
// `deleted` (0.65) is noisier/opt-in — so none warrant "high". That tier is reserved for a future
// high-confidence signal (a durable fact bound to a locus with recency), so the machine severity
// never contradicts the "could be a rename — verify" hedge: changed -> medium, deleted -> low.
func regressionSeverity(conf float64) string {
	switch {
	case conf >= 0.9:
		return "high" // reserved: no heuristic finding reaches this today
	case conf >= 0.7:
		return "medium"
	default:
		return "low"
	}
}

func anomalyToReviewFinding(a regressionAnomaly) reviewFinding {
	detail := "suspected regression at this location — verify against the brain's history."
	if a.Expected != "" {
		detail = fmt.Sprintf("history shows `%s`; current is `%s` — verify this change is intentional (could also be a rename).", a.Expected, a.Current)
	}
	return reviewFinding{
		Severity:   regressionSeverity(a.Confidence),
		File:       a.File,
		Line:       a.Line,
		Title:      fmt.Sprintf("Suspected regression: `%s` %s", a.Identifier, a.Kind),
		Detail:     detail,
		Evidence:   a.Evidence,
		Confidence: a.Confidence,
	}
}

func runBrainReview(ctx context.Context, cmd *cobra.Command, opts Options, ro regressionDetectorOptions, query string) error {
	if ro.limit <= 0 {
		ro.limit = 20
	}
	target := agentSurfaceTarget(opts, nil)
	status, err := buildBrainStatusReport(ctx, opts, target)
	if err != nil {
		return err
	}
	var semSource *semanticSourceManifest
	if status.Manifest != nil && status.Manifest.Sources != nil {
		semSource = status.Manifest.Sources.Semantic
	}
	anomalies, _, warnings := detectRegressionAnomalies(status.Brain.Path, status.Repo.Root, semSource, query, ro.limit, ro.includeDeletions)
	if ro.locationOnly {
		// Parity with `inspect regressions --location-only`: hand the suspected site, not the fix,
		// so a fair consumer/A-B measures detection rather than pasting a harness-computed value.
		for i := range anomalies {
			anomalies[i].Expected = ""
			anomalies[i].Current = ""
			anomalies[i].Reason = "suspected regression site (location only)"
		}
	}
	findings := make([]reviewFinding, 0, len(anomalies))
	for _, a := range anomalies {
		findings = append(findings, anomalyToReviewFinding(a))
	}
	summary := "Diff-less review: no suspected regressions (current tree matches the brain's memory)."
	if len(findings) > 0 {
		summary = fmt.Sprintf("Diff-less review: %d suspected regression(s) — verify each before acting.", len(findings))
	}
	report := reviewReport{
		SchemaVersion: reviewReportSchemaVersion,
		GeneratedAt:   opts.Now().UTC(),
		Mode:          "diff-less (brain memory vs current tree)",
		Query:         query,
		RepoPath:      status.Repo.Root,
		BrainPath:     status.Brain.Path,
		Summary:       summary,
		Findings:      findings,
		Warnings:      warnings,
	}
	if ro.json {
		return writeJSON(cmd, report)
	}
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, report.Summary)
	for _, f := range findings {
		fmt.Fprintf(out, "\n  [%s] %s\n    %s:%d\n    %s\n    evidence: %s\n",
			strings.ToUpper(f.Severity), f.Title, f.File, f.Line, f.Detail, f.Evidence)
	}
	for _, w := range warnings {
		fmt.Fprintf(out, "  note: %s\n", w)
	}
	return nil
}

func newBrainReviewCommand(opts Options) *cobra.Command {
	ro := regressionDetectorOptions{limit: 20}
	cmd := &cobra.Command{
		Use:   "review <query>",
		Short: "Diff-less review: flag suspected regressions in the current tree vs the brain's memory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBrainReview(cmd.Context(), cmd, opts, ro, args[0])
		},
	}
	cmd.Flags().IntVar(&ro.limit, "limit", 20, "Maximum findings")
	cmd.Flags().BoolVar(&ro.json, "json", false, "Emit machine-readable JSON")
	cmd.Flags().BoolVar(&ro.includeDeletions, "include-deletions", false, "Also flag deleted assignments (lower confidence, noisier)")
	cmd.Flags().BoolVar(&ro.locationOnly, "location-only", false, "Emit only the suspected file:line, not the expected/current values")
	return cmd
}
