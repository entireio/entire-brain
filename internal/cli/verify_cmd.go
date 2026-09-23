package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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

var (
	errVerifyIssues       = errors.New("verification found stale or orphaned facts")
	errVerifyStrictIssues = errors.New("verification found unverifiable-here facts")
	// errVerifyStoreIncomplete is the verdict for a corpus that is short: the
	// facts verify cannot read are neither verified nor orphaned, they are
	// gone, and reporting them as "0 orphaned" was the defect.
	errVerifyStoreIncomplete = errors.New("verification incomplete: the fact store cannot produce every fact the manifest declares")
)

type verifyCommandOptions struct {
	branch             string
	limit              int
	all                bool
	json               bool
	failOnUnverifiable bool
	strict             bool // alias flag for failOnUnverifiable; OR'd in at run time
	// sample verifies only the `limit` most recently updated facts in
	// no-target mode. Set by hot-path callers (brain status): full
	// verification shells two git subprocesses per distinct commit anchor,
	// which is `verify`'s job, not a status check's.
	sample bool
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
	// StoreIntegrity is the manifest-vs-store cross-check. verify's whole job
	// is fact integrity, and without this it answered "0 orphaned" about a
	// store that had lost a fact — it verified the facts it could still read
	// and never asked whether that was all of them. Present only when the
	// cross-check fails.
	StoreIntegrity *factStoreIntegrity `json:"store_integrity,omitempty"`
}

type verifySummary struct {
	Facts            int `json:"facts"`
	Verified         int `json:"verified"`
	Stale            int `json:"stale"`
	Orphaned         int `json:"orphaned"`
	UnverifiableHere int `json:"unverifiable_here"`
	// SampledOf is the active-fact count the sample was drawn from, set only
	// when sampling truncated the selection — so a sampled summary can never
	// be misread as full-corpus verification.
	SampledOf int `json:"sampled_of,omitempty"`
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

	// commitChecks memoizes verifyCommit results: a distill run anchors many
	// facts to the same checkpoint commits, and each uncached check costs two
	// git subprocesses (cat-file + for-each-ref --contains).
	commitChecks map[string]verifyCheck

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
	cmd.Flags().BoolVar(&verifyOpts.strict, "strict", false, "Alias for --fail-on-unverifiable")
	cmd.Flags().BoolVar(&verifyOpts.failOnUnverifiable, "fail-on-unverifiable", false, "Return nonzero when any matched fact is unverifiable-here (stale/orphaned always fail)")
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
	if err := verifyFailureForReport(report, verifyOpts); err != nil {
		return renderedCommandError{err: err}
	}
	return nil
}

func verifyFailureForReport(report verifyReport, verifyOpts verifyCommandOptions) error {
	// A store that cannot produce what the manifest declares fails verification
	// unconditionally, and before the per-fact verdicts: those verdicts were
	// computed over the survivors, so a clean sweep of them is not a pass. This
	// matches how a corrupt history index behaves today — the read refuses and
	// the command exits nonzero — rather than reporting success about a corpus
	// it knows is short.
	if report.StoreIntegrity != nil {
		return errVerifyStoreIncomplete
	}
	if report.Summary.Stale > 0 || report.Summary.Orphaned > 0 {
		return errVerifyIssues
	}
	if (verifyOpts.failOnUnverifiable || verifyOpts.strict) && report.Summary.UnverifiableHere > 0 {
		return errVerifyStrictIssues
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
	// A fact the store can no longer produce is not "0 orphaned" — it is a
	// fact that is gone, and verify is the command that must say so. Check the
	// manifest's claim against the store before verifying what survived, so
	// the verdict below can never be read as a clean bill of health for a
	// corpus that is short.
	var storeIntegrity *factStoreIntegrity
	if manifest != nil && manifest.Sources != nil {
		if integrity := inspectFactStore(brainDir, manifest.Sources.Facts); !integrity.OK() {
			storeIntegrity = &integrity
		}
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
		SchemaVersion:  verifySchemaVersion,
		GeneratedAt:    opts.Now().UTC(),
		Repo:           brainStatusRepo{Root: repoDir, Key: repoKey},
		BrainPath:      brainDir,
		Branch:         branch,
		Target:         strings.TrimSpace(target),
		Query:          query,
		Results:        []verifyFactResult{},
		StoreIntegrity: storeIntegrity,
	}
	for _, fact := range selected {
		result := vctx.verifyFact(fact)
		report.Results = append(report.Results, result)
		addVerifyResultToSummary(&report.Summary, result)
	}
	if verifyOpts.sample && target == "" {
		selectable := 0
		for _, fact := range allFacts {
			if verifyOpts.all || fact.Status == factStatusActive {
				selectable++
			}
		}
		if selectable > len(selected) {
			report.Summary.SampledOf = selectable
		}
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
	if opts.sample && opts.limit > 0 && len(selected) > opts.limit {
		// Most recently updated first: the facts an agent is most likely to
		// consume next are the most valuable ones to spot-check.
		sort.SliceStable(selected, func(i, j int) bool { return selected[i].UpdatedAt.After(selected[j].UpdatedAt) })
		selected = selected[:opts.limit]
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
	if fact.Origin == factOriginImported {
		// An imported fact's anchor names a session in the system it came from,
		// not one this repository ever exported. Run through the session checks
		// it would be reported orphaned — "its evidence is gone" — when the
		// truth is that its evidence was never here to begin with. That is a
		// different answer and a much more alarming one, particularly after an
		// import of any size.
		result.Verdict = verifyVerdictUnverifiableHere
		result.Reason = "fact was imported; its source is not this repository"
		return result
	}
	factBranch := fact.Branch
	if factBranch == "" {
		factBranch = v.branch
	}
	for _, anchor := range fact.Provenance {
		anchorResult := v.verifyAnchor(anchor, factBranch)
		result.Anchors = append(result.Anchors, anchorResult)
		result.Verdict = worseVerifyVerdict(result.Verdict, anchorResult.Verdict)
	}
	result.Reason = reasonForFactVerdict(result.Verdict)
	return result
}

func (v *verifyContext) verifyAnchor(anchor factAnchor, factBranch string) verifyAnchorResult {
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

	session, sessionFound, sessionCheck := v.verifyExportedSession(anchor, factBranch)
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
	transcriptRead := false
	if transcriptRel == "" {
		result.addCheck(verifyCheck{Name: "transcript", Verdict: verifyVerdictOrphaned, Reason: "anchor has no transcript path"})
	} else {
		if session.TranscriptPath != "" && anchor.Transcript != "" && filepath.ToSlash(session.TranscriptPath) != filepath.ToSlash(anchor.Transcript) {
			result.addCheck(verifyCheck{Name: "transcript_path", Verdict: verifyVerdictStale, Reason: fmt.Sprintf("exported session now points at %s", session.TranscriptPath)})
		}
		data, err := readCanonicalHistoryTranscript(v.ctx, v.brainDir, transcriptRel)
		if err != nil {
			result.addCheck(verifyCheck{Name: "transcript", Verdict: verifyVerdictOrphaned, Reason: err.Error()})
		} else {
			transcriptContent = string(data)
			transcriptRead = true
			result.addCheck(verifyCheck{Name: "transcript", Verdict: verifyVerdictVerified, Reason: transcriptRel})
		}
	}
	if anchor.Line > 0 && transcriptRead {
		line, ok := transcriptLine(transcriptContent, anchor.Line)
		if !ok {
			result.addCheck(verifyCheck{Name: "line", Verdict: verifyVerdictOrphaned, Reason: fmt.Sprintf("line %d is outside transcript", anchor.Line)})
		} else {
			result.LineExcerpt = truncateString(strings.Join(strings.Fields(line), " "), 200)
			result.addCheck(verifyCheck{Name: "line", Verdict: verifyVerdictVerified, Reason: fmt.Sprintf("line %d resolved", anchor.Line)})
		}
	}
	if strings.TrimSpace(anchor.CheckpointID) != "" && transcriptRead {
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
	// Anchors are data: a crafted "commit" like --upload-pack=... must never
	// reach git argv. Hex object ids only, matching the repo's hardening.
	if !verifyCommitIDPattern.MatchString(commit) {
		return verifyCheck{Name: "commit", Verdict: verifyVerdictOrphaned, Reason: "anchor commit is not a valid commit id"}
	}
	if cached, ok := v.commitChecks[commit]; ok {
		return cached
	}
	check := v.verifyCommitUncached(commit)
	if v.commitChecks == nil {
		v.commitChecks = map[string]verifyCheck{}
	}
	v.commitChecks[commit] = check
	return check
}

var verifyCommitIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{4,64}$`)

func (v *verifyContext) verifyCommitUncached(commit string) verifyCheck {
	// Verification is defined over locally retained evidence. Both existence
	// and reachability can traverse promisor objects, so guard every Git object
	// read against lazy network fetches just like checkpoint transcript reads.
	if _, _, err := runCheckpointGitWithPolicy(v.ctx, v.opts.Runner, v.repoDir, true, "cat-file", "-e", commit+"^{commit}"); err != nil {
		return verifyCheck{Name: "commit", Verdict: verifyVerdictOrphaned, Reason: "commit is missing locally"}
	}
	stdout, _, err := runCheckpointGitWithPolicy(v.ctx, v.opts.Runner, v.repoDir, true, "for-each-ref", "--format=%(refname)", "--contains", commit, "refs/heads", "refs/remotes", "refs/tags")
	if err != nil {
		return verifyCheck{Name: "commit", Verdict: verifyVerdictOrphaned, Reason: "commit reachability could not be checked locally"}
	}
	if strings.TrimSpace(string(stdout)) == "" {
		return verifyCheck{Name: "commit", Verdict: verifyVerdictOrphaned, Reason: "commit is not reachable from local refs"}
	}
	return verifyCheck{Name: "commit", Verdict: verifyVerdictVerified, Reason: "commit exists and is reachable from local refs"}
}

func (v *verifyContext) verifyExportedSession(anchor factAnchor, factBranch string) (exportSession, bool, verifyCheck) {
	sessionID := strings.TrimSpace(anchor.SessionID)
	if sessionID == "" {
		return exportSession{}, false, verifyCheck{Name: "session", Verdict: verifyVerdictOrphaned, Reason: "anchor has no session_id"}
	}
	if v.manifest == nil || v.manifest.Sources == nil || v.manifest.Sources.Sessions == nil {
		return exportSession{}, false, verifyCheck{Name: "session", Verdict: verifyVerdictOrphaned, Reason: "exported sessions source is missing"}
	}
	var bySession []exportSession
	for _, session := range v.manifest.Sources.Sessions.Sessions {
		if session.SessionID != sessionID {
			continue
		}
		if factBranch != "" && resolveDistillBranch(v.manifest, session) != factBranch {
			continue
		}
		bySession = append(bySession, session)
	}
	if len(bySession) > 0 {
		// One session id can span SEVERAL exported rows — the same session
		// exported on more than one branch — and a fact with no branch of its own
		// keeps all of them. Which row is returned decides the transcript and the
		// turn id every later check reads, so it may never be "whichever the
		// manifest listed first".
		if anchor.CheckpointID == "" {
			return pickExportedSession(v.manifest, bySession), true, verifyCheck{Name: "session", Verdict: verifyVerdictVerified, Reason: "session is present in exported sessions"}
		}
		if row, ok := exportedSessionAtCheckpoint(v.manifest, bySession, anchor.CheckpointID); ok {
			return row, true, verifyCheck{Name: "session", Verdict: verifyVerdictVerified, Reason: "session is present in exported sessions"}
		}
		// The anchor names a checkpoint that is not the session's latest. That
		// is not automatically staleness: provenance sharpening deliberately
		// anchors a fact to the checkpoint that actually changed the entity,
		// which is usually mid-session. It is only stale if the checkpoint does
		// not belong to this session at all.
		//
		// Verifying it requires knowing WHICH row owns it. With several rows and
		// no way to tell them apart, the honest answer is the pre-sharpening one
		// — stale — rather than an arbitrary row promoted to verified.
		if member, owned := v.sessionCheckpoint(sessionID, anchor.CheckpointID); owned {
			if row, ok := exportedSessionOwning(v.manifest, bySession, member); ok {
				return row, true, verifyCheck{
					Name:    "session",
					Verdict: verifyVerdictVerified,
					Reason:  fmt.Sprintf("session is present in exported sessions; anchor names its checkpoint %s", anchor.CheckpointID),
				}
			}
		}
		row := pickExportedSession(v.manifest, bySession)
		return row, true, verifyCheck{Name: "session", Verdict: verifyVerdictStale, Reason: fmt.Sprintf("session is now exported at checkpoint %s", row.LatestCheckpoint)}
	}
	if factBranch != "" {
		return exportSession{}, false, verifyCheck{Name: "session", Verdict: verifyVerdictOrphaned, Reason: fmt.Sprintf("session is missing from exported sessions on branch %s", factBranch)}
	}
	return exportSession{}, false, verifyCheck{Name: "session", Verdict: verifyVerdictOrphaned, Reason: "session is missing from exported sessions"}
}

// exportedSessionAtCheckpoint returns the row whose LATEST checkpoint is the
// anchored one — the exact match, and the only one that needs no local
// checkpoint store to justify it. Several rows sharing that checkpoint fall
// back to the deterministic pick.
func exportedSessionAtCheckpoint(manifest *exportManifest, rows []exportSession, checkpointID string) (exportSession, bool) {
	var matches []exportSession
	for _, row := range rows {
		if row.LatestCheckpoint == checkpointID {
			matches = append(matches, row)
		}
	}
	if len(matches) == 0 {
		return exportSession{}, false
	}
	return pickExportedSession(manifest, matches), true
}

// exportedSessionOwning returns the exported row a local (session, checkpoint)
// membership record belongs to, and ok=false when that cannot be decided. The
// membership record names the branch the checkpoint was made on, which is
// exactly what distinguishes one row of a multi-branch session from another; if
// it does not name one, or names one that does not select a single row, the
// caller keeps the conservative verdict instead of guessing.
func exportedSessionOwning(manifest *exportManifest, rows []exportSession, member sessionCheckpoint) (exportSession, bool) {
	if len(rows) == 1 {
		return rows[0], true
	}
	branch := strings.TrimSpace(member.Branch)
	if branch == "" {
		return exportSession{}, false
	}
	var matches []exportSession
	for _, row := range rows {
		if resolveDistillBranch(manifest, row) == branch {
			matches = append(matches, row)
		}
	}
	if len(matches) != 1 {
		return exportSession{}, false
	}
	return matches[0], true
}

// pickExportedSession chooses one of several rows the manifest lists for the
// same session id. Any total order over the rows' own contents will do; what
// matters is that it is not the manifest's array order, which is an artifact of
// how the export happened to be assembled.
func pickExportedSession(manifest *exportManifest, rows []exportSession) exportSession {
	best := rows[0]
	bestKey := exportedSessionKey(manifest, best)
	for _, row := range rows[1:] {
		if key := exportedSessionKey(manifest, row); key < bestKey {
			best, bestKey = row, key
		}
	}
	return best
}

func exportedSessionKey(manifest *exportManifest, row exportSession) string {
	return strings.Join([]string{
		resolveDistillBranch(manifest, row),
		row.LatestCheckpoint,
		strconv.Itoa(row.SessionIndex),
		row.TranscriptPath,
		row.TurnID,
	}, "\x00")
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
	if checkpointUnreadableInSnapshot(snapshot, checkpointID) {
		return verifyCheck{Name: "checkpoint", Verdict: verifyVerdictUnverifiableHere, Reason: "checkpoint was listed locally but its retained source is unreadable"}
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
	if checkpointUnreadableInSnapshot(snapshot, anchor.CheckpointID) {
		return verifyCheck{Name: "checkpoint_transcript", Verdict: verifyVerdictUnverifiableHere, Reason: "checkpoint was listed locally but its retained transcript source is unreadable"}
	}
	sourcePath, sourceKey := v.snapshotSourceTranscriptPath(snapshot, anchor, session)
	if sourcePath == "" {
		return verifyCheck{Name: "checkpoint_transcript", Verdict: verifyVerdictOrphaned, Reason: "checkpoint transcript path is missing"}
	}
	transcript, err := readSnapshotTranscriptFromSource(v.ctx, v.opts.Runner, snapshot, sourceKey, sourcePath)
	if err != nil {
		return verifyCheck{Name: "checkpoint_transcript", Verdict: verifyVerdictOrphaned, Reason: err.Error()}
	}
	if bytes.Equal(transcript, []byte(exportedTranscript)) {
		return verifyCheck{Name: "checkpoint_transcript", Verdict: verifyVerdictVerified, Reason: "exported transcript matches local checkpoint transcript"}
	}
	// The export carries the session's transcript at its LATEST checkpoint. A
	// sharpened anchor points at an earlier checkpoint of the same session,
	// whose transcript is that same session earlier in time — a prefix, because
	// a session's transcript only ever grows. Equality is still required for an
	// anchor on the latest checkpoint, so nothing about the original check is
	// relaxed; this only covers the earlier-checkpoint case the sharpener
	// introduced.
	if session.LatestCheckpoint != "" && anchor.CheckpointID != session.LatestCheckpoint &&
		len(transcript) > 0 && bytes.HasPrefix([]byte(exportedTranscript), transcript) {
		return verifyCheck{
			Name:    "checkpoint_transcript",
			Verdict: verifyVerdictVerified,
			Reason:  fmt.Sprintf("local checkpoint %s transcript is a prefix of the exported session transcript", anchor.CheckpointID),
		}
	}
	return verifyCheck{Name: "checkpoint_transcript", Verdict: verifyVerdictStale, Reason: "exported transcript differs from local checkpoint transcript"}
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

// sessionOwnsCheckpoint reports whether the local checkpoint store attributes a
// checkpoint to a session. It answers for EVERY checkpoint of the session, not
// just its latest, which is what lets a sharpened anchor verify.
func (v *verifyContext) sessionOwnsCheckpoint(sessionID, checkpointID string) bool {
	sessionID, checkpointID = strings.TrimSpace(sessionID), strings.TrimSpace(checkpointID)
	if sessionID == "" || checkpointID == "" {
		return false
	}
	_, ok := v.sessionCheckpoint(sessionID, checkpointID)
	return ok
}

// sessionCheckpoint returns the local (session, checkpoint) membership record.
func (v *verifyContext) sessionCheckpoint(sessionID, checkpointID string) (sessionCheckpoint, bool) {
	snapshot, err := v.localCheckpointSnapshot()
	if err != nil || snapshot == nil {
		return sessionCheckpoint{}, false
	}
	for _, member := range snapshot.SessionCheckpoints {
		if member.SessionID == sessionID && member.CheckpointID == checkpointID {
			return member, true
		}
	}
	return sessionCheckpoint{}, false
}

func (v *verifyContext) snapshotSourceTranscriptPath(snapshot *checkpointSnapshot, anchor factAnchor, session exportSession) (string, string) {
	for _, candidate := range snapshot.Selected {
		if candidate.SessionID == anchor.SessionID && candidate.CheckpointID == anchor.CheckpointID && candidate.SourceTranscriptPath != "" {
			return candidate.SourceTranscriptPath, candidate.SourceKey
		}
	}
	// Selected holds only each session's newest checkpoint. A sharpened anchor
	// names an earlier one, whose transcript lives at ITS own path under ITS own
	// session index — deriving the path from the exported (latest) session's
	// index would read the wrong blob, or none.
	for _, member := range snapshot.SessionCheckpoints {
		if member.SessionID == anchor.SessionID && member.CheckpointID == anchor.CheckpointID && member.TranscriptPath != "" {
			return member.TranscriptPath, member.SourceKey
		}
	}
	path := snapshotTranscriptPath(anchor.CheckpointID, session.SessionIndex, snapshot.TranscriptFileName)
	for sourceKey, source := range snapshot.Sources {
		if hasSnapshotTranscript(path, source.TreePaths) {
			return path, sourceKey
		}
	}
	if hasSnapshotTranscript(path, snapshot.TreePaths) {
		return path, ""
	}
	rootPath := snapshotRootTranscriptPath(anchor.CheckpointID, snapshot.TranscriptFileName)
	for sourceKey, source := range snapshot.Sources {
		if hasSnapshotTranscript(rootPath, source.TreePaths) {
			return rootPath, sourceKey
		}
	}
	if hasSnapshotTranscript(rootPath, snapshot.TreePaths) {
		return rootPath, ""
	}
	return "", ""
}

func loadLocalCheckpointSnapshotForVerify(ctx context.Context, runner CommandRunner, repoDir string) (*checkpointSnapshot, []string, error) {
	settings, settingsErr := readEntireSettings(repoDir)
	primary, configured, primaryErr := configuredCheckpointPrimary(repoDir, settings)
	if primaryErr != nil {
		return nil, nil, fmt.Errorf("%w: checkpoint backend selection is invalid: %v", errCheckpointSnapshotUnavailable, primaryErr)
	}
	if !configured {
		primary = checkpointBackendGitBranch
	}
	if settingsErr != nil && !errors.Is(settingsErr, os.ErrNotExist) && !configured {
		return nil, nil, fmt.Errorf("%w: checkpoint settings are malformed or unreadable: %v", errCheckpointSnapshotUnavailable, settingsErr)
	}
	snapshot, warnings, err := loadLocalCheckpointUnionSnapshot(ctx, runner, repoDir, primary, true, 0, checkpointBranchDestinations{}, nil, nil, true)
	if err != nil {
		return nil, warnings, err
	}
	return snapshot, warnings, nil
}

func checkpointExistsInSnapshot(snapshot *checkpointSnapshot, checkpointID string) bool {
	for sourceKey := range snapshot.Sources {
		if strings.HasSuffix(sourceKey, "\x00"+checkpointID) {
			return true
		}
	}
	prefix := checkpointPath(checkpointID) + "/"
	for path := range snapshot.TreePaths {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func checkpointUnreadableInSnapshot(snapshot *checkpointSnapshot, checkpointID string) bool {
	if snapshot == nil {
		return false
	}
	_, unreadable := snapshot.UnreadableCheckpoints[checkpointID]
	return unreadable
}

func transcriptLine(content string, line int) (string, bool) {
	if line <= 0 || content == "" {
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
	report, err := buildVerifyReport(ctx, opts, repoDir, brainDir, storage.Key, branch, verifyCommandOptions{limit: 10, sample: true}, "")
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
	if report.Facts == nil {
		report.Facts = &brainStatusFacts{}
	}
	report.Facts.Verification = &summary
}

func renderVerifyReportText(cmd *cobra.Command, report verifyReport) {
	out := cmd.OutOrStdout()
	s := report.Summary
	fmt.Fprintf(out, "verification: %d facts, %d verified, %d stale, %d orphaned, %d unverifiable-here\n",
		s.Facts, s.Verified, s.Stale, s.Orphaned, s.UnverifiableHere)
	// The summary above describes only the facts the store could still
	// produce. If it could not produce everything the manifest declares, that
	// line is a count of survivors, not a verdict — say so directly beneath it
	// rather than leaving "0 orphaned" to stand as the answer.
	if report.StoreIntegrity != nil {
		fmt.Fprintf(out, "store integrity: %s\n", report.StoreIntegrity.Warning())
		fmt.Fprintf(out, "  the counts above cover only the %d fact(s) still readable\n", report.StoreIntegrity.Readable)
	}
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
