package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func distillCandidateCacheTestIdentity() distillCandidateCacheIdentityV2 {
	return distillCandidateCacheIdentityV2{
		CardDigest: distillCandidateCacheDigestV2("redacted card"), PromptVersion: "candidate-id-v2",
		PromptDigest: distillCandidateCacheDigestV2("prompt"), TaxonomyDigest: distillCandidateCacheDigestV2("taxonomy"),
		AgentCommandDigest: distillCandidateCacheDigestV2("codex exec"), Model: "gpt-5", Effort: "medium", RedactionVersion: "redaction-v1",
	}
}

func distillCandidateCacheTestResult() distillCandidateCacheResultV2 {
	return distillCandidateCacheResultV2{Facts: []distillCandidateCachedFactV2{{
		Kind: factKindDecision, Paths: []string{"architecture.data.flow"}, Text: "The user chose the candidate cache protocol.",
	}}}
}

func TestDistillCandidateResultCacheV2RoundTripDeterministicAndPrivacyBounded(t *testing.T) {
	brainDir := t.TempDir()
	cache := newDistillCandidateResultCacheV2()
	identity := distillCandidateCacheTestIdentity()
	if err := cache.PutSuccess("candidate-v1:b", "session-b", identity, distillCandidateCacheResultV2{Empty: true}); err != nil {
		t.Fatal(err)
	}
	if err := cache.PutSuccess("candidate-v1:a", "session-a", identity, distillCandidateCacheTestResult()); err != nil {
		t.Fatal(err)
	}
	if err := saveDistillCandidateResultCacheV2(brainDir, cache); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(brainDir, filepath.FromSlash(distillCandidateResultCacheV2Path))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Index(string(data), "candidate-v1:a") > strings.Index(string(data), "candidate-v1:b") {
		t.Fatalf("cache entries are not deterministic: %s", data)
	}
	for _, forbidden := range []string{"raw provider", "checkpoint_id", "branch", "transcript", "packing", "confidence"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("cache retained forbidden %q: %s", forbidden, data)
		}
	}
	loaded, err := loadDistillCandidateResultCacheV2(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := loaded.LookupSuccess("candidate-v1:a", identity)
	if !ok || len(got.Facts) != 1 || got.Facts[0].Text != "The user chose the candidate cache protocol." {
		t.Fatalf("round-trip result = %+v, hit=%v", got, ok)
	}
	if _, ok := loaded.LookupSuccess("candidate-v1:b", identity); !ok {
		t.Fatal("explicit successful empty result was not cached")
	}
}

func TestDistillCandidateResultCacheV2RejectsCorruptionAndBounds(t *testing.T) {
	identity := distillCandidateCacheTestIdentity()
	entry := distillCandidateCacheEntryV2{CandidateID: "candidate-v1:a", SourceSessionID: "session-a", SourceSessionDigest: distillCandidateSessionOwnershipDigestV2("session-a"), Identity: identity, Result: distillCandidateCacheTestResult()}
	valid, err := marshalDistillCandidateResultCacheV2(distillCandidateResultCacheV2{entries: map[string]distillCandidateCacheEntryV2{entry.CandidateID: entry}})
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"unknown field":  append([]byte(`{"type":"header","version":2,"leak":"raw card"}`+"\n"), valid[strings.Index(string(valid), "\n")+1:]...),
		"duplicate":      append(valid, valid[strings.Index(string(valid), "\n")+1:]...),
		"missing header": []byte(`{"type":"result","entry":{}}` + "\n"),
		"too large":      []byte(strings.Repeat("x", distillCandidateResultCacheV2MaxBytes+1)),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseDistillCandidateResultCacheV2(data); err == nil {
				t.Fatalf("corrupt cache was accepted")
			}
		})
	}

	cache := newDistillCandidateResultCacheV2()
	bad := distillCandidateCacheTestResult()
	bad.Facts[0].Text = "token sk-abcdefghijklmnopqrstuvwxyz"
	if err := cache.PutSuccess("candidate-v1:a", "session-a", identity, bad); err == nil {
		t.Fatal("unredacted parsed fact was cacheable")
	}
	if err := cache.PutSuccess("candidate-v1:a", "session-a", identity, distillCandidateCacheResultV2{}); err == nil {
		t.Fatal("implicit empty result was cacheable")
	}
}

func TestDistillCandidateResultCacheV2IdentityMissesButRepackingDoesNotMatter(t *testing.T) {
	cache := newDistillCandidateResultCacheV2()
	identity := distillCandidateCacheTestIdentity()
	if err := cache.PutSuccess("candidate-v1:a", "session-a", identity, distillCandidateCacheTestResult()); err != nil {
		t.Fatal(err)
	}
	if _, ok := cache.LookupSuccess("candidate-v1:a", identity); !ok {
		t.Fatal("same candidate and identity should hit after repacking")
	}
	changes := []func(*distillCandidateCacheIdentityV2){
		func(value *distillCandidateCacheIdentityV2) {
			value.CardDigest = distillCandidateCacheDigestV2("other card")
		},
		func(value *distillCandidateCacheIdentityV2) {
			value.PromptDigest = distillCandidateCacheDigestV2("other prompt")
		},
		func(value *distillCandidateCacheIdentityV2) {
			value.TaxonomyDigest = distillCandidateCacheDigestV2("other taxonomy")
		},
		func(value *distillCandidateCacheIdentityV2) {
			value.AgentCommandDigest = distillCandidateCacheDigestV2("other command")
		},
		func(value *distillCandidateCacheIdentityV2) { value.Model = "other-model" },
		func(value *distillCandidateCacheIdentityV2) { value.Effort = "high" },
		func(value *distillCandidateCacheIdentityV2) { value.RedactionVersion = "redaction-v2" },
	}
	for _, change := range changes {
		changed := identity
		change(&changed)
		if _, ok := cache.LookupSuccess("candidate-v1:a", changed); ok {
			t.Fatal("changed extraction identity incorrectly hit")
		}
	}
}
