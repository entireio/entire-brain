package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	"github.com/entireio/entire-brain/internal/cli/internal/patterntest"
)

func TestCorpusEpisodeOpsCodex(t *testing.T) {
	// exec_command with a failing exit, an apply_patch file edit, and a tool
	// OUTPUT that merely contains command-looking text (must not be a command).
	work := `{"type":"response_item","payload":{"type":"function_call","call_id":"c1","name":"exec_command","arguments":"{\"cmd\":\"go test ./...\"}"}}
{"type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":"FAIL\nexited with code 1"}}
{"type":"response_item","payload":{"type":"custom_tool_call","call_id":"c2","name":"apply_patch","input":"*** Begin Patch\n*** Update File: internal/x.go\n+foo"}}
{"type":"response_item","payload":{"type":"function_call_output","call_id":"c3","output":"run: go build ./..."}}`
	tools, commands, files := corpusEpisodeOps(work)
	if len(commands) != 1 || commands[0].head != "go test" {
		t.Fatalf("commands = %+v, want only [go test] (output not parsed as command)", commands)
	}
	if commands[0].exitCode == nil || *commands[0].exitCode != 1 || !commands[0].failed {
		t.Errorf("go test should be failed exit 1: %+v", commands[0])
	}
	if len(files) != 1 || files[0].path != "internal/x.go" || files[0].action != "edit" {
		t.Errorf("files = %+v, want internal/x.go edit", files)
	}
	if len(tools) != 2 { // function_call(exec), custom_tool_call(apply_patch); outputs are not tools.
		var names []string
		for _, x := range tools {
			names = append(names, x.name)
		}
		t.Fatalf("tools = %d (%v), want 2 (exec_command, apply_patch)", len(tools), names)
	}
}

func TestCorpusEpisodeOpsClaude(t *testing.T) {
	work := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"bash","input":{"command":"gofmt -w ."}},{"type":"tool_use","id":"t2","name":"edit","input":{"file_path":"a.go"}}]}}`
	tools, commands, files := corpusEpisodeOps(work)
	if len(commands) != 1 || commands[0].head != "gofmt" {
		t.Errorf("commands = %+v, want [gofmt]", commands)
	}
	if len(files) != 1 || files[0].path != "a.go" || files[0].action != "edit" {
		t.Errorf("files = %+v, want a.go edit", files)
	}
	if len(tools) != 2 {
		t.Errorf("tools = %d, want 2 (bash, edit)", len(tools))
	}
}

func TestClassifyMetaHits(t *testing.T) {
	has := func(hits []corpusMetaHit, id string) bool {
		for _, h := range hits {
			if h.metaID == id {
				return true
			}
		}
		return false
	}
	if !has(classifyMetaHits("Review the current branch changes", "", 2, false), "review_request") {
		t.Error("expected review_request")
	}
	if !has(classifyMetaHits("no, that's wrong, revert it", "", 1, false), "correction_followup") {
		t.Error("expected correction_followup")
	}
	if !has(classifyMetaHits("how does flush stay dirty", "", 0, false), "read_only_diagnosis") {
		t.Error("expected read_only_diagnosis for a no-command question")
	}
	if !has(classifyMetaHits("run the suite", "", 3, true), "validation_loop") {
		t.Error("expected validation_loop when a validation command ran")
	}
}

func TestLinkEpisodeFacts(t *testing.T) {
	facts := []factRecord{
		{ID: "f1", Status: "active", Branch: "main", Text: "The worktree fingerprint hashes git status and the binary diff.", Locus: []string{"worktreefingerprint"}, Paths: []string{"architecture.x.y"}},
		{ID: "f2", Status: "active", Branch: "main", Text: "Unrelated note about colors.", Paths: []string{"misc.x.y"}},
	}
	links := linkEpisodeFacts(facts, "fix the worktree fingerprint and run tests", nil)
	if len(links) != 1 || links[0].factID != "f1" {
		t.Errorf("links = %+v, want only f1", links)
	}
	// Graceful: no facts -> no links.
	if l := linkEpisodeFacts(nil, "anything", nil); l != nil {
		t.Errorf("expected nil links with no facts, got %+v", l)
	}
}

func TestCorpusEnrichmentIntegration(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	transcript := `{"type":"event_msg","payload":{"type":"user_message","message":"fix the worktree fingerprint and run tests"}}
{"type":"response_item","payload":{"type":"function_call","call_id":"c1","name":"exec_command","arguments":"{\"cmd\":\"go test ./...\"}"}}
{"type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":"FAIL exited with code 1"}}
{"type":"response_item","payload":{"type":"custom_tool_call","call_id":"c2","name":"apply_patch","input":"*** Update File: internal/cli/semantic.go"}}
{"type":"response_item","payload":{"type":"function_call","call_id":"c3","name":"exec_command","arguments":"{\"cmd\":\"curl -H 'Authorization: Bearer ghp_SECRETTOKEN0123456789abcdef'\"}"}}
{"type":"event_msg","payload":{"type":"user_message","message":"thanks"}}`
	brainDir := writeEpisodeFixture(t, now, "gh/acme/cli", []sessionFixture{
		{id: "s1", branch: "main", agent: "Codex", checkpoint: "cp1", relPath: "sessions/main/s1.jsonl", author: "Ada", transcript: transcript},
	})
	if err := factstore.Write(brainDir, []factRecord{
		{ID: "fact:wt", Paths: []string{"architecture.x.y"}, Text: "The worktree fingerprint hashes git status and the binary diff.", Locus: []string{"worktreefingerprint"}, Branch: "main", Origin: "distilled", Status: "active", Provenance: []factAnchor{{SessionID: "s1"}}, UpdatedAt: now},
	}); err != nil {
		t.Fatal(err)
	}
	if err := buildPatternCorpus(brainDir, now); err != nil {
		t.Fatal(err)
	}
	db := openCorpus(t, brainDir)

	var fails int
	if err := db.QueryRow(`SELECT COUNT(*) FROM episode_commands WHERE failed=1`).Scan(&fails); err != nil {
		t.Fatal(err)
	}
	if fails < 1 {
		t.Error("expected a failed command recorded")
	}
	if n := patterntest.CorpusCount(t, db, "episode_files"); n < 1 {
		t.Error("expected an episode_files row from apply_patch")
	}
	if n := patterntest.CorpusCount(t, db, "episode_facts"); n < 1 {
		t.Error("expected an episode_facts link to the worktree-fingerprint fact")
	}
	var metaLoop int
	if err := db.QueryRow(`SELECT COUNT(*) FROM meta_hits WHERE meta_id='validation_loop'`).Scan(&metaLoop); err != nil {
		t.Fatal(err)
	}
	if metaLoop < 1 {
		t.Error("expected validation_loop meta hit (go test ran)")
	}
	// Redaction: the bearer token must not be stored in the corpus.
	var blob string
	if err := db.QueryRow(`SELECT group_concat(raw_redacted, '||') FROM episode_commands`).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(blob, "ghp_SECRETTOKEN0123456789abcdef") {
		t.Errorf("corpus leaked a secret in raw_redacted: %s", blob)
	}
}

// Compact v1 transcripts keep an assistant turn's content at the record's top
// level. Uses the fixtures from compact_transcript_ops_fixtures_test.go.
func TestCorpusEpisodeOpsCompactTranscript(t *testing.T) {
	cases := []struct {
		name, fixture string
		tools         []string
		commands      []string
		files         []string
	}{
		{"claude-code", compactOpsClaudeTranscript, []string{"bash"}, []string{"go test"}, nil},
		{"codex", compactOpsCodexTranscript, []string{"exec_command", "apply_patch"}, []string{"grep"}, []string{"edit internal/cli/export.go"}},
		{"pi", compactOpsPiTranscript, []string{"read"}, nil, []string{"read retry.go"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tools, commands, files := corpusEpisodeOps(c.fixture)
			var gotTools, gotCommands, gotFiles []string
			for _, x := range tools {
				gotTools = append(gotTools, x.name)
			}
			for _, x := range commands {
				gotCommands = append(gotCommands, x.head)
			}
			for _, x := range files {
				gotFiles = append(gotFiles, x.action+" "+x.path)
			}
			if strings.Join(gotTools, ",") != strings.Join(c.tools, ",") {
				t.Errorf("tools = %v, want %v", gotTools, c.tools)
			}
			if strings.Join(gotCommands, ",") != strings.Join(c.commands, ",") {
				t.Errorf("commands = %v, want %v", gotCommands, c.commands)
			}
			if strings.Join(gotFiles, ",") != strings.Join(c.files, ",") {
				t.Errorf("files = %v, want %v", gotFiles, c.files)
			}
		})
	}
}

// A compact codex apply_patch has no path field: the cli converter wraps the
// custom_tool_call patch text as {"input": <patch>}. It must give the same file
// refs as the native custom_tool_call does.
func TestCorpusEpisodeOpsCompactApplyPatchMatchesNative(t *testing.T) {
	patch, err := json.Marshal("*** Begin Patch\n*** Add File: docs/new.md\n+x\n*** Update File: internal/cli/export.go\n@@\n-a\n+b\n*** Delete File: old.go\n*** End Patch")
	if err != nil {
		t.Fatal(err)
	}
	refs := func(work string) string {
		t.Helper()
		_, _, files := corpusEpisodeOps(work)
		var got []string
		for _, f := range files {
			got = append(got, f.action+" "+f.path)
		}
		return strings.Join(got, ",")
	}
	const want = "add docs/new.md,edit internal/cli/export.go,delete old.go"
	native := `{"type":"response_item","payload":{"type":"custom_tool_call","call_id":"c2","name":"apply_patch","input":` + string(patch) + `}}`
	if got := refs(native); got != want {
		t.Fatalf("native codex files = %q, want %q", got, want)
	}
	compact := `{"v":1,"agent":"codex","type":"assistant","ts":"t","content":[{"type":"tool_use","id":"c2","name":"apply_patch","input":{"input":` + string(patch) + `},"result":{"output":"Success.","status":"success"}}]}`
	if got := refs(compact); got != want {
		t.Errorf("compact codex files = %q, want %q (same as native)", got, want)
	}
	// An explicit path still wins over the patch text, as before.
	withPath := `{"v":1,"agent":"codex","type":"assistant","ts":"t","content":[{"type":"tool_use","id":"c3","name":"apply_patch","input":{"path":"a.go","input":` + string(patch) + `}}]}`
	if got := refs(withPath); got != "edit a.go" {
		t.Errorf("apply_patch with a path: files = %q, want %q", got, "edit a.go")
	}
}

// Through the corpus build: the patch-only compact codex turn is a write, not
// read_only. A corpus indexed by IndexerVersion 10 stored no file refs for it,
// and the transcript bytes do not change on upgrade, so only a version bump
// makes the next build re-index it instead of skipping it as unchanged.
func TestPatternCorpusCompactApplyPatchShapesWrite(t *testing.T) {
	now := time.Now()
	brainDir := writeEpisodeFixture(t, now, "gh/acme/cli", []sessionFixture{{id: "s1", branch: "main", agent: "codex", relPath: "sessions/main/s1.jsonl", transcript: compactOpsCodexTranscript}})
	query := func(q string) string {
		t.Helper()
		db := openCorpus(t, brainDir)
		defer db.Close()
		rows, err := db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			got = append(got, s)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return strings.Join(got, ",")
	}
	const (
		shapesQ    = `SELECT e.turn_ord || ':' || s.shape FROM episodes e JOIN episode_shapes s ON s.episode_id = e.id ORDER BY e.turn_ord`
		filesQ     = `SELECT e.turn_ord || ':' || f.action || ' ' || f.path FROM episode_files f JOIN episodes e ON e.id = f.episode_id ORDER BY e.turn_ord, f.path`
		wantShapes = "0:shell,1:write"
		wantFiles  = "1:edit internal/cli/export.go"
	)
	if err := buildPatternCorpus(brainDir, now); err != nil {
		t.Fatal(err)
	}
	if got := query(shapesQ); got != wantShapes {
		t.Fatalf("shapes = %q, want %q", got, wantShapes)
	}
	if got := query(filesQ); got != wantFiles {
		t.Fatalf("episode_files = %q, want %q", got, wantFiles)
	}

	db := openCorpus(t, brainDir)
	for _, stmt := range []string{`UPDATE indexed_sessions SET parser_version=10`, `UPDATE meta SET value='10' WHERE key='pattern_indexer_version'`, `DELETE FROM episode_files`} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	db.Close()
	if err := buildPatternCorpus(brainDir, now); err != nil {
		t.Fatal(err)
	}
	if got := query(filesQ); got != wantFiles {
		t.Fatalf("version 10 corpus kept its missing file refs after rebuild: episode_files = %q, want %q", got, wantFiles)
	}
	if got := query(shapesQ); got != wantShapes {
		t.Fatalf("version 10 corpus kept its shapes after rebuild: shapes = %q, want %q", got, wantShapes)
	}
}

// End to end through the corpus build, plus the upgrade path: a corpus indexed
// by IndexerVersion 9 recorded no tool operations for compact sessions, and
// the transcript bytes do not change on upgrade, so only a version bump makes
// the next build re-index them instead of skipping them as unchanged.
func TestPatternCorpusReindexesCompactTranscriptToolOps(t *testing.T) {
	now := time.Now()
	brainDir := writeEpisodeFixture(t, now, "gh/acme/cli", []sessionFixture{{id: "s1", branch: "main", agent: "codex", relPath: "sessions/main/s1.jsonl", transcript: compactOpsCodexTranscript}})
	ops := func() string {
		t.Helper()
		db := openCorpus(t, brainDir)
		defer db.Close()
		var got []string
		for _, q := range []string{
			`SELECT t.tool_type || COALESCE(':' || t.tool_name, '') FROM episode_tools t JOIN episodes e ON e.id = t.episode_id ORDER BY e.turn_ord, t.ord`,
			`SELECT c.head FROM episode_commands c JOIN episodes e ON e.id = c.episode_id ORDER BY e.turn_ord, c.ord`,
		} {
			rows, err := db.Query(q)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var s string
				if err := rows.Scan(&s); err != nil {
					t.Fatal(err)
				}
				got = append(got, s)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			rows.Close()
		}
		return strings.Join(got, ",")
	}
	const want = "exec_command,apply_patch,grep"
	if err := buildPatternCorpus(brainDir, now); err != nil {
		t.Fatal(err)
	}
	if got := ops(); got != want {
		t.Fatalf("corpus ops = %q, want %q", got, want)
	}

	db := openCorpus(t, brainDir)
	for _, stmt := range []string{`UPDATE indexed_sessions SET parser_version=9`, `UPDATE meta SET value='9' WHERE key='pattern_indexer_version'`, `DELETE FROM episode_tools`, `DELETE FROM episode_commands`} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	db.Close()
	if err := buildPatternCorpus(brainDir, now); err != nil {
		t.Fatal(err)
	}
	if got := ops(); got != want {
		t.Fatalf("version 9 corpus kept its empty tool ops after rebuild: ops = %q, want %q", got, want)
	}
}
