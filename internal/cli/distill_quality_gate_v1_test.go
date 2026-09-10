package cli

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestResolveDistillPhaseGateV1MajorityAndArtifactBinding(t *testing.T) {
	evidence := phaseGateEvidenceForTest(t, []string{"pass", "pass", "fail"}, -1)
	path := filepath.Join(t.TempDir(), "evidence.json")
	writePhaseGateTestJSON(t, path, evidence)
	out := filepath.Join(t.TempDir(), "resolved")
	got, err := resolveDistillPhaseGateV1(path, "", out)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "pass" || got.ValidPrimaries != 3 || got.MissingPrimaries != 0 {
		t.Fatalf("summary = %+v", got)
	}
	if _, err := os.Stat(filepath.Join(out, "archive.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(evidence.BaseArtifacts[0].Path, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	badPath := filepath.Join(t.TempDir(), "changed.json")
	writePhaseGateTestJSON(t, badPath, evidence)
	if _, err := resolveDistillPhaseGateV1(badPath, "", filepath.Join(t.TempDir(), "bad")); err == nil {
		t.Fatal("accepted artifact bytes that did not match the sealed digest")
	}
}

func TestResolveDistillPhaseGateV1CoverageAndAstraEscalation(t *testing.T) {
	one := phaseGateEvidenceForTest(t, []string{"pass"}, -1)
	onePath := filepath.Join(t.TempDir(), "one.json")
	writePhaseGateTestJSON(t, onePath, one)
	got, err := resolveDistillPhaseGateV1(onePath, "", filepath.Join(t.TempDir(), "one-out"))
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "fail" || got.Reason != "fewer than two valid primary verdicts" {
		t.Fatalf("coverage result = %+v", got)
	}

	evidence := phaseGateEvidenceForTest(t, []string{"pass", "pass", "fail"}, 2)
	evidencePath := filepath.Join(t.TempDir(), "evidence.json")
	writePhaseGateTestJSON(t, evidencePath, evidence)
	primariesDigest := distillPhaseGatePrimariesDigestV1(evidence)
	astra := distillPhaseGateAstraV1{Contract: distillPhaseGateAstraContractV1, SchemaVersion: 1, Model: "astra-test", EvidenceDigest: evidence.EvidenceDigest, PrimariesDigest: primariesDigest, Verdict: "fail"}
	attachPhaseGateAstraArtifactsForTest(t, &astra)
	astraForDigest := astra
	astraForDigest.Artifacts = redactDistillPhaseGateArtifactPathsV1(astra.Artifacts)
	astra.JudgmentDigest, _ = distillPhaseGateDigestV1(astraForDigest)
	astraPath := filepath.Join(t.TempDir(), "astra.json")
	writePhaseGateTestJSON(t, astraPath, astra)
	got, err = resolveDistillPhaseGateV1(evidencePath, astraPath, filepath.Join(t.TempDir(), "astra-out"))
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "fail" || !got.CriticalDissent || got.AstraVerdict != "fail" {
		t.Fatalf("Astra result = %+v", got)
	}
	astra.EvidenceDigest = distillCandidateCacheDigestV2("wrong")
	bad := filepath.Join(t.TempDir(), "bad-astra.json")
	writePhaseGateTestJSON(t, bad, astra)
	if _, err := resolveDistillPhaseGateV1(evidencePath, bad, filepath.Join(t.TempDir(), "bad-out")); err == nil {
		t.Fatal("accepted Astra judgment bound to different evidence")
	}
}

func TestResolveDistillPhaseGateV1RejectsUnboundOrInconsistentVerdict(t *testing.T) {
	evidence := phaseGateEvidenceForTest(t, []string{"pass", "pass"}, -1)
	evidence.PrimaryVerdicts[0].PayloadDigest = distillCandidateCacheDigestV2("unverified payload")
	evidence.PrimaryVerdicts[0].VerdictDigest = ""
	evidence.PrimaryVerdicts[0].VerdictDigest, _ = distillPhaseGateDigestV1(evidence.PrimaryVerdicts[0])
	path := filepath.Join(t.TempDir(), "unbound.json")
	writePhaseGateTestJSON(t, path, evidence)
	if _, err := resolveDistillPhaseGateV1(path, "", filepath.Join(t.TempDir(), "out")); err == nil {
		t.Fatal("accepted a verdict payload digest without matching verified bytes")
	}

	evidence = phaseGateEvidenceForTest(t, []string{"pass", "pass"}, -1)
	evidence.PrimaryVerdicts[0].Dimensions[1].Verdict = "fail"
	evidence.PrimaryVerdicts[0].VerdictDigest = ""
	evidence.PrimaryVerdicts[0].VerdictDigest, _ = distillPhaseGateDigestV1(evidence.PrimaryVerdicts[0])
	path = filepath.Join(t.TempDir(), "inconsistent.json")
	writePhaseGateTestJSON(t, path, evidence)
	if _, err := resolveDistillPhaseGateV1(path, "", filepath.Join(t.TempDir(), "out")); err == nil {
		t.Fatal("accepted overall pass with failing quality dimension")
	}
}

func TestResolveDistillPhaseGateV1AggregateBindsAstraJudgmentAndRejectsSymlink(t *testing.T) {
	evidence := phaseGateEvidenceForTest(t, []string{"pass", "pass", "fail"}, 2)
	evidencePath := filepath.Join(t.TempDir(), "evidence.json")
	writePhaseGateTestJSON(t, evidencePath, evidence)
	makeAstra := func(model, verdict string) distillPhaseGateAstraV1 {
		a := distillPhaseGateAstraV1{Contract: distillPhaseGateAstraContractV1, SchemaVersion: 1, Model: model, EvidenceDigest: evidence.EvidenceDigest, PrimariesDigest: distillPhaseGatePrimariesDigestV1(evidence), Verdict: verdict}
		attachPhaseGateAstraArtifactsForTest(t, &a)
		aForDigest := a
		aForDigest.Artifacts = redactDistillPhaseGateArtifactPathsV1(a.Artifacts)
		a.JudgmentDigest, _ = distillPhaseGateDigestV1(aForDigest)
		return a
	}
	var aggregates []string
	for _, model := range []string{"astra-a", "astra-b"} {
		path := filepath.Join(t.TempDir(), model+".json")
		writePhaseGateTestJSON(t, path, makeAstra(model, "fail"))
		got, err := resolveDistillPhaseGateV1(evidencePath, path, filepath.Join(t.TempDir(), model+"-out"))
		if err != nil {
			t.Fatal(err)
		}
		aggregates = append(aggregates, got.AggregateDigest)
	}
	if aggregates[0] == aggregates[1] {
		t.Fatal("aggregate digest ignored the Astra judgment digest")
	}

	target := filepath.Join(t.TempDir(), "target.json")
	writePhaseGateTestJSON(t, target, evidence)
	link := filepath.Join(t.TempDir(), "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveDistillPhaseGateV1(link, "", filepath.Join(t.TempDir(), "link-out")); err == nil {
		t.Fatal("accepted symlink evidence input")
	}
	oversized := filepath.Join(t.TempDir(), "oversized.json")
	f, err := os.Create(oversized)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(distillPhaseGateMaxInputBytesV1 + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveDistillPhaseGateV1(oversized, "", filepath.Join(t.TempDir(), "oversized-out")); err == nil {
		t.Fatal("accepted oversized evidence input")
	}
}

func phaseGateEvidenceForTest(t *testing.T, verdicts []string, critical int) distillPhaseGateEvidenceV1 {
	t.Helper()
	dir := t.TempDir()
	writeArtifacts := func(items []struct{ name, content string }) []distillPhaseGateArtifactV1 {
		var artifacts []distillPhaseGateArtifactV1
		for _, item := range items {
			path := filepath.Join(dir, item.name+".bin")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(item.content), 0o600); err != nil {
				t.Fatal(err)
			}
			artifacts = append(artifacts, distillPhaseGateArtifactV1{Name: item.name, Path: path, Digest: distillCandidateCacheDigestV2(item.content)})
		}
		sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Name < artifacts[j].Name })
		return artifacts
	}
	baseArtifacts := writeArtifacts([]struct{ name, content string }{{"evaluation", "sealed evidence"}, {"input", "input"}, {"revision", "revision"}, {"rubric", "rubric"}})
	artifactDigest := distillCandidateCacheDigestV2("sealed evidence")
	e := distillPhaseGateEvidenceV1{Contract: distillPhaseGateEvidenceContractV1, SchemaVersion: 1, Phase: 2, RevisionDigest: distillCandidateCacheDigestV2("revision"), ArtifactDigest: artifactDigest, BaseArtifacts: baseArtifacts, RubricDigest: distillCandidateCacheDigestV2("rubric"), InputDigest: distillCandidateCacheDigestV2("input")}
	core := struct {
		Contract       string `json:"contract"`
		SchemaVersion  int    `json:"schema_version"`
		Phase          int    `json:"phase"`
		RevisionDigest string `json:"revision_digest"`
		ArtifactDigest string `json:"artifact_digest"`
		RubricDigest   string `json:"rubric_digest"`
		InputDigest    string `json:"input_digest"`
	}{e.Contract, e.SchemaVersion, e.Phase, e.RevisionDigest, e.ArtifactDigest, e.RubricDigest, e.InputDigest}
	e.EvidenceDigest, _ = distillPhaseGateDigestV1(core)
	judges := []string{"claude", "copilot", "cursor"}
	var judgeItems []struct{ name, content string }
	for _, judge := range judges {
		for _, part := range []string{"prompt", "payload", "telemetry", "rationale"} {
			judgeItems = append(judgeItems, struct{ name, content string }{"judge/" + judge + "/" + part, judge + " " + part + " " + e.EvidenceDigest})
		}
		judgeItems = append(judgeItems, struct{ name, content string }{"judge/" + judge + "/status", judge + " unavailable"})
	}
	e.JudgeArtifacts = writeArtifacts(judgeItems)
	artifactByName := map[string]string{}
	for _, artifact := range e.JudgeArtifacts {
		artifactByName[artifact.Name] = artifact.Digest
	}
	for i, v := range verdicts {
		judge := judges[i]
		verdict := distillPhaseGateVerdictV1{Judge: judge, Model: "test", EvidenceDigest: e.EvidenceDigest, Verdict: v, Critical: i == critical, Dimensions: []distillPhaseGateDimensionV1{{"performance", v}, {"quality", v}, {"progress", v}}, PromptDigest: artifactByName["judge/"+judge+"/prompt"], PayloadDigest: artifactByName["judge/"+judge+"/payload"], TelemetryDigest: artifactByName["judge/"+judge+"/telemetry"], RationaleDigest: artifactByName["judge/"+judge+"/rationale"]}
		verdict.VerdictDigest, _ = distillPhaseGateDigestV1(verdict)
		e.PrimaryVerdicts = append(e.PrimaryVerdicts, verdict)
	}
	for i, judge := range judges {
		status := "unavailable"
		detail := distillCandidateCacheDigestV2(judge + " unavailable")
		if i < len(verdicts) {
			status = "completed"
			detail = ""
		}
		e.PrimaryCoverage = append(e.PrimaryCoverage, distillPhaseGateCoverageV1{Judge: judge, Status: status, DetailDigest: detail})
	}
	return e
}

func attachPhaseGateAstraArtifactsForTest(t *testing.T, a *distillPhaseGateAstraV1) {
	t.Helper()
	dir := t.TempDir()
	for _, part := range []string{"prompt", "payload", "telemetry", "rationale"} {
		content := "astra " + part + " " + a.EvidenceDigest + " " + a.PrimariesDigest
		path := filepath.Join(dir, part+".bin")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		artifact := distillPhaseGateArtifactV1{Name: "astra/" + part, Path: path, Digest: distillCandidateCacheDigestV2(content)}
		a.Artifacts = append(a.Artifacts, artifact)
		switch part {
		case "prompt":
			a.PromptDigest = artifact.Digest
		case "payload":
			a.PayloadDigest = artifact.Digest
		case "telemetry":
			a.TelemetryDigest = artifact.Digest
		case "rationale":
			a.RationaleDigest = artifact.Digest
		}
	}
}

func writePhaseGateTestJSON(t *testing.T, path string, v any) {
	t.Helper()
	if err := writeDistillPhaseGateJSONV1(path, v); err != nil {
		t.Fatal(err)
	}
}
