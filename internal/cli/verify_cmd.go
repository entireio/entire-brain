package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	verifySchemaVersion = 1

	verifyVerdictVerified           = "verified"
	verifyVerdictStale              = "stale"
	verifyVerdictOrphaned           = "orphaned"
	verifyVerdictUnverifiableHere   = "unverifiable-here"
	verifyTurnSigningLimitation     = "pending turn signing"
	verifyCheckpointUnavailableHint = "local checkpoint refs unavailable"
)

var errVerifyIssues = errors.New("verification found stale or orphaned facts")

type verifyCommandOptions struct {
	branch string
	limit  int
	all    bool
	json   bool
}

type verifyReport struct {
	SchemaVersion int                `json:"schema_version"`
	GeneratedAt   time.Time          `json:"generated_at"`
	Repo          brainStatusRepo    `json:"repo"`
	BrainPath     string             `json:"brain_path"`
	Branch        string             `json:"branch"`
	Target        string             `json:"target,omitempty"`
	Query         string             `json:"query,omitempty"`
	Summary       verifySummary      `json:"summary"`
	Results       []verifyFactResult `json:"results"`
	Warnings      []string           `json:"warnings,omitempty"`
}

type verifySummary struct {
	Facts            int `json:"facts"`
	Verified         int `json:"verified"`
	Stale            int `json:"stale"`
	Orphaned         int `json:"orphaned"`
	UnverifiableHere int `json:"unverifiable_here"`
}

type verifyFactResult struct {
	Fact        factRecord           `json:"fact"`
	Verdict     string               `json:"verdict"`
	Reason      string               `json:"reason"`
	Anchors     []verifyAnchorResult `json:"anchors"`
	Limitations []verifyLimitation   `json:"limitations,omitempty"`
}

type verifyAnchorResult struct {
	Anchor      factAnchor    `json:"anchor"`
	Verdict     string        `json:"verdict"`
	Reason      string        `json:"reason"`
	Checks      []verifyCheck `json:"checks"`
	LineExcerpt string        `json:"line_excerpt,omitempty"`
}

type verifyCheck struct {
	Name    string `json:"name"`
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
}

type verifyLimitation struct {
	Scope   string `json:"scope"`
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
}

type verifyContext struct {
	ctx      context.Context
	opts     Options
	repoDir  string
	brainDir string
	branch   string

	manifest *exportManifest

	snapshotLoaded   bool
	snapshot         *checkpointSnapshot
	snapshotErr      error
	snapshotWarnings []string
}

func newVerifyCommand(opts Options) *cobra.Command {
	verifyOpts := verifyCommandOptions{limit: 10}
	cmd := &cobra.Command{
		Use:   "verify [<fact-id | query>]",
		Short: "Verify durable fact anchors against local retained sources",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := ""
			if len(args) == 1 {
				target = args[0]
			}
			return runVerify(cmd.Context(), cmd, opts, verifyOpts, target)
		},
	}
	cmd.Flags().StringVar(&verifyOpts.branch, "branch", "", "Branch to verify facts from (default: current branch)")
	cmd.Flags().IntVar(&verifyOpts.limit, "limit", 10, "Maximum facts to verify for query mode")
	cmd.Flags().BoolVar(&verifyOpts.all, "all", false, "Include superseded and retracted facts")
	cmd.Flags().BoolVar(&verifyOpts.json, "json", false, "Emit machine-readable JSON")
	return cmd
}

func runVerify(ctx context.Context, cmd *cobra.Command, opts Options, verifyOpts verifyCommandOptions, target string) error {
	if verifyOpts.limit <= 0 {
		return fmt.Errorf("--limit must be greater than 0")
	}
	repoDir, brainDir, branch, err := resolveFactsTarget(ctx, opts, agentSurfaceTarget(opts, nil), verifyOpts.branch)
	if err != nil {
		return err
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}
	report, err := buildVerifyReport(ctx, opts, repoDir, brainDir, storage.Key, branch, verifyOpts, target)
	if err != nil {
		return err
	}
	if verifyOpts.json {
		if err := writeJSON(cmd, report); err != nil {
			return err
		}
	} else {
		renderVerifyReportText(cmd, report)
	}
	if report.Summary.Stale > 0 || report.Summary.Orphaned > 0 {
		return renderedCommandError{err: errVerifyIssues}
	}
	return nil
}

func buildVerifyReport(ctx context.Context, opts Options, repoDir, brainDir, repoKey, branch string, verifyOpts verifyCommandOptions, target string) (verifyReport, error) {
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		return verifyReport{}, err
	}
	allFacts, err := loadFacts(brainDir, branch)
	if err != nil {
		return verifyReport{}, err
	}
	selected, query, err := selectFactsForVerify(allFacts, target, verifyOpts)
	if err != nil {
		return verifyReport{}, err
	}
	vctx := &verifyContext{
		ctx:      ctx,
		opts:     opts,
		repoDir:  repoDir,
		brainDir: brainDir,
		branch:   branch,
		manifest: manifest,
	}
	report := verifyReport{
		SchemaVersion: verifySchemaVersion,
		GeneratedAt:   opts.Now().UTC(),
		Repo:          brainStatusRepo{Root: repoDir, Key: repoKey},
		BrainPath:     brainDir,
		Branch:        branch,
		Target:        strings.TrimSpace(target),
		Query:         query,
		Results:       []verifyFactResult{},
	}
	for _, fact := range selected {
		result := vctx.verifyFact(fact)
		report.Results = append(report.Results, result)
		addVerifyResultToSummary(&report.Summary, result)
	}
	report.Warnings = append(report.Warnings, vctx.snapshotWarnings...)
	return report, nil
}

func selectFactsForVerify(facts []factRecord, target string, opts verifyCommandOptions) ([]factRecord, string, error) {
	target = strings.TrimSpace(target)
	if strings.HasPrefix(target, "fact:") {
		i := indexOfFact(facts, target)
		if i < 0 {
			return nil, "", fmt.Errorf("no fact %s", target)
		}
		return []factRecord{facts[i]}, "", nil
	}
	if target != "" {
		return rankFacts(facts, target, opts.limit, opts.all), target, nil
	}
	selected := make([]factRecord, 0, len(facts))
	for _, fact := range facts {
		if opts.all || fact.Status == factStatusActive {
			selected = append(selected, fact)
		}
	}
	return selected, "", nil
}

func (v *verifyContext) verifyFact(fact factRecord) verifyFactResult {
	result := verifyFactResult{
		Fact:    fact,
		Verdict: verifyVerdictVerified,
		Reason:  "all locally checkable anchors resolved",
		Anchors: []verifyAnchorResult{},
		Limitations: []verifyLimitation{{
			Scope:   "turn-signature",
			Verdict: verifyVerdictUnverifiableHere,
			Reason:  verifyTurnSigningLimitation,
		}},
	}
	if len(fact.Provenance) == 0 {
		result.Verdict = verifyVerdictUnverifiableHere
		result.Reason = "fact has no retained source anchor"
		return result
	}
	for _, anchor := range fact.Provenance {
		anchorResult := v.verifyAnchor(anchor)
		result.Anchors = append(result.Anchors, anchorResult)
		result.Verdict = worseVerifyVerdict(result.Verdict, anchorResult.Verdict)
	}
	result.Reason = reasonForFactVerdict(result.Verdict)
	return result
}

func (v *verifyContext) verifyAnchor(anchor factAnchor) verifyAnchorResult {
	result := verifyAnchorResult{Anchor: anchor, Verdict: verifyVerdictVerified, Reason: "anchor resolved locally", Checks: []verifyCheck{}}
	hasRetainedSource := strings.TrimSpace(anchor.SessionID) != "" ||
		strings.TrimSpace(anchor.CheckpointID) != "" ||
		strings.TrimSpace(anchor.Transcript) != "" ||
		anchor.Line > 0

	if strings.TrimSpace(anchor.Commit) != "" {
		result.addCheck(v.verifyCommit(anchor.Commit))
	}

	if !hasRetainedSource {
		if result.Verdict == verifyVerdictOrphaned {
			result.Reason = "commit anchor is missing locally"
			return result
		}
		result.addCheck(verifyCheck{Name: "retained_source", Verdict: verifyVerdictUnverifiableHere, Reason: "authored fact has no retained source anchor"})
		result.Reason = "authored fact has no retained source anchor"
		return result
	}

	session, sessionFound, sessionCheck := v.verifyExportedSession(anchor)
	result.addCheck(sessionCheck)
	if !sessionFound {
		result.Reason = sessionCheck.Reason
		return result
	}
	if strings.TrimSpace(anchor.TurnID) != "" {
		result.addCheck(verifyTurnID(session, anchor.TurnID))
	}
	if strings.TrimSpace(anchor.CheckpointID) != "" {
		result.addCheck(v.verifyCheckpoint(anchor.CheckpointID))
	}

	transcriptRel := strings.TrimSpace(anchor.Transcript)
	if transcriptRel == "" {
		transcriptRel = session.TranscriptPath
	}
	transcriptContent := ""
	if transcriptRel == "" {
		result.addCheck(verifyCheck{Name: "transcript", Verdict: verifyVerdictOrphaned, Reason: "anchor has no transcript path"})
	} else {
		if session.TranscriptPath != "" && anchor.Transcript != "" && filepath.ToSlash(session.TranscriptPath) != filepath.ToSlash(anchor.Transcript) {
			result.addCheck(verifyCheck{Name: "transcript_path", Verdict: verifyVerdictStale, Reason: fmt.Sprintf("exported session now points at %s", session.TranscriptPath)})
		}
		content, err := readBrainRelativeFile(v.brainDir, transcriptRel)
		if err != nil {
			result.addCheck(verifyCheck{Name: "transcript", Verdict: verifyVerdictOrphaned, Reason: err.Error()})
		} else {
			transcriptContent = content
			result.addCheck(verifyCheck{Name: "transcript", Verdict: verifyVerdictVerified, Reason: transcriptRel})
		}
	}
	if anchor.Line > 0 && transcriptContent != "" {
		line, ok := transcriptLine(transcriptContent, anchor.Line)
		if !ok {
			result.addCheck(verifyCheck{Name: "line", Verdict: verifyVerdictOrphaned, Reason: fmt.Sprintf("line %d is outside transcript", anchor.Line)})
		} else {
			result.LineExcerpt = truncateString(strings.Join(strings.Fields(line), " "), 200)
			result.addCheck(verifyCheck{Name: "line", Verdict: verifyVerdictVerified, Reason: fmt.Sprintf("line %d resolved", anchor.Line)})
		}
	}
	if strings.TrimSpace(anchor.CheckpointID) != "" && transcriptContent != "" {
		result.addCheck(v.verifyCheckpointTranscript(anchor, session, transcriptContent))
	}
	result.Reason = reasonForAnchorVerdict(result.Verdict)
	return result
}

func (r *verifyAnchorResult) addCheck(check verifyCheck) {
	if check.Name == "" {
		return
	}
	r.Checks = append(r.Checks, check)
	r.Verdict = worseVerifyVerdict(r.Verdict, check.Verdict)
}

func (v *verifyContext) verifyCommit(commit string) verifyCheck {
	commit = strings.TrimSpace(commit)
	if commit == "" {
		return verifyCheck{}
	}
	if _, _, err := v.opts.Runner.Run(v.ctx, v.repoDir, "git", "cat-file", "-e", commit+"^{commit}"); err != nil {
		return verifyCheck{Name: "commit", Verdict: verifyVerdictOrphaned, Reason: "commit is missing locally"}
	}
	stdout, _, err := v.opts.Runner.Run(v.ctx, v.repoDir, "git", "for-each-ref", "--format=%(refname)", "--contains", commit, "refs/heads", "refs/remotes", "refs/tags")
	if err != nil {
		return verifyCheck{Name: "commit", Verdict: verifyVerdictOrphaned, Reason: "commit reachability could not be checked locally"}
	}
	if strings.TrimSpace(string(stdout)) == "" {
		return verifyCheck{Name: "commit", Verdict: verifyVerdictOrphaned, Reason: "commit is not reachable from local refs"}
	}
	return verifyCheck{Name: "commit", Verdict: verifyVerdictVerified, Reason: "commit exists and is reachable from local refs"}
}

func (v *verifyContext) verifyExportedSession(anchor factAnchor) (exportSession, bool, verifyCheck) {
	sessionID := strings.TrimSpace(anchor.SessionID)
	if sessionID == "" {
		return exportSession{}, false, verifyCheck{Name: "session", Verdict: verifyVerdictOrphaned, Reason: "anchor has no session_id"}
	}
	if v.manifest == nil || v.manifest.Sources == nil || v.manifest.Sources.Sessions == nil {
		return exportSession{}, false, verifyCheck{Name: "session", Verdict: verifyVerdictOrphaned, Reason: "exported sessions source is missing"}
	}
	var bySession []exportSession
	for _, session := range v.manifest.Sources.Sessions.Sessions {
		if session.SessionID == sessionID {
			bySession = append(bySession, session)
			if anchor.CheckpointID == "" || session.LatestCheckpoint == anchor.CheckpointID {
				return session, true, verifyCheck{Name: "session", Verdict: verifyVerdictVerified, Reason: "session is present in exported sessions"}
			}
		}
	}
	if len(bySession) > 0 {
		return bySession[0], true, verifyCheck{Name: "session", Verdict: verifyVerdictStale, Reason: fmt.Sprintf("session is now exported at checkpoint %s", bySession[0].LatestCheckpoint)}
	}
	return exportSession{}, false, verifyCheck{Name: "session", Verdict: verifyVerdictOrphaned, Reason: "session is missing from exported sessions"}
}

func verifyTurnID(session exportSession, turnID string) verifyCheck {
	if session.TurnID == "" {
		return verifyCheck{Name: "turn", Verdict: verifyVerdictUnverifiableHere, Reason: "exported session has no turn_id; pending turn signing"}
	}
	if session.TurnID != turnID {
		return verifyCheck{Name: "turn", Verdict: verifyVerdictStale, Reason: fmt.Sprintf("exported session now points at turn %s", session.TurnID)}
	}
	return verifyCheck{Name: "turn", Verdict: verifyVerdictVerified, Reason: "turn_id is present in exported session metadata"}
}

func (v *verifyContext) verifyCheckpoint(checkpointID string) verifyCheck {
	snapshot, err := v.localCheckpointSnapshot()
	if err != nil {
		return verifyCheck{Name: "checkpoint", Verdict: verifyVerdictUnverifiableHere, Reason: verifyCheckpointUnavailableHint + ": " + err.Error()}
	}
	if !checkpointExistsInSnapshot(snapshot, checkpointID) {
		return verifyCheck{Name: "checkpoint", Verdict: verifyVerdictOrphaned, Reason: "checkpoint is missing from local checkpoint refs"}
	}
	return verifyCheck{Name: "checkpoint", Verdict: verifyVerdictVerified, Reason: "checkpoint exists in local checkpoint refs"}
}

func (v *verifyContext) verifyCheckpointTranscript(anchor factAnchor, session exportSession, exportedTranscript string) verifyCheck {
	snapshot, err := v.localCheckpointSnapshot()
	if err != nil {
		return verifyCheck{Name: "checkpoint_transcript", Verdict: verifyVerdictUnverifiableHere, Reason: verifyCheckpointUnavailableHint + ": " + err.Error()}
	}
	sourcePath := v.snapshotSourceTranscriptPath(snapshot, anchor, session)
	if sourcePath == "" {
		return verifyCheck{Name: "checkpoint_transcript", Verdict: verifyVerdictOrphaned, Reason: "checkpoint transcript path is missing"}
	}
	transcript, err := readSnapshotTranscript(v.ctx, v.opts.Runner, snapshot, sourcePath)
	if err != nil {
		return verifyCheck{Name: "checkpoint_transcript", Verdict: verifyVerdictOrphaned, Reason: err.Error()}
	}
	if !bytes.Equal(transcript, []byte(exportedTranscript)) {
		return verifyCheck{Name: "checkpoint_transcript", Verdict: verifyVerdictStale, Reason: "exported transcript differs from local checkpoint transcript"}
	}
	return verifyCheck{Name: "checkpoint_transcript", Verdict: verifyVerdictVerified, Reason: "exported transcript matches local checkpoint transcript"}
}

func (v *verifyContext) localCheckpointSnapshot() (*checkpointSnapshot, error) {
	if v.snapshotLoaded {
		return v.snapshot, v.snapshotErr
	}
	v.snapshotLoaded = true
	snapshot, warnings, err := loadLocalCheckpointSnapshotForVerify(v.ctx, v.opts.Runner, v.repoDir)
	v.snapshot = snapshot
	v.snapshotErr = err
	v.snapshotWarnings = append(v.snapshotWarnings, warnings...)
	return v.snapshot, v.snapshotErr
}

func (v *verifyContext) snapshotSourceTranscriptPath(snapshot *checkpointSnapshot, anchor factAnchor, session exportSession) string {
	for _, candidate := range snapshot.Selected {
		if candidate.SessionID == anchor.SessionID && candidate.CheckpointID == anchor.CheckpointID && candidate.SourceTranscriptPath != "" {
			return candidate.SourceTranscriptPath
		}
	}
	path := snapshotTranscriptPath(anchor.CheckpointID, session.SessionIndex, snapshot.TranscriptFileName)
	if _, ok := snapshot.TreePaths[path]; ok {
		return path
	}
	rootPath := snapshotRootTranscriptPath(anchor.CheckpointID, snapshot.TranscriptFileName)
	if _, ok := snapshot.TreePaths[rootPath]; ok {
		return rootPath
	}
	return ""
}

func loadLocalCheckpointSnapshotForVerify(ctx context.Context, runner CommandRunner, repoDir string) (*checkpointSnapshot, []string, error) {
	version := checkpointStorageV1
	refs := []string{v1MainRef, v1OriginRef}
	transcriptFileName := v1TranscriptFileName
	mode := "raw"
	if settings, err := readEntireSettings(repoDir); err == nil && settings.CheckpointsV2Enabled() {
		version = checkpointStorageV2
		refs = []string{v2MainRef}
		transcriptFileName = v2TranscriptFileName
		mode = "compact"
	}
	snapshot, _, warnings, err := loadCheckpointSnapshotFromGitDirRefs(ctx, runner, repoDir, refs, version, transcriptFileName, mode, defaultCheckpointLimit, checkpointBranchDestinations{}, nil, nil)
	if err != nil {
		return nil, warnings, err
	}
	return snapshot, warnings, nil
}

func checkpointExistsInSnapshot(snapshot *checkpointSnapshot, checkpointID string) bool {
	prefix := checkpointPath(checkpointID) + "/"
	for path := range snapshot.TreePaths {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func transcriptLine(content string, line int) (string, bool) {
	if line <= 0 {
		return "", false
	}
	lines := strings.Split(content, "\n")
	if line > len(lines) {
		return "", false
	}
	return lines[line-1], true
}

func addVerifyResultToSummary(summary *verifySummary, result verifyFactResult) {
	summary.Facts++
	switch result.Verdict {
	case verifyVerdictVerified:
		summary.Verified++
	case verifyVerdictStale:
		summary.Stale++
	case verifyVerdictOrphaned:
		summary.Orphaned++
	case verifyVerdictUnverifiableHere:
		summary.UnverifiableHere++
	}
}

func worseVerifyVerdict(a, b string) string {
	if verifyVerdictRank(b) > verifyVerdictRank(a) {
		return b
	}
	return a
}

func verifyVerdictRank(verdict string) int {
	switch verdict {
	case verifyVerdictOrphaned:
		return 4
	case verifyVerdictStale:
		return 3
	case verifyVerdictUnverifiableHere:
		return 2
	case verifyVerdictVerified:
		return 1
	default:
		return 0
	}
}

func reasonForFactVerdict(verdict string) string {
	switch verdict {
	case verifyVerdictVerified:
		return "all locally checkable anchors resolved"
	case verifyVerdictStale:
		return "one or more anchors resolve to changed local source"
	case verifyVerdictOrphaned:
		return "one or more anchors are missing locally"
	case verifyVerdictUnverifiableHere:
		return "one or more anchors need finer local evidence"
	default:
		return "unknown verification verdict"
	}
}

func reasonForAnchorVerdict(verdict string) string {
	switch verdict {
	case verifyVerdictVerified:
		return "anchor resolved locally"
	case verifyVerdictStale:
		return "anchor source changed locally"
	case verifyVerdictOrphaned:
		return "anchor source is missing locally"
	case verifyVerdictUnverifiableHere:
		return "anchor cannot be fully verified with local granularity"
	default:
		return "unknown verification verdict"
	}
}

func verificationSummaryForBranch(ctx context.Context, opts Options, repoDir, brainDir, branch string) (verifySummary, error) {
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return verifySummary{}, err
	}
	report, err := buildVerifyReport(ctx, opts, repoDir, brainDir, storage.Key, branch, verifyCommandOptions{limit: 10}, "")
	if err != nil {
		return verifySummary{}, err
	}
	return report.Summary, nil
}

func populateBrainStatusVerification(ctx context.Context, opts Options, report *brainStatusReport) {
	if report == nil || !report.Sources.Facts {
		return
	}
	branch := report.Live.Branch
	if branch == "" {
		branch = distillDefaultBranch
	}
	summary, err := verificationSummaryForBranch(ctx, opts, report.Repo.Root, report.Brain.Path, branch)
	if err != nil {
		report.Warnings = append(report.Warnings, "fact verification unavailable: "+err.Error())
		return
	}
	report.Verification = &summary
}

func renderVerifyReportText(cmd *cobra.Command, report verifyReport) {
	out := cmd.OutOrStdout()
	s := report.Summary
	fmt.Fprintf(out, "verification: %d facts, %d verified, %d stale, %d orphaned, %d unverifiable-here\n",
		s.Facts, s.Verified, s.Stale, s.Orphaned, s.UnverifiableHere)
	for _, result := range report.Results {
		fmt.Fprintf(out, "%s %s [%s]\n  %s\n", result.Verdict, result.Fact.ID, strings.Join(result.Fact.Paths, ","), result.Fact.Text)
		if result.Reason != "" {
			fmt.Fprintf(out, "  reason: %s\n", result.Reason)
		}
		for _, anchor := range result.Anchors {
			fmt.Fprintf(out, "  - %s session=%s checkpoint=%s commit=%s %s:%d\n",
				anchor.Verdict,
				valueOrUnset(anchor.Anchor.SessionID),
				valueOrUnset(anchor.Anchor.CheckpointID),
				valueOrUnset(anchor.Anchor.Commit),
				valueOrUnset(anchor.Anchor.Transcript),
				anchor.Anchor.Line)
			if anchor.Reason != "" {
				fmt.Fprintf(out, "    reason: %s\n", anchor.Reason)
			}
		}
	}
	for _, warning := range report.Warnings {
		fmt.Fprintf(out, "warning: %s\n", warning)
	}
}
