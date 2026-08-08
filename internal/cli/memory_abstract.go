package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// memory_abstract.go is C4: optional, evidence-linked session abstracts.
// Abstracts are disposable navigation metadata, never capture truth, durable
// facts, or instructions. Generation requires explicit configuration of a
// provider; no network provider is ever selected implicitly, and every
// deterministic search/get/privacy/lifecycle path makes zero generation
// calls. The feature ships disabled: with no provider registered, every
// surface reports its status truthfully and nothing else changes.

const (
	memoryConfigRel           = historyDirName + "/memory-config-v1.json"
	memoryConfigSchemaVersion = 1

	abstractsDirRel        = historyDirName + "/abstracts/v1"
	abstractSchemaVersion  = 1
	abstractOverviewMax    = 4 * 1024
	abstractStatementMax   = 1024
	abstractStatementsMax  = 8
	abstractEvidenceMax    = 32
	abstractRangesMax      = 32
	abstractArtifactMax    = 16 * 1024
	abstractWindowMaxTurns = 8
	abstractWindowMaxBytes = 64 * 1024
	abstractMaxWindows     = 32
	abstractMaxTurns       = 256

	abstractStatusDisabled    = "disabled"
	abstractStatusMissing     = "missing"
	abstractStatusCurrent     = "current"
	abstractStatusStale       = "stale"
	abstractStatusUnavailable = "provider_unavailable"
)

type memoryAbstractsConfig struct {
	Enabled             bool   `json:"enabled"`
	Automatic           bool   `json:"automatic"`
	Provider            string `json:"provider"`
	Model               string `json:"model"`
	HostedEgressAllowed bool   `json:"hosted_egress_allowed"`
}

type memoryConfig struct {
	SchemaVersion int                   `json:"schema_version"`
	Abstracts     memoryAbstractsConfig `json:"abstracts"`
}

// loadMemoryConfig: the absent-file state is identical to all-false defaults.
func loadMemoryConfig(brainDir string) memoryConfig {
	empty := memoryConfig{SchemaVersion: memoryConfigSchemaVersion}
	data, err := safeReadFile(filepath.Join(brainDir, filepath.FromSlash(memoryConfigRel)), maxManifestBytes)
	if err != nil {
		return empty
	}
	var config memoryConfig
	if json.Unmarshal(data, &config) != nil || config.SchemaVersion != memoryConfigSchemaVersion {
		return empty
	}
	return config
}

func saveMemoryConfig(brainDir string, config memoryConfig) error {
	config.SchemaVersion = memoryConfigSchemaVersion
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return writeBrainRelativeFileAtomic(brainDir, memoryConfigRel, append(data, '\n'), 0o600)
}

// abstractStatement is one evidence-linked claim; the evidence ids must cite
// exchanges of the same session.
type abstractStatement struct {
	Text        string   `json:"text"`
	EvidenceIDs []string `json:"evidence_ids"`
}

type abstractCoverageRange struct {
	StartTurn int `json:"start_turn"`
	EndTurn   int `json:"end_turn"`
}

type sessionAbstract struct {
	SchemaVersion int       `json:"schema_version"`
	SessionRef    string    `json:"session_ref"`
	SessionDigest string    `json:"session_digest"`
	GeneratedAt   time.Time `json:"generated_at"`
	Generator     struct {
		Kind            string `json:"kind"` // local | hosted
		Provider        string `json:"provider"`
		Model           string `json:"model"`
		ContractVersion int    `json:"contract_version"`
	} `json:"generator"`
	Overview       abstractStatement   `json:"overview"`
	Outcomes       []abstractStatement `json:"outcomes,omitempty"`
	Decisions      []abstractStatement `json:"decisions,omitempty"`
	Unresolved     []abstractStatement `json:"unresolved,omitempty"`
	InputTruncated bool                `json:"input_truncated,omitempty"`
	Coverage       struct {
		TotalTurns     int                     `json:"total_turns"`
		IncludedTurns  int                     `json:"included_turns"`
		IncludedRanges []abstractCoverageRange `json:"included_ranges,omitempty"`
	} `json:"coverage"`
}

// conversationAbstractInput is the bounded structured view a generator
// receives: visible exchange projections with their ids only. Hidden
// reasoning and raw tool output are excluded by construction (projections
// never contained them).
type conversationAbstractInput struct {
	SessionRef     string
	SessionDigest  string
	InputTruncated bool
	Windows        [][]conversationTurn
	TotalTurns     int
	IncludedRanges []abstractCoverageRange
}

// ConversationAbstractor is the C4 provider seam. Implementations expose
// their resolved identity; hosted providers may be invoked only with an
// explicit egress acknowledgement.
type ConversationAbstractor interface {
	Identity() (kind, provider, model string)
	Abstract(input conversationAbstractInput) (sessionAbstract, error)
}

// memoryAbstractorFactory resolves the configured provider name to an
// implementation. nil (the default build) has no providers: every generation
// request is memory_provider_unavailable, and no fallback is ever attempted.
var memoryAbstractorFactory func(config memoryAbstractsConfig) (ConversationAbstractor, error)

// sessionViewDigest is the C4 abstract input identity, computed over the
// reconciled session view sorted by turn ordinal.
func sessionViewDigest(view conversationSessionView) string {
	h := sha256.New()
	h.Write([]byte(view.Ref))
	h.Write([]byte{0})
	for _, record := range view.Records {
		fmt.Fprintf(h, "%d", record.TurnOrdinal)
		h.Write([]byte{0})
		h.Write([]byte(record.ID))
		h.Write([]byte{0})
		h.Write([]byte(record.SourceDigest))
		h.Write([]byte{0})
		h.Write([]byte(record.RequestDigest))
		h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func abstractRel(sessionDigest string) string {
	return filepath.ToSlash(filepath.Join(abstractsDirRel, strings.TrimPrefix(sessionDigest, "sha256:")+".json"))
}

func loadSessionAbstract(brainDir, sessionDigest string) (sessionAbstract, bool) {
	data, err := safeReadFile(filepath.Join(brainDir, filepath.FromSlash(abstractRel(sessionDigest))), maxManifestBytes)
	if err != nil {
		return sessionAbstract{}, false
	}
	var artifact sessionAbstract
	if json.Unmarshal(data, &artifact) != nil || artifact.SchemaVersion != abstractSchemaVersion {
		return sessionAbstract{}, false
	}
	return artifact, true
}

// abstractWindows builds bounded deterministic windows over the session view:
// at most 8 exchanges / 64 KiB per window; past 32 windows or 256 exchanges,
// select the first two and last two windows plus uniformly spaced middle
// windows and mark the input truncated, recording exact turn ranges.
func abstractWindows(view conversationSessionView) ([][]conversationTurn, []abstractCoverageRange, bool) {
	var windows [][]conversationTurn
	var current []conversationTurn
	currentBytes := 0
	flush := func() {
		if len(current) > 0 {
			windows = append(windows, current)
			current = nil
			currentBytes = 0
		}
	}
	for _, record := range view.Records {
		entry := conversationOutlineEntry(record)
		entry.Text = record.Summary // bounded projection, not raw transcript
		cost := len(entry.Text) + len(entry.Request)
		if len(current) >= abstractWindowMaxTurns || currentBytes+cost > abstractWindowMaxBytes {
			flush()
		}
		current = append(current, entry)
		currentBytes += cost
	}
	flush()
	truncated := len(windows) > abstractMaxWindows || len(view.Records) > abstractMaxTurns
	if truncated && len(windows) > 4 {
		keep := make([][]conversationTurn, 0, abstractMaxWindows)
		keep = append(keep, windows[0], windows[1])
		middle := windows[2 : len(windows)-2]
		slots := abstractMaxWindows - 4
		if slots > len(middle) {
			slots = len(middle)
		}
		for i := 0; i < slots; i++ {
			keep = append(keep, middle[i*len(middle)/max(slots, 1)])
		}
		keep = append(keep, windows[len(windows)-2], windows[len(windows)-1])
		windows = keep
	}
	var ranges []abstractCoverageRange
	for _, window := range windows {
		if len(window) == 0 {
			continue
		}
		ranges = append(ranges, abstractCoverageRange{StartTurn: window[0].TurnOrdinal, EndTurn: window[len(window)-1].TurnOrdinal})
		if len(ranges) >= abstractRangesMax {
			break
		}
	}
	return windows, ranges, truncated
}

// validateSessionAbstract enforces every C4 bound and citation rule before
// publication. Partial or fabricated output is discarded, never stored.
func validateSessionAbstract(artifact sessionAbstract, view conversationSessionView) error {
	valid := map[string]bool{}
	for _, record := range view.Records {
		valid[record.ID] = true
	}
	checkStatement := func(kind string, statement abstractStatement, overviewBound int) error {
		if len(statement.Text) > overviewBound {
			return fmt.Errorf("%s text exceeds %d bytes", kind, overviewBound)
		}
		if len(statement.EvidenceIDs) > abstractEvidenceMax {
			return fmt.Errorf("%s cites more than %d evidence ids", kind, abstractEvidenceMax)
		}
		for _, id := range statement.EvidenceIDs {
			if !valid[id] {
				return fmt.Errorf("%s cites %s which is not an exchange of session %s", kind, id, view.Ref)
			}
		}
		return nil
	}
	if artifact.SessionRef != view.Ref {
		return fmt.Errorf("abstract session_ref mismatch")
	}
	if strings.TrimSpace(artifact.Overview.Text) != "" && len(artifact.Overview.EvidenceIDs) == 0 {
		return fmt.Errorf("a non-empty overview requires at least one evidence id")
	}
	if err := checkStatement("overview", artifact.Overview, abstractOverviewMax); err != nil {
		return err
	}
	for name, list := range map[string][]abstractStatement{"outcomes": artifact.Outcomes, "decisions": artifact.Decisions, "unresolved": artifact.Unresolved} {
		if len(list) > abstractStatementsMax {
			return fmt.Errorf("%s has more than %d statements", name, abstractStatementsMax)
		}
		for _, statement := range list {
			if err := checkStatement(name, statement, abstractStatementMax); err != nil {
				return err
			}
			if strings.TrimSpace(statement.Text) != "" && len(statement.EvidenceIDs) == 0 {
				return fmt.Errorf("%s statement lacks evidence", name)
			}
		}
	}
	if len(artifact.Coverage.IncludedRanges) > abstractRangesMax {
		return fmt.Errorf("coverage lists more than %d ranges", abstractRangesMax)
	}
	encoded, err := json.Marshal(artifact)
	if err != nil {
		return err
	}
	if len(encoded) > abstractArtifactMax {
		return fmt.Errorf("abstract artifact exceeds %d bytes", abstractArtifactMax)
	}
	return nil
}

// generateSessionAbstract runs the configured provider for one session view
// and atomically publishes the validated artifact.
func generateSessionAbstract(brainDir string, view conversationSessionView, config memoryConfig, now time.Time) (sessionAbstract, error) {
	if !config.Abstracts.Enabled {
		return sessionAbstract{}, fmt.Errorf("%s: session abstracts are disabled; enable them with `memory configure abstracts --enable --provider <name>`", memoryErrProviderUnavail)
	}
	if memoryAbstractorFactory == nil {
		return sessionAbstract{}, fmt.Errorf("%s: no abstract provider is available in this build", memoryErrProviderUnavail)
	}
	provider, err := memoryAbstractorFactory(config.Abstracts)
	if err != nil {
		return sessionAbstract{}, fmt.Errorf("%s: %s", memoryErrProviderUnavail, err.Error())
	}
	kind, providerName, model := provider.Identity()
	if kind == "hosted" && !config.Abstracts.HostedEgressAllowed {
		return sessionAbstract{}, fmt.Errorf("%s: provider %s is hosted and hosted egress is not acknowledged (--allow-hosted-egress)", memoryErrProviderUnavail, providerName)
	}
	windows, ranges, truncated := abstractWindows(view)
	input := conversationAbstractInput{
		SessionRef: view.Ref, SessionDigest: sessionViewDigest(view),
		InputTruncated: truncated, Windows: windows, TotalTurns: len(view.Records), IncludedRanges: ranges,
	}
	artifact, err := provider.Abstract(input)
	if err != nil {
		return sessionAbstract{}, err
	}
	artifact.SchemaVersion = abstractSchemaVersion
	artifact.SessionRef = view.Ref
	artifact.SessionDigest = input.SessionDigest
	artifact.GeneratedAt = now
	artifact.Generator.Kind = kind
	artifact.Generator.Provider = providerName
	artifact.Generator.Model = model
	artifact.Generator.ContractVersion = 1
	artifact.InputTruncated = truncated
	artifact.Coverage.TotalTurns = len(view.Records)
	included := 0
	for _, window := range windows {
		included += len(window)
	}
	artifact.Coverage.IncludedTurns = included
	artifact.Coverage.IncludedRanges = ranges
	if err := validateSessionAbstract(artifact, view); err != nil {
		return sessionAbstract{}, fmt.Errorf("abstract validation failed (discarded): %s", err.Error())
	}
	data, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		return sessionAbstract{}, err
	}
	if err := writeBrainRelativeFileAtomic(brainDir, abstractRel(artifact.SessionDigest), append(data, '\n'), 0o600); err != nil {
		return sessionAbstract{}, err
	}
	return artifact, nil
}

// sessionAbstractStatus resolves the C4 status for one session view without
// ever making a generation call.
func sessionAbstractStatus(brainDir string, view conversationSessionView) (string, *sessionAbstract) {
	config := loadMemoryConfig(brainDir)
	digest := sessionViewDigest(view)
	if artifact, ok := loadSessionAbstract(brainDir, digest); ok {
		if !config.Abstracts.Enabled {
			return abstractStatusDisabled, nil
		}
		return abstractStatusCurrent, &artifact
	}
	if !config.Abstracts.Enabled {
		return abstractStatusDisabled, nil
	}
	// Any artifact for this session under another digest is stale.
	entries, err := os.ReadDir(filepath.Join(brainDir, filepath.FromSlash(abstractsDirRel)))
	if err == nil {
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			if artifact, ok := loadSessionAbstract(brainDir, "sha256:"+strings.TrimSuffix(entry.Name(), ".json")); ok && artifact.SessionRef == view.Ref {
				return abstractStatusStale, nil
			}
		}
	}
	if memoryAbstractorFactory == nil || strings.TrimSpace(config.Abstracts.Provider) == "" {
		return abstractStatusUnavailable, nil
	}
	return abstractStatusMissing, nil
}

// sessionRefsForSessionID computes every session reference the raw session
// id can occupy under the current manifest (one per captured branch, plus
// the degraded digest identity when derivable).
func sessionRefsForSessionID(brainDir string, manifest *exportManifest, sessionID string) map[string]bool {
	refs := map[string]bool{}
	if manifest == nil || manifest.Sources == nil || manifest.Sources.Sessions == nil {
		return refs
	}
	for _, session := range manifest.Sources.Sessions.Sessions {
		if strings.TrimSpace(session.SessionID) != sessionID {
			continue
		}
		branch := strings.TrimSpace(session.Branch)
		if branch == "" {
			branch = strings.TrimSpace(manifest.Sources.Sessions.DefaultBranch)
		}
		ref, _ := conversationSessionRef(manifest.RepoKey, branch, sessionID, "")
		refs[ref] = true
	}
	return refs
}

// purgeSessionAbstracts removes every stored abstract for the session's
// scopes (called from the shared privacy cleanup). Unreadable artifacts under
// the abstracts directory are deleted rather than retained through a privacy
// operation: an abstract is disposable by contract.
func purgeSessionAbstracts(brainDir string, manifest *exportManifest, sessionID string) error {
	refs := sessionRefsForSessionID(brainDir, manifest, sessionID)
	entries, err := os.ReadDir(filepath.Join(brainDir, filepath.FromSlash(abstractsDirRel)))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		full := filepath.Join(brainDir, filepath.FromSlash(abstractsDirRel), entry.Name())
		data, readErr := safeReadFile(full, maxManifestBytes)
		remove := false
		if readErr != nil {
			remove = true
		} else {
			var artifact sessionAbstract
			if json.Unmarshal(data, &artifact) != nil {
				remove = true
			} else if refs[artifact.SessionRef] {
				remove = true
			}
		}
		if remove {
			if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

func newMemoryConfigureCommand(opts Options) *cobra.Command {
	abstracts := &cobra.Command{
		Use:   "abstracts",
		Short: "Manage the optional session-abstract feature selection (content-free; never stores credentials)",
	}
	var enable, disable, automatic, allowEgress bool
	var provider, model string
	abstracts.Flags().BoolVar(&enable, "enable", false, "Enable session abstracts")
	abstracts.Flags().BoolVar(&disable, "disable", false, "Disable session abstracts")
	abstracts.Flags().BoolVar(&automatic, "automatic", false, "Enqueue regeneration automatically after deterministic projection")
	abstracts.Flags().StringVar(&provider, "provider", "", "Provider name (explicit; no implicit fallback)")
	abstracts.Flags().StringVar(&model, "model", "", "Model identity")
	abstracts.Flags().BoolVar(&allowEgress, "allow-hosted-egress", false, "Acknowledge that a hosted provider sends bounded session projections off-machine")
	abstracts.RunE = func(cmd *cobra.Command, args []string) error {
		if enable == disable {
			return fmt.Errorf("pass exactly one of --enable or --disable")
		}
		storage, err := resolveSessionsBrain(cmd.Context(), opts)
		if err != nil {
			return err
		}
		config := loadMemoryConfig(storage.BrainDir)
		config.Abstracts.Enabled = enable
		config.Abstracts.Automatic = automatic && enable
		if provider != "" {
			config.Abstracts.Provider = provider
		}
		if model != "" {
			config.Abstracts.Model = model
		}
		config.Abstracts.HostedEgressAllowed = allowEgress
		if err := saveMemoryConfig(storage.BrainDir, config); err != nil {
			return err
		}
		return writeJSON(cmd, config)
	}
	cmd := &cobra.Command{Use: "configure", Short: "Manage optional memory feature selection"}
	cmd.AddCommand(abstracts)
	return cmd
}

func newMemoryAbstractCommand(opts Options) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "abstract <conversation-session:id>",
		Short: "Explicitly generate or inspect the optional evidence-linked session abstract",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := strings.TrimSpace(args[0])
			if !strings.HasPrefix(ref, conversationSessionIDPrefix) {
				return fmt.Errorf("argument must be a %s id", conversationSessionIDPrefix)
			}
			storage, err := resolveSessionsBrain(cmd.Context(), opts)
			if err != nil {
				return err
			}
			brainDir := storage.BrainDir
			manifest, err := loadBrainManifest(brainDir)
			if err != nil {
				return err
			}
			if manifest.Sources == nil || manifest.Sources.History == nil {
				return fmt.Errorf("no history projection")
			}
			fresh, err := loadFreshHistory(brainDir, manifest.Sources.History)
			if err != nil {
				return err
			}
			guard := loadSessionReadGuard(brainDir, manifest)
			views := buildConversationSessionViews(fresh, manifest.RepoKey, manifest, guard)
			view, ok := views[ref]
			if !ok {
				return fmt.Errorf("session %s not found", ref)
			}
			artifact, err := generateSessionAbstract(brainDir, view, loadMemoryConfig(brainDir), opts.Now().UTC())
			if err != nil {
				return err
			}
			if jsonOut {
				return writeJSON(cmd, artifact)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "abstract generated for %s (%d/%d turns covered)\n", ref, artifact.Coverage.IncludedTurns, artifact.Coverage.TotalTurns)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}
