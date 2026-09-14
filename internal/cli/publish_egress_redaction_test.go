package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ashtom/entire-brain/internal/brainwire"

	"github.com/ashtom/entire-brain/internal/factmerge"
)

// A distilled fact's anchor carries two LOCAL-ONLY coordinates: the brain-relative
// transcript path and the turn offset in it. `facts sync` has always stripped both
// before pushing a member's facts to the shared head (factsync.SanitizeForEgress);
// `brain publish` shipped the on-disk facts.ndjson verbatim, so the SAME records
// leaving for the SAME service disagreed about what may leave.
//
// The transcript path is not an opaque id. It renders as
//
//	sessions/main/20260530T174906Z_codex_<session-uuid>_<checkpoint>.jsonl
//
// which dates the session, names the agent tool that ran it, and with `line` points
// at the exact turn a statement came from.
//
// These tests pin the redaction at the level it must hold: the bytes of the facts
// artifact, and the digest/size the manifest publishes for them.

// factsStreamWithLocalAnchors is one branch's on-disk facts stream carrying both
// local-only coordinates plus the opaque ids that must SURVIVE the redaction.
const factsStreamWithLocalAnchors = `{"id":"fact:aaaa","paths":["architecture.auth.secrets"],"kind":"decision","text":"The vendor secret is read from 1Password at boot.","branch":"main","origin":"distilled","status":"active","provenance":[{"session_id":"019e78b6-21f0-7471-8066-c2586662120e","commit":"cafebabe","checkpoint_id":"14f4da1e48f4","turn_id":"t-9","transcript":"sessions/main/20260530T174906Z_codex_019e78b6-21f0-7471-8066-c2586662120e_14f4da1e48f4.jsonl","line":1634}],"created_at":"2026-06-17T17:20:31.308238Z","updated_at":"2026-06-17T17:20:31.308238Z"}
{"id":"fact:bbbb","paths":["architecture.auth.ratelimit"],"kind":"invariant","text":"Rate limiting uses a token bucket keyed by tenant id.","branch":"main","origin":"distilled","status":"active","provenance":[{"session_id":"3c8f3e78-b939-42b0-8ab8-aae534b6f768","transcript":"sessions/main/20260605T145718Z_claude-code_3c8f3e78-b939-42b0-8ab8-aae534b6f768_042ae6c1b089.jsonl","line":88}],"created_at":"2026-06-18T09:00:00Z","updated_at":"2026-06-18T09:00:00Z"}
`

func newTestBrainArtifact() *brainwire.BrainArtifact {
	return brainwire.NewBrainArtifact("gh/owner/repo", "main", time.Unix(0, 0).UTC())
}

func writeFactsStream(t *testing.T, brainDir, slug, content string) {
	t.Helper()
	dir := filepath.Join(brainDir, factsDirName, slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, factsFileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPublishRedactsLocalOnlyFactProvenance(t *testing.T) {
	brainDir := t.TempDir()
	writeFactsStream(t, brainDir, "main-0d6e4079", factsStreamWithLocalAnchors)

	art := newTestBrainArtifact()
	arts, err := collectFactArtifacts(art, brainDir)
	if err != nil {
		t.Fatalf("collectFactArtifacts: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("got %d fact artifacts, want 1", len(arts))
	}
	wire := string(arts[0].Data)

	// The local coordinates must be gone from the bytes that leave.
	for _, leak := range []string{
		"transcript",
		"sessions/main/",
		"20260530T174906Z",
		"20260605T145718Z",
		"_codex_",
		"claude-code",
		"1634",
	} {
		if strings.Contains(wire, leak) {
			t.Errorf("published facts artifact still carries local-only provenance %q:\n%s", leak, wire)
		}
	}
	// The opaque cross-member ids must survive, or provenance stops unioning.
	for _, keep := range []string{
		"019e78b6-21f0-7471-8066-c2586662120e",
		"3c8f3e78-b939-42b0-8ab8-aae534b6f768",
		"14f4da1e48f4",
		"cafebabe",
		"t-9",
	} {
		if !strings.Contains(wire, keep) {
			t.Errorf("published facts artifact dropped opaque provenance %q, which must cross the wire:\n%s", keep, wire)
		}
	}
	// And the facts themselves are unchanged.
	records, err := factmerge.ParseNDJSON(bytes.NewReader(arts[0].Data))
	if err != nil {
		t.Fatalf("published facts artifact does not parse: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("published %d records, want 2", len(records))
	}
	for _, r := range records {
		if r.Text == "" || r.Branch != "main" || r.Status != factmerge.StatusActive {
			t.Errorf("redaction damaged a record: %+v", r)
		}
		for _, a := range r.Provenance {
			if a.Transcript != "" || a.Line != 0 {
				t.Errorf("anchor still carries local coordinates: %+v", a)
			}
			if a.SessionID == "" {
				t.Errorf("anchor lost its session id: %+v", a)
			}
		}
	}
}

// The manifest must describe what actually leaves. If the digest or size still
// described the UNREDACTED file, the server would reject the bundle (or, worse,
// accept a reference that does not match its content).
func TestPublishManifestDigestsTheRedactedFacts(t *testing.T) {
	brainDir := t.TempDir()
	writeFactsStream(t, brainDir, "main-0d6e4079", factsStreamWithLocalAnchors)

	art := newTestBrainArtifact()
	arts, err := collectFactArtifacts(art, brainDir)
	if err != nil {
		t.Fatalf("collectFactArtifacts: %v", err)
	}
	if len(art.Facts) != 1 {
		t.Fatalf("manifest registered %d fact refs, want 1", len(art.Facts))
	}
	ref := art.Facts[0].Content
	if got, want := ref.Digest, contentDigest(arts[0].Data); got != want {
		t.Errorf("manifest digest %s does not describe the uploaded bytes %s", got, want)
	}
	if got, want := ref.Size, int64(len(arts[0].Data)); got != want {
		t.Errorf("manifest size %d does not describe the uploaded bytes (%d)", got, want)
	}
	if ref.Size == int64(len(factsStreamWithLocalAnchors)) {
		t.Errorf("manifest still sizes the UNREDACTED on-disk stream (%d bytes)", ref.Size)
	}
}

// A facts stream that cannot be parsed cannot be redacted, and an unredactable
// stream must not be shipped raw as a fallback.
func TestPublishRefusesFactsItCannotRedact(t *testing.T) {
	brainDir := t.TempDir()
	writeFactsStream(t, brainDir, "main-0d6e4079", "{\"branch\":\"main\",\"id\":\"fact:a\"}\n{not json\n")

	if _, err := collectFactArtifacts(newTestBrainArtifact(), brainDir); err == nil {
		t.Fatal("collectFactArtifacts accepted an unparseable facts stream; it must refuse rather than ship it unredacted")
	}
}

// A 200 whose body acknowledges no artifacts is the worst kind of failure to paper
// over: the member is told the brain is published and it is not.
func TestPublishRefusesA200ThatAcknowledgesNothing(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	body := publishRequestBody{Artifacts: []publishArtifact{
		{Kind: brainKindManifest, Ref: "abc", Data: []byte(`{}`)},
		{Kind: brainKindFacts, Ref: "main", Data: []byte("{}\n")},
	}}
	_, err := postBrainArtifacts(t.Context(), srv.URL, "01HZZPROBE0000000000000000", "tok", body)
	if err == nil {
		t.Fatal("publish reported success for a 200 that stored nothing")
	}
	if !strings.Contains(err.Error(), "publish_unacknowledged") {
		t.Errorf("error %q does not carry the publish_unacknowledged code", err)
	}
	if !strings.Contains(err.Error(), "NOT published") {
		t.Errorf("error %q does not tell the member the brain was not published", err)
	}
	if hits != 1 {
		t.Errorf("made %d requests, want 1", hits)
	}
}

// The normal acknowledged case must still succeed, so the check above cannot be
// satisfied by simply failing every publish.
func TestPublishAcceptsAnAcknowledged200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in publishRequestBody
		_ = json.NewDecoder(r.Body).Decode(&in)
		out := map[string]any{"status": "ok", "stored": []map[string]string{}}
		stored := make([]map[string]string, 0, len(in.Artifacts))
		for _, a := range in.Artifacts {
			stored = append(stored, map[string]string{"kind": a.Kind, "ref": a.Ref})
		}
		out["stored"] = stored
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()

	body := publishRequestBody{Artifacts: []publishArtifact{{Kind: brainKindFacts, Ref: "main", Data: []byte("{}\n")}}}
	res, err := postBrainArtifacts(t.Context(), srv.URL, "01HZZPROBE0000000000000000", "tok", body)
	if err != nil {
		t.Fatalf("publish refused an acknowledged 200: %v", err)
	}
	if len(res.Stored) != 1 {
		t.Errorf("got %d stored entries, want 1", len(res.Stored))
	}
}

// A hosted endpoint that echoes the request's Authorization header into its error
// body would otherwise get the caller's own bearer token written into stderr, and
// from there into terminal scrollback, a CI job log, or a pasted bug report.
func TestPublishErrorsDoNotEchoTheBearerToken(t *testing.T) {
	const token = "entire_pat_0123456789abcdef"
	for _, status := range []int{
		http.StatusUnauthorized, http.StatusForbidden, http.StatusUnprocessableEntity,
		http.StatusServiceUnavailable, http.StatusRequestEntityTooLarge, http.StatusInternalServerError,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"detail":"rejected credential ` + r.Header.Get("Authorization") + `"}`))
		}))
		body := publishRequestBody{Artifacts: []publishArtifact{{Kind: brainKindFacts, Ref: "main", Data: []byte("{}\n")}}}
		_, err := postBrainArtifacts(t.Context(), srv.URL, "01HZZPROBE0000000000000000", token, body)
		srv.Close()
		if err == nil {
			t.Errorf("HTTP %d was not reported as an error", status)
			continue
		}
		if strings.Contains(err.Error(), token) {
			t.Errorf("HTTP %d echoed the bearer token into the publish error: %v", status, err)
		}
		if !strings.Contains(err.Error(), "rejected credential") {
			t.Errorf("HTTP %d lost the server's explanation along with the token: %v", status, err)
		}
	}
}
