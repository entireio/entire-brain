package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/internal/patterntest"
)

// A current Codex rollout reaches the brain through the cli's item path
// (entireio/cli#2897): a command is a tool_use named "Bash" that carries the
// command's own output and marks a failure only as status "error", and an edit
// is a tool_use named "FileChange" with absolute paths. These are the lines
// the cli's converter writes for a failing go test and an edit of main.go,
// with the rollout's /work/repo checkout replaced by repo.
func codexItemPathTranscript(t *testing.T, repo string) string {
	t.Helper()
	path, err := json.Marshal(filepath.Join(repo, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	return `{"v":1,"agent":"codex","cli_version":"0.13.1","type":"user","ts":"2030-01-01T00:00:01.000Z","id":"u1","content":[{"text":"Fix the failing test in main.go"}]}
{"v":1,"agent":"codex","cli_version":"0.13.1","type":"assistant","ts":"2030-01-01T00:00:02.000Z","id":"e1","content":[{"id":"e1","input":{"command":"go test ./..."},"name":"Bash","result":{"output":"--- FAIL: TestX\nFAIL\n","status":"error"},"type":"tool_use"}]}
{"v":1,"agent":"codex","cli_version":"0.13.1","type":"assistant","ts":"2030-01-01T00:00:03.000Z","id":"f1","content":[{"id":"f1","input":{"changes":[{"path":` + string(path) + `,"kind":"update","diff":"@@ -1 +1 @@\n-a\n+b\n"}]},"name":"FileChange","result":{"output":"","status":"success"},"type":"tool_use"}]}
{"v":1,"agent":"codex","cli_version":"0.13.1","type":"assistant","ts":"2030-01-01T00:00:04.000Z","id":"a1","content":[{"type":"text","text":"Fixed main.go."}]}
`
}

func describeFileRefs(files []corpusFileRef) string {
	var out []string
	for _, f := range files {
		out = append(out, f.action+" "+f.path)
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// A Bash block's status "error" is the item path's only failure signal: the
// command is stored as failed with no exit code. Its output is the command's
// own, so exit text in it is not read, and status "success" is not a failure.
func TestCorpusEpisodeOpsCodexItemPathCommands(t *testing.T) {
	bash := func(output, status string) string {
		out, err := json.Marshal(output)
		if err != nil {
			t.Fatal(err)
		}
		return `{"v":1,"agent":"codex","cli_version":"0.13.1","type":"assistant","ts":"t","id":"e1","content":[{"id":"e1","input":{"command":"go test ./..."},"name":"Bash","result":{"output":` + string(out) + `,"status":"` + status + `"},"type":"tool_use"}]}`
	}
	cases := []struct {
		name, work, want string
	}{
		{"status error", bash("--- FAIL: TestX\nFAIL\n", "error"), `["go test exit=nil failed=true"]`},
		{"status error with exit text in output", bash("Exit code: 0\n", "error"), `["go test exit=nil failed=true"]`},
		{"status success", bash("ok\n", "success"), `["go test exit=nil failed=false"]`},
		{"status success printing exit code colon", bash("Exit code: 1\n", "success"), `["go test exit=nil failed=false"]`},
		{"status success printing exited with code", bash("child process exited with code 1\nok\n", "success"), `["go test exit=nil failed=false"]`},
		{"no result yet", `{"v":1,"agent":"codex","type":"assistant","ts":"t","id":"e1","content":[{"id":"e1","input":{"command":"go test ./..."},"name":"Bash","type":"tool_use"}]}`, `["go test exit=nil failed=false"]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, commands, _ := corpusEpisodeOps(c.work, "")
			if got := describeCommands(commands); got != c.want {
				t.Errorf("commands = %s, want %s", got, c.want)
			}
		})
	}
}

// A FileChange gives one file ref per change: add, delete and update map to
// add, delete and edit, and an update with a move_path deletes the old path
// and adds the new one. An absolute path inside the checkout is stored
// relative to it, like an apply_patch path; any other path is kept as is.
func TestCorpusEpisodeOpsCodexItemPathFileChange(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	other := filepath.Join(t.TempDir(), "other-checkout", "x.go")
	in := func(rel string) string { return filepath.Join(repo, filepath.FromSlash(rel)) }
	type change struct {
		Path     string `json:"path"`
		Kind     string `json:"kind,omitempty"`
		MovePath string `json:"move_path,omitempty"`
	}
	fileChange := func(changes ...change) string {
		input, err := json.Marshal(map[string]any{"changes": changes})
		if err != nil {
			t.Fatal(err)
		}
		return `{"v":1,"agent":"codex","type":"assistant","ts":"t","id":"f1","content":[{"id":"f1","input":` + string(input) + `,"name":"FileChange","result":{"output":"","status":"success"},"type":"tool_use"}]}`
	}
	otherJSON, err := json.Marshal([]string{"edit " + other})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, work, root, want string
	}{
		{"update", fileChange(change{Path: in("main.go"), Kind: "update"}), repo, `["edit main.go"]`},
		{"add and delete", fileChange(change{Path: in("docs/new.md"), Kind: "add"}, change{Path: in("old.go"), Kind: "delete"}), repo, `["add docs/new.md","delete old.go"]`},
		{"move", fileChange(change{Path: in("a/old.go"), Kind: "update", MovePath: in("b/new.go")}), repo, `["delete a/old.go","add b/new.go"]`},
		{"unknown kind is an edit", fileChange(change{Path: in("main.go")}), repo, `["edit main.go"]`},
		{"relative path kept", fileChange(change{Path: "internal/x.go", Kind: "update"}), repo, `["edit internal/x.go"]`},
		{"outside the checkout kept", fileChange(change{Path: other, Kind: "update"}), repo, string(otherJSON)},
		{"empty path skipped", fileChange(change{Path: "", Kind: "update"}, change{Path: in("main.go"), Kind: "update"}), repo, `["edit main.go"]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tools, _, files := corpusEpisodeOps(c.work, c.root)
			if len(tools) != 1 || tools[0].name != "filechange" {
				t.Errorf("tools = %+v, want [filechange]", tools)
			}
			if got := describeFileRefs(files); got != c.want {
				t.Errorf("files = %s, want %s", got, c.want)
			}
		})
	}
	// With no checkout known, the absolute path is stored as written.
	_, _, files := corpusEpisodeOps(fileChange(change{Path: in("main.go"), Kind: "update"}), "")
	want, err := json.Marshal([]string{"edit " + in("main.go")})
	if err != nil {
		t.Fatal(err)
	}
	if got := describeFileRefs(files); got != string(want) {
		t.Errorf("no checkout: files = %s, want %s", got, want)
	}
}

// Through the corpus build from a live checkout: the failing go test counts in
// exit_fails, the FileChange of <checkout>/main.go is stored as main.go and
// links to its symbols, and the edit makes the turn a write. A corpus indexed
// by IndexerVersion 11 stored neither, and the transcript bytes do not change
// on upgrade, so only a version bump makes the next build re-index it instead
// of skipping it as unchanged.
func TestPatternCorpusIndexesCodexItemPath(t *testing.T) {
	opts, repo, _, d := newGraphStoreFixture(t)
	runner := opts.Runner.(*fakeCommandRunner)
	d.Header.RepoKey = "gh/example/repo"
	key := semanticStorageKeyForTest(t, runner, opts.Env, repo)
	brain := writeEpisodeFixture(t, opts.Now(), key, []sessionFixture{{id: "s1", agent: "codex", branch: "main", checkpoint: "cp1", relPath: "sessions/main/s1.jsonl", transcript: codexItemPathTranscript(t, repo)}})
	data, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	runner.responses[fakeCommandKey("custom-entire", "graph", "store", "--repo", repo, "--profile", "full")] = fakeCommandResponse{stdout: string(data)}
	graph := patternCorpusGraphContext{Context: context.Background(), Options: opts, RepoDir: repo, Binary: "custom-entire"}

	state := func() string {
		t.Helper()
		db := openCorpusReadSnapshot(t, brain)
		var head, files, shape string
		var exit, failed, exitFails any
		if err := db.QueryRow(`SELECT c.head, c.exit_code, c.failed, e.exit_fails, COALESCE(e.files,''), COALESCE(s.shape,'')
			FROM episode_commands c JOIN episodes e ON e.id = c.episode_id LEFT JOIN episode_shapes s ON s.episode_id = e.id`).Scan(&head, &exit, &failed, &exitFails, &files, &shape); err != nil {
			t.Fatal(err)
		}
		var refs []string
		rows, err := db.Query(`SELECT action || ' ' || path FROM episode_files ORDER BY path`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var r string
			if err := rows.Scan(&r); err != nil {
				t.Fatal(err)
			}
			refs = append(refs, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%s exit_code=%v failed=%v exit_fails=%v files=%s refs=%s shape=%s symbols=%d",
			head, exit, failed, exitFails, files, strings.Join(refs, ","), shape, patterntest.CorpusCount(t, db, "episode_symbols"))
	}
	const want = `go test exit_code=<nil> failed=1 exit_fails=1 files=["main.go"] refs=edit main.go shape=write symbols=2`
	if err := buildPatternCorpus(brain, opts.Now(), graph); err != nil {
		t.Fatal(err)
	}
	if got := state(); got != want {
		t.Fatalf("corpus = %q, want %q", got, want)
	}

	db := openCorpus(t, brain)
	for _, stmt := range []string{
		`UPDATE indexed_sessions SET parser_version=11`,
		`UPDATE meta SET value='11' WHERE key='pattern_indexer_version'`,
		`UPDATE episode_commands SET exit_code=NULL, failed=0`,
		`UPDATE episodes SET exit_fails=0, files=''`,
		`DELETE FROM episode_files`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	db.Close()
	if err := buildPatternCorpus(brain, opts.Now(), graph); err != nil {
		t.Fatal(err)
	}
	if got := state(); got != want {
		t.Fatalf("version 11 corpus kept its missing failure and file ref after rebuild: corpus = %q, want %q", got, want)
	}
}
