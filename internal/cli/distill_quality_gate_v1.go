package cli

import (
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
)

const (
	distillPhaseGateEvidenceContractV1 = "phase_gate_evidence_v1"
	distillPhaseGateAstraContractV1    = "phase_gate_astra_judgment_v1"
	distillPhaseGateArchiveContractV1  = "phase_gate_archive_v1"
	distillPhaseGateMaxInputBytesV1    = 8 << 20
)

type distillPhaseGateEvidenceV1 struct {
	Contract        string                       `json:"contract"`
	SchemaVersion   int                          `json:"schema_version"`
	Phase           int                          `json:"phase"`
	RevisionDigest  string                       `json:"revision_digest"`
	ArtifactDigest  string                       `json:"artifact_digest"`
	BaseArtifacts   []distillPhaseGateArtifactV1 `json:"base_artifacts"`
	JudgeArtifacts  []distillPhaseGateArtifactV1 `json:"judge_artifacts"`
	RubricDigest    string                       `json:"rubric_digest"`
	InputDigest     string                       `json:"input_digest"`
	EvidenceDigest  string                       `json:"evidence_digest"`
	PrimaryVerdicts []distillPhaseGateVerdictV1  `json:"primary_verdicts"`
	PrimaryCoverage []distillPhaseGateCoverageV1 `json:"primary_coverage"`
}

type distillPhaseGateCoverageV1 struct {
	Judge        string `json:"judge"`
	Status       string `json:"status"`
	DetailDigest string `json:"detail_digest,omitempty"`
}

type distillPhaseGateArtifactV1 struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Digest string `json:"digest"`
}

type distillPhaseGateVerdictV1 struct {
	Judge           string                        `json:"judge"`
	Model           string                        `json:"model"`
	EvidenceDigest  string                        `json:"evidence_digest"`
	Verdict         string                        `json:"verdict"`
	Critical        bool                          `json:"critical,omitempty"`
	Dimensions      []distillPhaseGateDimensionV1 `json:"dimensions"`
	PromptDigest    string                        `json:"prompt_digest"`
	PayloadDigest   string                        `json:"payload_digest"`
	TelemetryDigest string                        `json:"telemetry_digest"`
	RationaleDigest string                        `json:"rationale_digest"`
	VerdictDigest   string                        `json:"verdict_digest"`
}

type distillPhaseGateDimensionV1 struct {
	Name    string `json:"name"`
	Verdict string `json:"verdict"`
}

type distillPhaseGateAstraV1 struct {
	Contract        string                       `json:"contract"`
	SchemaVersion   int                          `json:"schema_version"`
	Model           string                       `json:"model"`
	EvidenceDigest  string                       `json:"evidence_digest"`
	PrimariesDigest string                       `json:"primaries_digest"`
	Verdict         string                       `json:"verdict"`
	PromptDigest    string                       `json:"prompt_digest"`
	PayloadDigest   string                       `json:"payload_digest"`
	TelemetryDigest string                       `json:"telemetry_digest"`
	RationaleDigest string                       `json:"rationale_digest"`
	Artifacts       []distillPhaseGateArtifactV1 `json:"artifacts"`
	JudgmentDigest  string                       `json:"judgment_digest"`
}

type distillPhaseGateSummaryV1 struct {
	Phase               int      `json:"phase"`
	State               string   `json:"state"`
	Reason              string   `json:"reason"`
	EvidenceDigest      string   `json:"evidence_digest"`
	PrimariesDigest     string   `json:"primaries_digest"`
	ValidPrimaries      int      `json:"valid_primaries"`
	MissingPrimaries    int      `json:"missing_primaries"`
	MissingJudges       []string `json:"missing_judges,omitempty"`
	InvalidJudges       []string `json:"invalid_judges,omitempty"`
	UnavailableJudges   []string `json:"unavailable_judges,omitempty"`
	CriticalDissent     bool     `json:"critical_dissent"`
	AstraVerdict        string   `json:"astra_verdict,omitempty"`
	AstraJudgmentDigest string   `json:"astra_judgment_digest,omitempty"`
	AggregateDigest     string   `json:"aggregate_digest"`
	ArchivePath         string   `json:"archive_path"`
}

type distillPhaseGateArchiveV1 struct {
	Contract      string                     `json:"contract"`
	SchemaVersion int                        `json:"schema_version"`
	Evidence      distillPhaseGateEvidenceV1 `json:"evidence"`
	Astra         *distillPhaseGateAstraV1   `json:"astra,omitempty"`
	Summary       distillPhaseGateSummaryV1  `json:"summary"`
}

func resolveDistillPhaseGateV1(evidencePath, astraPath, outDir string) (distillPhaseGateSummaryV1, error) {
	if strings.TrimSpace(evidencePath) == "" || strings.TrimSpace(outDir) == "" {
		return distillPhaseGateSummaryV1{}, errors.New("phase gate resolve: --evidence and --out are required")
	}
	if _, err := os.Stat(outDir); err == nil || !os.IsNotExist(err) {
		return distillPhaseGateSummaryV1{}, errors.New("phase gate resolve: --out must be a fresh directory")
	}
	var evidence distillPhaseGateEvidenceV1
	if err := readStrictDistillPhaseGateJSONV1(evidencePath, &evidence); err != nil {
		return distillPhaseGateSummaryV1{}, err
	}
	if err := validateDistillPhaseGateEvidenceV1(evidence); err != nil {
		return distillPhaseGateSummaryV1{}, err
	}
	artifactDigests, err := verifyDistillPhaseGateArtifactsV1(evidence)
	if err != nil {
		return distillPhaseGateSummaryV1{}, err
	}
	for _, verdict := range evidence.PrimaryVerdicts {
		if err := verifyDistillPhaseGateVerdictArtifactsV1(verdict, artifactDigests); err != nil {
			return distillPhaseGateSummaryV1{}, err
		}
	}
	for _, record := range evidence.PrimaryCoverage {
		if record.Status != "completed" && artifactDigests["judge/"+record.Judge+"/status"] != record.DetailDigest {
			return distillPhaseGateSummaryV1{}, fmt.Errorf("phase gate evidence: %s coverage status is not bound to a verified artifact", record.Judge)
		}
	}
	canonicalizeDistillPhaseGateVerdictsV1(evidence.PrimaryVerdicts)
	primariesDigest := distillPhaseGatePrimariesDigestV1(evidence)
	state, reason, critical := resolveDistillPrimaryPanelV1(evidence.PrimaryVerdicts)
	var astra *distillPhaseGateAstraV1
	if state == "escalation_required" && astraPath != "" {
		var judgment distillPhaseGateAstraV1
		if err := readStrictDistillPhaseGateJSONV1(astraPath, &judgment); err != nil {
			return distillPhaseGateSummaryV1{}, err
		}
		if err := validateDistillPhaseGateAstraV1(judgment, evidence.EvidenceDigest, primariesDigest); err != nil {
			return distillPhaseGateSummaryV1{}, err
		}
		if err := verifyDistillPhaseGateAstraArtifactsV1(judgment); err != nil {
			return distillPhaseGateSummaryV1{}, err
		}
		astra = &judgment
		state = judgment.Verdict
		reason = "resolved by digest-bound Astra escalation"
		if state == "insufficient_evidence" {
			state = "open"
			reason = "Astra reported insufficient evidence"
		}
	}
	valid := len(evidence.PrimaryVerdicts)
	present := map[string]bool{}
	for _, verdict := range evidence.PrimaryVerdicts {
		present[verdict.Judge] = true
	}
	missingJudges := make([]string, 0, 3-valid)
	for _, judge := range []string{"claude", "copilot", "cursor"} {
		if !present[judge] {
			missingJudges = append(missingJudges, judge)
		}
	}
	summary := distillPhaseGateSummaryV1{Phase: evidence.Phase, State: state, Reason: reason, EvidenceDigest: evidence.EvidenceDigest, PrimariesDigest: primariesDigest, ValidPrimaries: valid, MissingPrimaries: 3 - valid, CriticalDissent: critical}
	summary.MissingJudges = missingJudges
	for _, record := range evidence.PrimaryCoverage {
		if record.Status == "invalid" {
			summary.InvalidJudges = append(summary.InvalidJudges, record.Judge)
		}
		if record.Status == "unavailable" {
			summary.UnavailableJudges = append(summary.UnavailableJudges, record.Judge)
		}
	}
	sort.Strings(summary.InvalidJudges)
	sort.Strings(summary.UnavailableJudges)
	if astra != nil {
		summary.AstraVerdict = astra.Verdict
		summary.AstraJudgmentDigest = astra.JudgmentDigest
	}
	aggregateInput := struct {
		Evidence  string `json:"evidence_digest"`
		Primaries string `json:"primaries_digest"`
		State     string `json:"state"`
		Astra     string `json:"astra_digest,omitempty"`
	}{evidence.EvidenceDigest, primariesDigest, summary.State, summary.AstraJudgmentDigest}
	summary.AggregateDigest, _ = distillPhaseGateDigestV1(aggregateInput)
	summary.ArchivePath = filepath.Join(outDir, "archive.json")
	archiveEvidence := evidence
	archiveEvidence.BaseArtifacts = redactDistillPhaseGateArtifactPathsV1(evidence.BaseArtifacts)
	archiveEvidence.JudgeArtifacts = redactDistillPhaseGateArtifactPathsV1(evidence.JudgeArtifacts)
	if astra != nil {
		copyAstra := *astra
		copyAstra.Artifacts = redactDistillPhaseGateArtifactPathsV1(astra.Artifacts)
		astra = &copyAstra
	}
	archive := distillPhaseGateArchiveV1{Contract: distillPhaseGateArchiveContractV1, SchemaVersion: 1, Evidence: archiveEvidence, Astra: astra, Summary: summary}
	if err := os.Mkdir(outDir, 0o700); err != nil {
		return distillPhaseGateSummaryV1{}, err
	}
	if err := writeDistillPhaseGateJSONV1(filepath.Join(outDir, "summary.json"), summary); err != nil {
		return distillPhaseGateSummaryV1{}, err
	}
	if err := writeDistillPhaseGateJSONV1(summary.ArchivePath, archive); err != nil {
		return distillPhaseGateSummaryV1{}, err
	}
	return summary, nil
}

func validateDistillPhaseGateEvidenceV1(e distillPhaseGateEvidenceV1) error {
	if e.Contract != distillPhaseGateEvidenceContractV1 || e.SchemaVersion != 1 || e.Phase < 0 || e.Phase > 4 {
		return errors.New("phase gate evidence: invalid contract, schema, or phase")
	}
	for name, digest := range map[string]string{"revision": e.RevisionDigest, "artifact": e.ArtifactDigest, "rubric": e.RubricDigest, "input": e.InputDigest, "evidence": e.EvidenceDigest} {
		if !validSHA256Identity(digest) {
			return fmt.Errorf("phase gate evidence: invalid %s digest", name)
		}
	}
	if len(e.BaseArtifacts) == 0 {
		return errors.New("phase gate evidence: no verifiable artifacts")
	}
	core := struct {
		Contract       string `json:"contract"`
		SchemaVersion  int    `json:"schema_version"`
		Phase          int    `json:"phase"`
		RevisionDigest string `json:"revision_digest"`
		ArtifactDigest string `json:"artifact_digest"`
		RubricDigest   string `json:"rubric_digest"`
		InputDigest    string `json:"input_digest"`
	}{e.Contract, e.SchemaVersion, e.Phase, e.RevisionDigest, e.ArtifactDigest, e.RubricDigest, e.InputDigest}
	want, _ := distillPhaseGateDigestV1(core)
	if e.EvidenceDigest != want {
		return errors.New("phase gate evidence: evidence digest mismatch")
	}
	seen := map[string]bool{}
	for i := range e.PrimaryVerdicts {
		if err := validateDistillPhaseGateVerdictV1(e.PrimaryVerdicts[i], e.EvidenceDigest); err != nil {
			return fmt.Errorf("phase gate evidence: primary %d: %w", i+1, err)
		}
		if seen[e.PrimaryVerdicts[i].Judge] {
			return errors.New("phase gate evidence: duplicate primary judge")
		}
		seen[e.PrimaryVerdicts[i].Judge] = true
	}
	coverage := map[string]string{}
	for _, record := range e.PrimaryCoverage {
		if record.Judge != "claude" && record.Judge != "copilot" && record.Judge != "cursor" {
			return errors.New("phase gate evidence: invalid coverage judge")
		}
		if coverage[record.Judge] != "" || (record.Status != "completed" && record.Status != "invalid" && record.Status != "unavailable") {
			return errors.New("phase gate evidence: invalid primary coverage")
		}
		if record.Status == "completed" {
			if !seen[record.Judge] || record.DetailDigest != "" {
				return errors.New("phase gate evidence: completed coverage has no matching verdict")
			}
		} else {
			if seen[record.Judge] || !validSHA256Identity(record.DetailDigest) {
				return errors.New("phase gate evidence: invalid/unavailable coverage is not explicitly evidenced")
			}
		}
		coverage[record.Judge] = record.Status
	}
	if len(coverage) != 3 {
		return errors.New("phase gate evidence: coverage must account for all three primary judges")
	}
	return nil
}

func verifyDistillPhaseGateArtifactsV1(e distillPhaseGateEvidenceV1) (map[string]string, error) {
	base, err := verifyDistillPhaseGateArtifactListV1(e.BaseArtifacts)
	if err != nil {
		return nil, err
	}
	for name := range base {
		if strings.HasPrefix(name, "judge/") || strings.HasPrefix(name, "astra/") {
			return nil, errors.New("phase gate evidence: later-phase artifact mixed into base artifacts")
		}
	}
	judge, err := verifyDistillPhaseGateArtifactListV1(e.JudgeArtifacts)
	if err != nil {
		return nil, err
	}
	for name, digest := range judge {
		parts := strings.Split(name, "/")
		if len(parts) != 3 || parts[0] != "judge" || (parts[1] != "claude" && parts[1] != "copilot" && parts[1] != "cursor") || (parts[2] != "prompt" && parts[2] != "payload" && parts[2] != "telemetry" && parts[2] != "rationale" && parts[2] != "status") {
			return nil, errors.New("phase gate evidence: invalid judge artifact name")
		}
		if _, exists := base[name]; exists {
			return nil, errors.New("phase gate evidence: duplicate base/judge artifact name")
		}
		base[name] = digest
	}
	for name, digest := range map[string]string{"revision": e.RevisionDigest, "rubric": e.RubricDigest, "input": e.InputDigest, "evaluation": e.ArtifactDigest} {
		if base[name] != digest {
			return nil, fmt.Errorf("phase gate evidence: %s digest is not bound to a verified base artifact", name)
		}
	}
	return base, nil
}

func verifyDistillPhaseGateArtifactListV1(input []distillPhaseGateArtifactV1) (map[string]string, error) {
	artifacts := append([]distillPhaseGateArtifactV1(nil), input...)
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Name < artifacts[j].Name })
	seen := map[string]bool{}
	bound := map[string]string{}
	for i := range artifacts {
		a := &artifacts[i]
		if strings.TrimSpace(a.Name) == "" || strings.TrimSpace(a.Path) == "" || seen[a.Name] || !validSHA256Identity(a.Digest) {
			return nil, errors.New("phase gate evidence: invalid artifact reference")
		}
		seen[a.Name] = true
		bound[a.Name] = a.Digest
		data, err := readDistillPhaseGateRegularFileV1(a.Path)
		if err != nil {
			return nil, fmt.Errorf("phase gate artifact %q: %w", a.Name, err)
		}
		sum := sha256.Sum256(data)
		if "sha256:"+hex.EncodeToString(sum[:]) != a.Digest {
			return nil, fmt.Errorf("phase gate artifact %q digest mismatch", a.Name)
		}
		a.Path = ""
	}
	return bound, nil
}

func verifyDistillPhaseGateVerdictArtifactsV1(v distillPhaseGateVerdictV1, artifacts map[string]string) error {
	for suffix, digest := range map[string]string{"prompt": v.PromptDigest, "payload": v.PayloadDigest, "telemetry": v.TelemetryDigest, "rationale": v.RationaleDigest} {
		if artifacts["judge/"+v.Judge+"/"+suffix] != digest {
			return fmt.Errorf("phase gate evidence: %s %s digest is not bound to a verified artifact", v.Judge, suffix)
		}
	}
	return nil
}

func verifyDistillPhaseGateAstraArtifactsV1(a distillPhaseGateAstraV1) error {
	artifacts, err := verifyDistillPhaseGateArtifactListV1(a.Artifacts)
	if err != nil {
		return err
	}
	if len(artifacts) != 4 {
		return errors.New("phase gate evidence: Astra must provide exactly four artifacts")
	}
	for suffix, digest := range map[string]string{"prompt": a.PromptDigest, "payload": a.PayloadDigest, "telemetry": a.TelemetryDigest, "rationale": a.RationaleDigest} {
		if artifacts["astra/"+suffix] != digest {
			return fmt.Errorf("phase gate evidence: Astra %s digest is not bound to a verified artifact", suffix)
		}
	}
	return nil
}

func redactDistillPhaseGateArtifactPathsV1(input []distillPhaseGateArtifactV1) []distillPhaseGateArtifactV1 {
	out := append([]distillPhaseGateArtifactV1(nil), input...)
	for i := range out {
		out[i].Path = ""
	}
	return out
}

func validateDistillPhaseGateVerdictV1(v distillPhaseGateVerdictV1, evidenceDigest string) error {
	if v.Judge != "claude" && v.Judge != "copilot" && v.Judge != "cursor" {
		return errors.New("invalid judge")
	}
	if strings.TrimSpace(v.Model) == "" || v.EvidenceDigest != evidenceDigest || !validPhaseGateVerdictV1(v.Verdict) {
		return errors.New("invalid model, evidence binding, or verdict")
	}
	for _, d := range []string{v.PromptDigest, v.PayloadDigest, v.TelemetryDigest, v.RationaleDigest, v.VerdictDigest} {
		if !validSHA256Identity(d) {
			return errors.New("invalid bound digest")
		}
	}
	dims := map[string]string{}
	for _, d := range v.Dimensions {
		if !validPhaseGateVerdictV1(d.Verdict) || dims[d.Name] != "" {
			return errors.New("invalid dimension")
		}
		dims[d.Name] = d.Verdict
	}
	for _, name := range []string{"performance", "quality", "progress"} {
		if dims[name] == "" {
			return fmt.Errorf("missing %s dimension", name)
		}
	}
	if len(v.Dimensions) != 3 {
		return errors.New("expected exactly performance, quality, and progress dimensions")
	}
	if v.Verdict == "pass" {
		for _, d := range v.Dimensions {
			if d.Verdict != "pass" {
				return errors.New("overall pass conflicts with a non-pass dimension")
			}
		}
	}
	if v.Verdict == "fail" {
		failed := false
		for _, d := range v.Dimensions {
			failed = failed || d.Verdict == "fail"
		}
		if !failed {
			return errors.New("overall fail has no failing dimension")
		}
	}
	if v.Verdict == "insufficient_evidence" {
		insufficient := false
		for _, d := range v.Dimensions {
			insufficient = insufficient || d.Verdict == "insufficient_evidence"
		}
		if !insufficient {
			return errors.New("overall insufficient evidence has no matching dimension")
		}
	}
	canonical := v
	canonical.VerdictDigest = ""
	want, _ := distillPhaseGateDigestV1(canonical)
	if v.VerdictDigest != want {
		return errors.New("verdict digest mismatch")
	}
	return nil
}

func resolveDistillPrimaryPanelV1(verdicts []distillPhaseGateVerdictV1) (string, string, bool) {
	if len(verdicts) < 2 {
		return "fail", "fewer than two valid primary verdicts", false
	}
	counts := map[string]int{}
	critical := false
	for _, v := range verdicts {
		counts[v.Verdict]++
		critical = critical || v.Critical
	}
	if critical {
		return "escalation_required", "critical primary dissent requires Astra", true
	}
	for _, verdict := range []string{"pass", "fail", "insufficient_evidence"} {
		if counts[verdict] >= 2 {
			if verdict == "insufficient_evidence" {
				return "fail", "primary majority reported insufficient evidence", false
			}
			return verdict, "decisive primary majority", false
		}
	}
	return "escalation_required", "primary panel has no majority", false
}

func validateDistillPhaseGateAstraV1(a distillPhaseGateAstraV1, evidence, primaries string) error {
	if a.Contract != distillPhaseGateAstraContractV1 || a.SchemaVersion != 1 || strings.TrimSpace(a.Model) == "" || a.EvidenceDigest != evidence || a.PrimariesDigest != primaries || !validPhaseGateVerdictV1(a.Verdict) {
		return errors.New("phase gate Astra judgment: invalid contract or digest binding")
	}
	for _, digest := range []string{a.PromptDigest, a.PayloadDigest, a.TelemetryDigest, a.RationaleDigest, a.JudgmentDigest} {
		if !validSHA256Identity(digest) {
			return errors.New("phase gate Astra judgment: invalid bound digest")
		}
	}
	canonical := a
	canonical.JudgmentDigest = ""
	canonical.Artifacts = redactDistillPhaseGateArtifactPathsV1(canonical.Artifacts)
	want, _ := distillPhaseGateDigestV1(canonical)
	if a.JudgmentDigest != want {
		return errors.New("phase gate Astra judgment: judgment digest mismatch")
	}
	return nil
}

func validPhaseGateVerdictV1(v string) bool {
	return v == "pass" || v == "fail" || v == "insufficient_evidence"
}
func distillPhaseGateDigestV1(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
func readStrictDistillPhaseGateJSONV1(path string, dst any) error {
	data, err := readDistillPhaseGateRegularFileV1(path)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("phase gate JSON has trailing value")
		}
		return err
	}
	return nil
}
func readDistillPhaseGateRegularFileV1(path string) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, errors.New("phase gate input must be a regular file")
	}
	if before.Size() > distillPhaseGateMaxInputBytesV1 {
		return nil, errors.New("phase gate input exceeds size limit")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return nil, errors.New("phase gate input changed during open")
	}
	data, err := io.ReadAll(io.LimitReader(f, distillPhaseGateMaxInputBytesV1+1))
	if err != nil {
		return nil, err
	}
	if len(data) > distillPhaseGateMaxInputBytesV1 {
		return nil, errors.New("phase gate input exceeds size limit")
	}
	return data, nil
}
func writeDistillPhaseGateJSONV1(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeFileAtomic(path, data, 0o600)
}

func canonicalizeDistillPhaseGateVerdictsV1(v []distillPhaseGateVerdictV1) {
	sort.Slice(v, func(i, j int) bool { return v[i].Judge < v[j].Judge })
}
func distillPhaseGatePrimariesDigestV1(e distillPhaseGateEvidenceV1) string {
	verdicts := append([]distillPhaseGateVerdictV1(nil), e.PrimaryVerdicts...)
	canonicalizeDistillPhaseGateVerdictsV1(verdicts)
	coverage := append([]distillPhaseGateCoverageV1(nil), e.PrimaryCoverage...)
	sort.Slice(coverage, func(i, j int) bool { return coverage[i].Judge < coverage[j].Judge })
	digest, _ := distillPhaseGateDigestV1(struct {
		Verdicts []distillPhaseGateVerdictV1  `json:"verdicts"`
		Coverage []distillPhaseGateCoverageV1 `json:"coverage"`
	}{verdicts, coverage})
	return digest
}
