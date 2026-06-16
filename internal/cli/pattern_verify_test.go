package cli

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestParseDossierVerdict(t *testing.T) {
	out := "noise before {\n  \"verdict\": \"NEEDS_SPLIT\", \"reason\": \"two patterns\", \"conflated_subpatterns\": [\"a\",\"b\"]\n} trailing"
	v, raw, err := parseDossierVerdict(out)
	if err != nil {
		t.Fatal(err)
	}
	if v.Verdict != "needs_split" {
		t.Errorf("verdict = %q, want needs_split (lowercased)", v.Verdict)
	}
	if v.SchemaVersion != dossierVerifySchemaVersion {
		t.Errorf("schema_version defaulted wrong: %d", v.SchemaVersion)
	}
	if strings.Contains(raw, "noise") || strings.Contains(raw, "trailing") {
		t.Errorf("raw should be the trimmed JSON object, got %q", raw)
	}
	if _, _, err := parseDossierVerdict(`{"verdict":"banana"}`); err == nil {
		t.Error("expected error for invalid verdict")
	}
	if _, _, err := parseDossierVerdict(`not json`); err == nil {
		t.Error("expected error for non-JSON output")
	}
}

func TestVerifyDossiersNoEgressRejected(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	db := promotableCorpus(t, time.Now())
	now := time.Now()
	if err := buildPatternCandidates(db, "gh/acme/cli", now); err != nil {
		t.Fatal(err)
	}
	called := false
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		called = true
		return "", nil
	}
	_, err := verifyDossiers(context.Background(), db, t.TempDir(), "codex", "", "", run, now)
	if err == nil {
		t.Fatal("expected no_egress rejection for --agent codex")
	}
	if !strings.Contains(err.Error(), "no_egress") {
		t.Errorf("error = %v, want no_egress", err)
	}
	if called {
		t.Error("agent runner must NOT be called under no-egress")
	}
}

func TestVerifyDossiersWritesAndCachesVerdict(t *testing.T) {
	db := promotableCorpus(t, time.Now())
	now := time.Now()
	if err := buildPatternCandidates(db, "gh/acme/cli", now); err != nil {
		t.Fatal(err)
	}
	calls := 0
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		calls++
		// The dossier (redacted JSON) must reach the agent as input.
		if !strings.Contains(string(input), "deploy:release") {
			t.Errorf("verifier input missing the dossier: %s", string(input))
		}
		return `{"schema_version":1,"verdict":"accepted","reason":"sound","evidence_fingerprint":"ignored"}`, nil
	}
	stats, err := verifyDossiers(context.Background(), db, t.TempDir(), "codex", "", "", run, now)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Verified != 1 || stats.Considered != 1 {
		t.Fatalf("stats = %+v, want 1 considered/1 verified", stats)
	}
	var verdict, status, verified, fp string
	db.QueryRow(`SELECT verdict, status, COALESCE(verified_fingerprint,''), fingerprint FROM dossiers LIMIT 1`).Scan(&verdict, &status, &verified, &fp)
	if verdict != "accepted" {
		t.Errorf("verdict = %q, want accepted", verdict)
	}
	if status != "current" {
		t.Errorf("status = %q, want current", status)
	}
	if verified != fp {
		t.Errorf("verified_fingerprint %q must be pinned to evidence fingerprint %q", verified, fp)
	}

	// Second run: fingerprint unchanged → cached, agent NOT called again.
	stats2, err := verifyDossiers(context.Background(), db, t.TempDir(), "codex", "", "", run, now)
	if err != nil {
		t.Fatal(err)
	}
	if stats2.Cached != 1 || stats2.Verified != 0 {
		t.Errorf("second run stats = %+v, want all cached", stats2)
	}
	if calls != 1 {
		t.Errorf("agent called %d times, want exactly 1 (second run served from cache)", calls)
	}
}

// TestRefreshDoesNotInvokeVerifier proves the deterministic build never produces
// a verdict — the agent path is reachable only through the explicit verify
// surface (refresh/watch/brief/query/MCP must stay token-free).
func TestRefreshDoesNotInvokeVerifier(t *testing.T) {
	db := promotableCorpus(t, time.Now())
	if err := buildPatternCandidates(db, "gh/acme/cli", time.Now()); err != nil {
		t.Fatal(err)
	}
	var withVerdict int
	db.QueryRow(`SELECT COUNT(*) FROM dossiers WHERE verdict IS NOT NULL`).Scan(&withVerdict)
	if withVerdict != 0 {
		t.Errorf("deterministic build set %d verdict(s); the verifier must only run via explicit `patterns verify`", withVerdict)
	}
}
