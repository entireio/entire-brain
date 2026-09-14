package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Episode layer (Pattern Consolidation, Phase 1).
//
// An episode is the normalized unit pattern detection runs on: one substantive
// user request, the agent work that followed, and the next user turn (the
// reinforcement signal). Episodes are derived from the exported session
// transcripts already on disk, so extraction is deterministic and token-free —
// it must be safe to run during refresh.
//
// SCOPE NOTE: episodes carry identity, intent, the reinforcement label (via the
// Phase 0 classifier + commit-success signal), a source anchor, and the
// operational shape — tool_sequence and command_sequence (Phase 2,
// episode_tools.go) — that procedure detection groups on. Still deferred:
// per-episode `files` (needs apply_patch/edit target parsing) and a per-checkpoint
// Error -> WorkFailed signal (the manifest session record does not carry it).

const patternsEpisodesPath = "patterns/episodes.ndjson"

// patternSourceManifest is the brain-manifest entry for the patterns layer. It
// parallels historySourceManifest/factSourceManifest. Freshness is derived from
// the same sessions fingerprint the history source uses, so `patterns status` can
// report current/stale without re-running extraction. The procedure/practice/
// pattern/skill-memory counters are reserved for later phases and stay zero until
// then.
type patternSourceManifest struct {
	GeneratedAt  time.Time `json:"generated_at"`
	EpisodesPath string    `json:"episodes_path"`
	// PrivacyIdentity binds this corpus to the exact tombstone bytes the build
	// filtered against, exactly as historySourceManifest does. The sessions
	// fingerprint alone cannot see a privacy change: exclude/include leave the
	// canonical session set untouched, so without this an exclusion sweep
	// (which rewrites episodes.ndjson in place, behind this manifest's back)
	// left `patterns status` reporting the pre-sweep episode count as
	// "current", and a later `privacy include` never rebuilt the corpus at all
	// because nothing had changed as far as the fingerprint could tell.
	PrivacyIdentity     string              `json:"privacy_identity,omitempty"`
	SessionsFingerprint string              `json:"sessions_fingerprint,omitempty"`
	Episodes            int                 `json:"episodes"`
	Reinforcement       reinforcementCounts `json:"reinforcement"`
	Tasks               int                 `json:"tasks,omitempty"`
	Procedures          int                 `json:"procedures,omitempty"`
	Practices           int                 `json:"practices,omitempty"`
	Patterns            int                 `json:"patterns,omitempty"`
	SkillMemory         int                 `json:"skill_memory,omitempty"`
	Warnings            []string            `json:"warnings,omitempty"`
}

type reinforcementCounts struct {
	Success   int `json:"success"`
	Corrected int `json:"corrected"`
	Neutral   int `json:"neutral"`
}

// episodeRecord is one episode persisted to patterns/episodes.ndjson.
type episodeRecord struct {
	ID              string        `json:"id"`
	RepoKey         string        `json:"repo_key,omitempty"`
	Workspace       string        `json:"workspace,omitempty"`
	SessionID       string        `json:"session_id,omitempty"`
	CheckpointID    string        `json:"checkpoint_id,omitempty"`
	Branch          string        `json:"branch,omitempty"`
	Author          string        `json:"author,omitempty"`
	Agent           string        `json:"agent,omitempty"`
	Intent          string        `json:"intent,omitempty"`
	IntentSignature string        `json:"intent_signature,omitempty"`
	ToolSequence    []string      `json:"tool_sequence,omitempty"`
	CommandSequence []string      `json:"command_sequence,omitempty"`
	Reinforcement   string        `json:"reinforcement"`
	Source          episodeAnchor `json:"source"`
	CreatedAt       *time.Time    `json:"created_at,omitempty"`
}

type episodeAnchor struct {
	Path string `json:"path"`
	Line int    `json:"line,omitempty"`
}

// episodeID is a stable content hash over the identity-defining fields only —
// session, turn position, and normalized intent text. It deliberately excludes
// volatile fields (reinforcement, recency) so an episode's id never changes when
// later turns or feedback are added; the turn index disambiguates an identical
// request repeated within one session. Mirrors the fact-id scheme.
func episodeID(sessionID string, turnIndex int, intent string) string {
	sum := sha256.Sum256([]byte(sessionID + "\x00" + strconv.Itoa(turnIndex) + "\x00" + normalizeFeedback(intent)))
	return "episode:" + hex.EncodeToString(sum[:])
}

// intentSignature is a coarse, deterministic grouping key: the first two
// significant tokens of the intent joined by ":". It reuses the shared
// genericQueryStopwords base (and only that base, per the package's
// single-source-of-truth stopword rule) so generic fillers/verbs drop out while
// the topic survives, e.g. "review release readiness" -> "review:release".
func intentSignature(intent string) string {
	var sig []string
	for _, w := range brainBriefTaskWordPattern.FindAllString(strings.ToLower(intent), -1) {
		if genericQueryStopwords[w] {
			continue
		}
		sig = append(sig, w)
		if len(sig) == 2 {
			break
		}
	}
	return strings.Join(sig, ":")
}

// buildBrainEpisodes derives episodes from the exported session transcripts under
// outputDir. Deterministic and token-free. Canonical containment, inventory,
// and size failures refuse the whole build so callers never publish a partial
// episode/pattern projection.
func buildBrainEpisodes(outputDir string, now time.Time) ([]episodeRecord, *patternSourceManifest, error) {
	manifest, err := loadBrainManifest(outputDir)
	if err != nil {
		return nil, nil, err
	}
	if manifest.Sources == nil || manifest.Sources.Sessions == nil {
		return nil, nil, fmt.Errorf("session history missing; run `entire brain refresh sessions` first")
	}

	var (
		episodes []episodeRecord
		warnings []string
		counts   reinforcementCounts
	)
	// Session tombstones (Phase 4): excluded sessions and any duplicate
	// manifest aliases of their transcript contribute no episodes.
	guard, err := loadSessionReadGuard(outputDir, manifest)
	if err != nil {
		return nil, nil, err
	}
	// Episodes are a complete projection of the canonical session tree. Refuse
	// the build before parsing if any entry is unsafe or the bounded inventory
	// cannot be proven complete, and prove the entire membership stayed fixed
	// after all direct transcript reads (including irrelevant entries).
	beforeInventory, err := collectHistorySessionInventory(context.Background(), outputDir)
	if err != nil {
		return nil, nil, err
	}
	defer beforeInventory.Close()
	for _, session := range manifest.Sources.Sessions.Sessions {
		if guard.blocksExportSession(session) {
			continue
		}
		rel := strings.TrimSpace(session.TranscriptPath)
		if rel == "" {
			continue
		}
		data, readErr := readCanonicalHistoryTranscript(context.Background(), outputDir, rel)
		if readErr != nil {
			return nil, nil, fmt.Errorf("read transcript %s: %w", rel, readErr)
		}
		transcript := string(data)
		segments := transcriptEpisodeSegments(transcript)
		author := ""
		if len(session.Authors) > 0 {
			author = session.Authors[0].Name
		}
		for i, seg := range segments {
			turn := seg.Request
			feedback := ""
			if i+1 < len(segments) {
				feedback = segments[i+1].Request.Text
			}
			label := classifyReinforcement(reinforcementSignal{
				FeedbackText:  feedback,
				WorkCommitted: workCommitted(seg.WorkText),
			})
			tools, commands := episodeToolAndCommandSequences(seg.WorkText)
			switch label {
			case reinforcementSuccess:
				counts.Success++
			case reinforcementCorrected:
				counts.Corrected++
			default:
				counts.Neutral++
			}
			ep := episodeRecord{
				ID:              episodeID(session.SessionID, i, turn.Text),
				RepoKey:         manifest.RepoKey,
				SessionID:       session.SessionID,
				CheckpointID:    session.LatestCheckpoint,
				Branch:          session.Branch,
				Author:          author,
				Agent:           session.Agent,
				Intent:          truncateString(turn.Text, 280),
				IntentSignature: intentSignature(turn.Text),
				ToolSequence:    tools,
				CommandSequence: commands,
				Reinforcement:   label,
				Source:          episodeAnchor{Path: filepath.ToSlash(rel), Line: turn.Line},
			}
			if !session.CreatedAt.IsZero() {
				created := session.CreatedAt
				ep.CreatedAt = &created
			}
			episodes = append(episodes, ep)
		}
	}
	afterInventory, err := collectHistorySessionInventory(context.Background(), outputDir)
	if err != nil {
		return nil, nil, err
	}
	unchanged := beforeInventory.sameMembership(afterInventory)
	_ = afterInventory.Close()
	if !unchanged {
		return nil, nil, fmt.Errorf("%s: canonical transcript inventory changed while building episodes", memoryErrSourceStale)
	}

	// Deterministic order keeps episodes.ndjson stable across refreshes (small,
	// git-friendly diffs) and the output reproducible for tests.
	sort.Slice(episodes, func(i, j int) bool {
		if episodes[i].SessionID != episodes[j].SessionID {
			return episodes[i].SessionID < episodes[j].SessionID
		}
		if episodes[i].Source.Line != episodes[j].Source.Line {
			return episodes[i].Source.Line < episodes[j].Source.Line
		}
		return episodes[i].ID < episodes[j].ID
	})

	source := &patternSourceManifest{
		GeneratedAt:  now,
		EpisodesPath: patternsEpisodesPath,
		// The guard's identity is the exact tombstone bytes this build
		// filtered against, so the recorded epoch cannot drift from the
		// corpus it describes.
		PrivacyIdentity:     guard.policyIdentity,
		SessionsFingerprint: brainSessionsFingerprint(outputDir),
		Episodes:            len(episodes),
		Reinforcement:       counts,
		Warnings:            warnings,
	}
	return episodes, source, nil
}

// writeBrainEpisodesAndSource rebuilds the episode layer and records the patterns
// source in the manifest, under the brain write lock.
func writeBrainEpisodesAndSource(outputDir string, now time.Time) (*patternSourceManifest, error) {
	var source *patternSourceManifest
	err := withBrainWriteLock(outputDir, func() error {
		var runErr error
		source, runErr = writeBrainEpisodesAndSourceLocked(outputDir, now)
		return runErr
	})
	return source, err
}

func writeBrainEpisodesAndSourceLocked(outputDir string, now time.Time) (*patternSourceManifest, error) {
	episodes, source, err := buildBrainEpisodes(outputDir, now)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, ep := range episodes {
		if err := enc.Encode(ep); err != nil {
			return nil, fmt.Errorf("encode episode: %w", err)
		}
	}
	if err := writeBrainRelativeFileAtomic(outputDir, patternsEpisodesPath, buf.Bytes(), 0o600); err != nil {
		return nil, fmt.Errorf("write episodes: %w", err)
	}

	tasks := buildTaskCandidates(episodes, loadAllActiveFacts(outputDir))
	if err := writeBrainTasksFile(outputDir, tasks); err != nil {
		return nil, err
	}
	source.Tasks = len(tasks)

	procedures := buildBrainProcedures(episodes)
	if err := writeBrainProceduresFile(outputDir, procedures); err != nil {
		return nil, err
	}
	source.Procedures = len(procedures)

	manifest, err := loadBrainManifest(outputDir)
	if err != nil {
		return nil, err
	}
	if manifest.Sources == nil {
		manifest.Sources = &brainSources{}
	}

	practices, err := buildBrainPractices(outputDir, manifest, now)
	if err != nil {
		return nil, err
	}
	if err := writeBrainPracticesFile(outputDir, practices); err != nil {
		return nil, err
	}
	source.Practices = len(practices)
	source.Patterns = len(procedures) + len(practices)

	manifest.Sources.Patterns = source
	if manifest.GeneratedAt.IsZero() {
		manifest.GeneratedAt = now
	}
	if err := writeBrainManifestAndReadme(outputDir, *manifest); err != nil {
		return nil, err
	}
	return source, nil
}

// patternSourceCurrent reports whether a recorded patterns source still
// describes the brain's inputs. Both inputs matter: the canonical session set
// AND the tombstone policy the corpus was filtered against. `patterns status`
// and the refresh trigger share this so they can never disagree about whether
// a rebuild is owed.
//
// A source recorded before privacy_identity existed carries "", which stays
// current only while the brain has no tombstones at all — the same allowance
// the vector epoch check already makes. Once a tombstone exists, a corpus that
// cannot name the epoch it filtered against is exactly the silently-rewritten
// one this predicate has to catch.
func patternSourceCurrent(source *patternSourceManifest, brainDir string) bool {
	if source == nil {
		return false
	}
	if !sessionSourceFingerprintCurrent(source.SessionsFingerprint, brainSessionsFingerprint(brainDir)) {
		return false
	}
	_, state, err := loadSessionTombstonesChecked(brainDir)
	if err != nil {
		return false
	}
	if source.PrivacyIdentity == state.Identity {
		return true
	}
	return source.PrivacyIdentity == "" && state.State == sessionTombstoneAbsent
}

// refreshPatternLayer rebuilds the pattern layer (episodes -> tasks, procedures,
// practices) when its inputs changed or force is set, and reports whether it
// rebuilt. Deterministic and token-free; called by `entire brain refresh` so the
// pattern layer is populated without a separate command.
func refreshPatternLayer(brainDir string, force bool, now time.Time) (bool, error) {
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		return false, err
	}
	if manifest.Sources == nil || manifest.Sources.Sessions == nil {
		return false, nil // no sessions to build from
	}
	if !force && patternSourceCurrent(manifest.Sources.Patterns, brainDir) {
		return false, nil // current
	}
	if _, err := writeBrainEpisodesAndSource(brainDir, now); err != nil {
		return false, err
	}
	return true, nil
}

// loadBrainEpisodes reads the persisted episode layer. Missing file -> empty.
func loadBrainEpisodes(brainDir string) ([]episodeRecord, error) {
	content, err := readBrainRelativeStateFile(brainDir, patternsEpisodesPath)
	if err != nil {
		return nil, nil
	}
	var episodes []episodeRecord
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ep episodeRecord
		if err := json.Unmarshal([]byte(line), &ep); err != nil {
			return nil, fmt.Errorf("parse episode line: %w", err)
		}
		episodes = append(episodes, ep)
	}
	return episodes, nil
}
