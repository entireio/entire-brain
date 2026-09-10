package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	distillQualityCorpusContractV1 = "distill_fully_labeled_source_corpus_v1"
	distillQualityCorpusMaxBytesV1 = 64 << 20
)

type distillQualityCorpusLabelTemplateV1 struct {
	ShouldEmit     *bool `json:"should_emit"`
	ReferenceFacts any   `json:"reference_facts"`
}

type distillQualityCorpusMembershipV1 struct {
	CandidateID string `json:"candidate_id"`
	StartLine   int    `json:"start_line"`
	EndLine     int    `json:"end_line"`
	Trigger     bool   `json:"trigger"`
}

type distillQualityCorpusTurnV1 struct {
	Contract      string                              `json:"contract"`
	SchemaVersion int                                 `json:"schema_version"`
	RecordID      string                              `json:"record_id"`
	FamilyID      string                              `json:"family_id"`
	SessionID     string                              `json:"session_id"`
	Partition     string                              `json:"partition"`
	SourceDigest  string                              `json:"source_digest"`
	TurnDigest    string                              `json:"turn_digest"`
	TurnID        string                              `json:"turn_id"`
	Role          distillTranscriptRoleV1             `json:"role"`
	Origins       []distillTranscriptOriginV1         `json:"origins"`
	SourceLines   []int                               `json:"source_lines"`
	StartLine     int                                 `json:"start_line"`
	EndLine       int                                 `json:"end_line"`
	DirectUser    bool                                `json:"direct_user"`
	Text          string                              `json:"text"`
	Candidates    []distillQualityCorpusMembershipV1  `json:"candidates"`
	Labels        distillQualityCorpusLabelTemplateV1 `json:"labels"`
}

type distillQualityCorpusPrivateSourceV1 struct {
	SessionID       string `json:"session_id"`
	FamilyID        string `json:"family_id"`
	SourceSessionID string `json:"source_session_id"`
	Branch          string `json:"branch"`
	TranscriptPath  string `json:"transcript_path"`
	SourceDigest    string `json:"source_digest"`
}
type distillQualityCorpusManifestV1 struct {
	Contract                        string `json:"contract"`
	SchemaVersion                   int    `json:"schema_version"`
	GeneratedAt                     string `json:"generated_at"`
	Partition                       string `json:"partition"`
	SplitSeed                       string `json:"split_seed"`
	PriorDevelopmentInventoryDigest string `json:"prior_development_inventory_digest,omitempty"`
	CandidateSchemaVersion          int    `json:"candidate_schema_version"`
	CandidateRulesVersion           string `json:"candidate_rules_version"`
	RedactionVersion                string `json:"redaction_version"`
	PartitionMethod                 string `json:"partition_method"`
	ConfirmationExclusion           string `json:"confirmation_exclusion"`
	LabelStatus                     string `json:"label_status"`
	ReferenceLabelsComplete         bool   `json:"reference_labels_complete"`
	Sessions                        int    `json:"sessions"`
	ZeroCandidateSessions           int    `json:"zero_candidate_sessions"`
	Turns                           int    `json:"turns"`
	ExcludedContaminatedFamilies    int    `json:"excluded_contaminated_families"`
	CorpusDigest                    string `json:"corpus_digest"`
	CorpusPath                      string `json:"corpus_path"`
	PrivateSourceMapPath            string `json:"private_source_map_path"`
	FamilyInventoryPath             string `json:"family_inventory_path"`
}
type distillQualityCorpusInventoryV1 struct {
	Contract      string   `json:"contract"`
	SchemaVersion int      `json:"schema_version"`
	FamilyDigests []string `json:"family_digests"`
	SourceDigests []string `json:"source_digests"`
}

func buildDistillQualityCorpusV1(ctx context.Context, opts Options, target, outDir, partition, splitSeed, inventoryPath, branch, session string, now time.Time) (distillQualityCorpusManifestV1, error) {
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return distillQualityCorpusManifestV1{}, err
	}
	if !local {
		return distillQualityCorpusManifestV1{}, errors.New("distill-quality corpus requires a local repository")
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return distillQualityCorpusManifestV1{}, err
	}
	outDir, err = validateFreshDistillQualityOutputDirV1(outDir, repoDir, storage.BrainDir)
	if err != nil {
		return distillQualityCorpusManifestV1{}, err
	}
	privacyUnlock, err := acquireBrainPrivacySideEffectLock(storage.BrainDir)
	if err != nil {
		return distillQualityCorpusManifestV1{}, err
	}
	defer privacyUnlock()
	partition = strings.TrimSpace(partition)
	if partition == "" {
		partition = "development"
	}
	if partition != "development" && partition != "confirmation" {
		return distillQualityCorpusManifestV1{}, errors.New("--partition must be development or confirmation")
	}
	if strings.TrimSpace(splitSeed) == "" {
		return distillQualityCorpusManifestV1{}, errors.New("--split-seed is required")
	}
	contaminated := map[string]bool{}
	contaminatedSources := map[string]bool{}
	inventoryDigest := ""
	if partition == "confirmation" {
		if inventoryPath == "" {
			return distillQualityCorpusManifestV1{}, errors.New("confirmation requires --prior-development-inventory")
		}
		data, err := readDistillPhaseGateRegularFileV1(inventoryPath)
		if err != nil {
			return distillQualityCorpusManifestV1{}, err
		}
		inventoryDigest = distillCandidateCacheDigestV2(string(data))
		var inv distillQualityCorpusInventoryV1
		if err := decodeDistillQualityCorpusStrictV1(data, &inv); err != nil {
			return distillQualityCorpusManifestV1{}, err
		}
		if inv.Contract != "distill_development_family_inventory_v1" || inv.SchemaVersion != 1 {
			return distillQualityCorpusManifestV1{}, errors.New("invalid development family inventory")
		}
		if len(inv.FamilyDigests)+len(inv.SourceDigests) == 0 {
			return distillQualityCorpusManifestV1{}, errors.New("development inventory contains no family or source digests")
		}
		for _, digest := range inv.FamilyDigests {
			if !validSHA256Identity(digest) || contaminated[digest] {
				return distillQualityCorpusManifestV1{}, errors.New("invalid development family digest inventory")
			}
			contaminated[digest] = true
		}
		for _, digest := range inv.SourceDigests {
			if !validSHA256Identity(digest) || contaminatedSources[digest] {
				return distillQualityCorpusManifestV1{}, errors.New("invalid development source digest inventory")
			}
			contaminatedSources[digest] = true
		}
	}
	manifest, err := loadBrainManifest(storage.BrainDir)
	if err != nil {
		return distillQualityCorpusManifestV1{}, err
	}
	if manifest.Sources == nil || manifest.Sources.Sessions == nil {
		return distillQualityCorpusManifestV1{}, errors.New("no exported sessions; run `entire brain refresh` first")
	}
	selection := distillCommandOptions{pipeline: distillPipelineCandidates, branch: strings.TrimSpace(branch), session: strings.TrimSpace(session)}
	sessions, err := filterTombstonedSessions(storage.BrainDir, append([]exportSession(nil), manifest.Sources.Sessions.Sessions...))
	if err != nil {
		return distillQualityCorpusManifestV1{}, err
	}
	sessions, err = coalesceCandidateDistillSessions(ctx, storage.BrainDir, sessions, func(s exportSession) string { return resolveDistillBranch(manifest, s) }, func(s exportSession) bool { return distillSessionSelected(manifest, s, selection) })
	if err != nil {
		return distillQualityCorpusManifestV1{}, err
	}
	selected := sessions[:0]
	for _, s := range sessions {
		if distillSessionSelected(manifest, s, selection) {
			selected = append(selected, s)
		}
	}
	sessions = selected
	sortDistillSessionsDeterministically(sessions, func(s exportSession) string { return resolveDistillBranch(manifest, s) })
	var public, private bytes.Buffer
	var familyDigests, sourceDigests []string
	corpusHash := sha256.New()
	result := distillQualityCorpusManifestV1{Contract: distillQualityCorpusContractV1, SchemaVersion: 1, GeneratedAt: now.UTC().Format(time.RFC3339Nano), Partition: partition, SplitSeed: splitSeed, PriorDevelopmentInventoryDigest: inventoryDigest, CandidateSchemaVersion: distillCandidateSchemaVersion, CandidateRulesVersion: "candidate-rules-v1", RedactionVersion: distillCandidateRedactionVersionV2, PartitionMethod: "operator_selected_scope_v1", ConfirmationExclusion: "known_family_and_exact_source_digest_v1", LabelStatus: "incomplete_unset_template", ReferenceLabelsComplete: false}
	for _, session := range sessions {
		data, err := readCanonicalHistoryTranscriptBounded(ctx, storage.BrainDir, session.TranscriptPath, distillCandidateMaxRawBytes)
		if err != nil {
			return distillQualityCorpusManifestV1{}, err
		}
		branch := resolveDistillBranch(manifest, session)
		normalized := normalizeDistillTranscriptV1(session, branch, string(data))
		if normalized.Overflow || normalized.Unsupported || !normalized.Recognized {
			return distillQualityCorpusManifestV1{}, fmt.Errorf("corpus cannot completely normalize session %q", session.SessionID)
		}
		cards, overflow := selectDistillCandidateCardsLimitedV1(normalized, distillCandidateMaxCards)
		if overflow {
			return distillQualityCorpusManifestV1{}, fmt.Errorf("corpus session %q exceeds candidate bound", session.SessionID)
		}
		familyDigest := distillCandidateCacheDigestV2("family-v1\x00" + strings.TrimSpace(session.SessionID))
		sourceDigest := distillCandidateCacheDigestV2(string(data))
		if partition == "confirmation" && (contaminated[familyDigest] || contaminatedSources[sourceDigest]) {
			result.ExcludedContaminatedFamilies++
			continue
		}
		familyDigests = append(familyDigests, familyDigest)
		sourceDigests = append(sourceDigests, sourceDigest)
		familyID := "family-" + strings.TrimPrefix(familyDigest, "sha256:")[:24]
		sessionID := "session-" + strings.TrimPrefix(distillCandidateCacheDigestV2("session-v1\x00"+session.SessionID), "sha256:")[:24]
		members := map[string][]distillQualityCorpusMembershipV1{}
		for _, card := range cards {
			triggers := map[string]bool{}
			for _, trigger := range card.Triggers {
				triggers[trigger.TurnID] = true
			}
			for _, turn := range card.Turns {
				members[turn.ID] = append(members[turn.ID], distillQualityCorpusMembershipV1{CandidateID: card.ID, StartLine: card.StartLine, EndLine: card.EndLine, Trigger: triggers[turn.ID]})
			}
		}
		if len(cards) == 0 {
			result.ZeroCandidateSessions++
		}
		source := distillQualityCorpusPrivateSourceV1{SessionID: sessionID, FamilyID: familyID, SourceSessionID: session.SessionID, Branch: branch, TranscriptPath: filepath.ToSlash(session.TranscriptPath), SourceDigest: sourceDigest}
		if err := appendDistillQualityCorpusJSONLineV1(&private, source); err != nil {
			return distillQualityCorpusManifestV1{}, err
		}
		result.Sessions++
		for _, turn := range normalized.Turns {
			text := redactText(turn.Text)
			turnDigest := distillCandidateCacheDigestV2(text)
			row := distillQualityCorpusTurnV1{Contract: distillQualityCorpusContractV1, SchemaVersion: 1, RecordID: "turn-" + strings.TrimPrefix(distillCandidateCacheDigestV2(familyDigest+"\x00"+turn.ID), "sha256:")[:24], FamilyID: familyID, SessionID: sessionID, Partition: partition, SourceDigest: sourceDigest, TurnDigest: turnDigest, TurnID: turn.ID, Role: turn.Role, Origins: append([]distillTranscriptOriginV1(nil), turn.Origins...), SourceLines: append([]int(nil), turn.SourceLines...), StartLine: turn.StartLine, EndLine: turn.EndLine, DirectUser: turn.DirectUser, Text: text, Candidates: append([]distillQualityCorpusMembershipV1(nil), members[turn.ID]...), Labels: distillQualityCorpusLabelTemplateV1{ShouldEmit: nil, ReferenceFacts: nil}}
			sort.Slice(row.Candidates, func(i, j int) bool { return row.Candidates[i].CandidateID < row.Candidates[j].CandidateID })
			line, err := json.Marshal(row)
			if err != nil {
				return distillQualityCorpusManifestV1{}, err
			}
			corpusHash.Write(line)
			corpusHash.Write([]byte{'\n'})
			public.Write(line)
			public.WriteByte('\n')
			result.Turns++
			if public.Len() > distillQualityCorpusMaxBytesV1 || private.Len() > distillQualityCorpusMaxBytesV1 {
				return distillQualityCorpusManifestV1{}, errors.New("corpus output exceeds size bound")
			}
		}
	}
	if result.Sessions == 0 || result.Turns == 0 {
		return distillQualityCorpusManifestV1{}, errors.New("corpus scope produced no eligible visible turns")
	}
	result.CorpusDigest = "sha256:" + hex.EncodeToString(corpusHash.Sum(nil))
	result.CorpusPath = filepath.Join(outDir, "corpus.jsonl")
	result.PrivateSourceMapPath = filepath.Join(outDir, "private-source-map.jsonl")
	result.FamilyInventoryPath = filepath.Join(outDir, "family-inventory.json")
	if err := os.Mkdir(outDir, 0o700); err != nil {
		return distillQualityCorpusManifestV1{}, err
	}
	if err := writeFileAtomic(result.CorpusPath, public.Bytes(), 0o600); err != nil {
		return distillQualityCorpusManifestV1{}, err
	}
	if err := writeFileAtomic(result.PrivateSourceMapPath, private.Bytes(), 0o600); err != nil {
		return distillQualityCorpusManifestV1{}, err
	}
	sort.Strings(familyDigests)
	sort.Strings(sourceDigests)
	inventory := distillQualityCorpusInventoryV1{Contract: "distill_development_family_inventory_v1", SchemaVersion: 1, FamilyDigests: familyDigests, SourceDigests: sourceDigests}
	inventoryData, _ := json.MarshalIndent(inventory, "", "  ")
	if err := writeFileAtomic(result.FamilyInventoryPath, append(inventoryData, '\n'), 0o600); err != nil {
		return distillQualityCorpusManifestV1{}, err
	}
	manifestData, _ := json.MarshalIndent(result, "", "  ")
	if err := writeFileAtomic(filepath.Join(outDir, "manifest.json"), append(manifestData, '\n'), 0o600); err != nil {
		return distillQualityCorpusManifestV1{}, err
	}
	return result, nil
}

func newFactsDistillQualityCorpusCommand(opts Options) *cobra.Command {
	var out, partition, seed, prior, branch, session string
	var jsonOut bool
	cmd := &cobra.Command{Use: "corpus [path]", Short: "Export selected-session source evidence with unset labels", Long: "Export every eligible visible turn in the selected sessions, including zero-candidate sessions. Labels remain explicitly unset and incomplete; this command does not create reference facts or quality-gate evidence.", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		target := "."
		if opts.Env.RepoRoot != "" {
			target = opts.Env.RepoRoot
		}
		if len(args) == 1 {
			target = args[0]
		}
		result, err := buildDistillQualityCorpusV1(cmd.Context(), opts, target, out, partition, seed, prior, branch, session, opts.Now())
		if err != nil {
			return err
		}
		if jsonOut {
			return writeJSON(cmd, result)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "exported %d visible turns from %d sessions (%d zero-candidate) to %s\n", result.Turns, result.Sessions, result.ZeroCandidateSessions, result.CorpusPath)
		return nil
	}}
	cmd.Flags().StringVar(&out, "out", "", "Fresh output directory outside repository and Brain (required)")
	cmd.Flags().StringVar(&partition, "partition", "development", "Corpus partition: development or confirmation")
	cmd.Flags().StringVar(&seed, "split-seed", "", "Pinned partition seed (required)")
	cmd.Flags().StringVar(&prior, "prior-development-inventory", "", "Development family inventory required for confirmation")
	cmd.Flags().StringVar(&branch, "branch", "", "Limit to one resolved branch")
	cmd.Flags().StringVar(&session, "session", "", "Limit to one session id")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit summary as JSON")
	_ = cmd.MarkFlagRequired("out")
	_ = cmd.MarkFlagRequired("split-seed")
	return cmd
}

func appendDistillQualityCorpusJSONLineV1(out *bytes.Buffer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	out.Write(data)
	out.WriteByte('\n')
	return nil
}
func decodeDistillQualityCorpusStrictV1(data []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("trailing development inventory data")
	}
	return nil
}
