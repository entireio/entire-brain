package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/patterntest"
)

// A current Codex rollout reaches the brain through the cli's item path
// (entireio/cli#2897): a command is a tool_use named "Bash" that carries the
// command's own output and marks a failure only as status "error", and an edit
// is a tool_use named "FileChange" with absolute paths. These are the lines
// the cli's converter writes for a failing go test and an edit of main.go in
// the checkout at root.
func codexItemPathTranscript(t *testing.T, root string) string {
	t.Helper()
	path, err := json.Marshal(root + "/main.go")
	if err != nil {
		t.Fatal(err)
	}
	return `{"v":1,"agent":"codex","cli_version":"0.13.1","type":"user","ts":"2030-01-01T00:00:01.000Z","id":"u1","content":[{"text":"Fix the failing test in main.go"}]}
{"v":1,"agent":"codex","cli_version":"0.13.1","type":"assistant","ts":"2030-01-01T00:00:02.000Z","id":"e1","content":[{"id":"e1","input":{"command":"go test ./..."},"name":"Bash","result":{"output":"--- FAIL: TestX\nFAIL\n","status":"error"},"type":"tool_use"}]}
{"v":1,"agent":"codex","cli_version":"0.13.1","type":"assistant","ts":"2030-01-01T00:00:03.000Z","id":"f1","content":[{"id":"f1","input":{"changes":[{"path":` + string(path) + `,"kind":"update","diff":"@@ -1 +1 @@\n-a\n+b\n"}]},"name":"FileChange","result":{"output":"","status":"success"},"type":"tool_use"}]}
{"v":1,"agent":"codex","cli_version":"0.13.1","type":"assistant","ts":"2030-01-01T00:00:04.000Z","id":"a1","content":[{"type":"text","text":"Fixed main.go."}]}
`
}

type codexChange struct {
	Path     string `json:"path"`
	Kind     string `json:"kind,omitempty"`
	MovePath string `json:"move_path,omitempty"`
}

// codexFileChange is one compact codex FileChange record.
func codexFileChange(t *testing.T, changes ...codexChange) string {
	t.Helper()
	input, err := json.Marshal(map[string]any{"changes": changes})
	if err != nil {
		t.Fatal(err)
	}
	return `{"v":1,"agent":"codex","type":"assistant","ts":"t","id":"f1","content":[{"id":"f1","input":` + string(input) + `,"name":"FileChange","result":{"output":"","status":"success"},"type":"tool_use"}]}`
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
// and adds the new one. An absolute path inside the session's root is stored
// relative to it, like an apply_patch path; any other path is kept as is.
func TestCorpusEpisodeOpsCodexItemPathFileChange(t *testing.T) {
	const root = "/work/repo"
	cases := []struct {
		name, work, root, want string
	}{
		{"update", codexFileChange(t, codexChange{Path: root + "/main.go", Kind: "update"}), root, `["edit main.go"]`},
		{"add and delete", codexFileChange(t, codexChange{Path: root + "/docs/new.md", Kind: "add"}, codexChange{Path: root + "/old.go", Kind: "delete"}), root, `["add docs/new.md","delete old.go"]`},
		{"move", codexFileChange(t, codexChange{Path: root + "/a/old.go", Kind: "update", MovePath: root + "/b/new.go"}), root, `["delete a/old.go","add b/new.go"]`},
		{"unknown kind is an edit", codexFileChange(t, codexChange{Path: root + "/main.go"}), root, `["edit main.go"]`},
		{"relative path kept", codexFileChange(t, codexChange{Path: "internal/x.go", Kind: "update"}), root, `["edit internal/x.go"]`},
		{"outside the root kept", codexFileChange(t, codexChange{Path: "/elsewhere/x.go", Kind: "update"}), root, `["edit /elsewhere/x.go"]`},
		{"root prefix is not a parent", codexFileChange(t, codexChange{Path: "/work/repo2/x.go", Kind: "update"}), root, `["edit /work/repo2/x.go"]`},
		{"empty path skipped", codexFileChange(t, codexChange{Path: "", Kind: "update"}, codexChange{Path: root + "/main.go", Kind: "update"}), root, `["edit main.go"]`},
		{"windows path", codexFileChange(t, codexChange{Path: `C:\work\repo\pkg\a.go`, Kind: "update"}), "C:/work/repo", `["edit pkg/a.go"]`},
		{"dot segments resolved", codexFileChange(t, codexChange{Path: root + "/dir/../main.go", Kind: "update"}), root, `["edit main.go"]`},
		{"dot segments leaving the root kept", codexFileChange(t, codexChange{Path: root + "/../outside/x.go", Kind: "update"}), root, `["edit /work/repo/../outside/x.go"]`},
		{"no root known", codexFileChange(t, codexChange{Path: root + "/main.go", Kind: "update"}), "", `["edit /work/repo/main.go"]`},
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
}

// The root comes from the session itself: an absolute FileChange path that
// ends in one of the checkpoint's repo-relative files_touched. It does not
// depend on where the corpus is built, and an ambiguous session gets none.
func TestCodexSessionRoot(t *testing.T) {
	lines := func(records ...string) string { return strings.Join(records, "\n") + "\n" }
	cases := []struct {
		name, transcript string
		touched          []string
		want             string
	}{
		{"one touched file", lines(codexFileChange(t, codexChange{Path: "/home/ada/src/repo/main.go", Kind: "update"})), []string{"main.go"}, "/home/ada/src/repo"},
		{"untouched paths follow the session root", lines(codexFileChange(t, codexChange{Path: "/w/r/cmd/x.go", Kind: "update"}, codexChange{Path: "/w/r/scratch.txt", Kind: "add"})), []string{"cmd/x.go"}, "/w/r"},
		{"one path ending in two touched files is ambiguous", lines(codexFileChange(t, codexChange{Path: "/w/r/cmd/main.go", Kind: "update"})), []string{"main.go", "cmd/main.go"}, ""},
		{"checkout named like a repo directory", lines(codexFileChange(t, codexChange{Path: "/work/repo/main.go", Kind: "update"}, codexChange{Path: "/work/repo/repo/main.go", Kind: "update"})), []string{"main.go", "repo/main.go"}, "/work/repo"},
		{"repeated edits of one file count once", lines(
			codexFileChange(t, codexChange{Path: "/work/app/main.go", Kind: "update"}),
			codexFileChange(t, codexChange{Path: "/work/app/main.go", Kind: "update"}),
			codexFileChange(t, codexChange{Path: "/work/app/app/main.go", Kind: "update"}),
		), []string{"main.go", "app/main.go"}, "/work/app"},
		{"untouched edits do not pull the root down", lines(
			codexFileChange(t, codexChange{Path: "/repo/main.go", Kind: "update"}),
			codexFileChange(t, codexChange{Path: "/repo/sub/main.go", Kind: "update"}),
			codexFileChange(t, codexChange{Path: "/repo/sub/main.go", Kind: "update"}),
		), []string{"main.go"}, "/repo"},
		{"dot segments resolved", lines(codexFileChange(t, codexChange{Path: "/w/r/x/../main.go", Kind: "update"})), []string{"main.go"}, "/w/r"},
		{"most paths agree", lines(
			codexFileChange(t, codexChange{Path: "/w/r/a/main.go", Kind: "update"}),
			codexFileChange(t, codexChange{Path: "/w/r/b.go", Kind: "update"}, codexChange{Path: "/w/r/c.go", Kind: "update"}),
		), []string{"main.go", "b.go", "c.go"}, "/w/r"},
		{"move path votes", lines(codexFileChange(t, codexChange{Path: "/w/r/old.go", Kind: "update", MovePath: "/w/r/pkg/new.go"})), []string{"pkg/new.go"}, "/w/r"},
		{"windows paths", lines(codexFileChange(t, codexChange{Path: `C:\src\repo\pkg\a.go`, Kind: "update"})), []string{"pkg/a.go"}, "C:/src/repo"},
		{"touched spelled with ./", lines(codexFileChange(t, codexChange{Path: "/w/r/main.go", Kind: "update"})), []string{"./main.go"}, "/w/r"},
		{"no files touched", lines(codexFileChange(t, codexChange{Path: "/w/r/main.go", Kind: "update"})), nil, ""},
		{"no touched file matches", lines(codexFileChange(t, codexChange{Path: "/w/r/main.go", Kind: "update"})), []string{"other.go"}, ""},
		{"relative paths give no root", lines(codexFileChange(t, codexChange{Path: "main.go", Kind: "update"})), []string{"main.go"}, ""},
		{"no FileChange", `{"v":1,"agent":"codex","type":"assistant","ts":"t","content":[{"type":"text","text":"edited /w/r/main.go"}]}` + "\n", []string{"main.go"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := codexSessionRoot(c.transcript, c.touched); got != c.want {
				t.Errorf("root = %q, want %q", got, c.want)
			}
		})
	}
}

// codexItemPathCorpus is a brain with codex item-path sessions and a live
// graph context whose semantic index has main.go's 2 symbols.
type codexItemPathCorpus struct {
	brain, key string
	now        time.Time
	graph      patternCorpusGraphContext
}

func newCodexItemPathCorpus(t *testing.T, sessions ...sessionFixture) codexItemPathCorpus {
	t.Helper()
	opts, repo, _, d := newGraphStoreFixture(t)
	runner := opts.Runner.(*fakeCommandRunner)
	d.Header.RepoKey = "gh/example/repo"
	c := codexItemPathCorpus{key: semanticStorageKeyForTest(t, runner, opts.Env, repo), now: opts.Now()}
	c.brain = writeEpisodeFixture(t, c.now, c.key, sessions)
	data, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	runner.responses[fakeCommandKey("custom-entire", "graph", "store", "--repo", repo, "--profile", "full")] = fakeCommandResponse{stdout: string(data)}
	c.graph = patternCorpusGraphContext{Context: context.Background(), Options: opts, RepoDir: repo, Binary: "custom-entire"}
	return c
}

func (c codexItemPathCorpus) build(t *testing.T) {
	t.Helper()
	if err := buildPatternCorpus(c.brain, c.now, c.graph); err != nil {
		t.Fatal(err)
	}
}

// state describes the corpus's one command, its episode, file refs and
// symbol links.
func (c codexItemPathCorpus) state(t *testing.T) string {
	t.Helper()
	db := openCorpusReadSnapshot(t, c.brain)
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

const codexItemPathCorpusWant = `go test exit_code=<nil> failed=1 exit_fails=1 files=["main.go"] refs=edit main.go shape=write symbols=2`

// Through the corpus build: the failing go test counts in exit_fails, and the
// FileChange of main.go is stored as main.go, links to its symbols, and makes
// the turn a write. The session ran in a checkout other than the one building
// the corpus (a teammate's machine, or another worktree), which does not
// matter: the root comes from its checkpoint's files_touched. A corpus indexed
// by IndexerVersion 11 stored neither, and the transcript bytes do not change
// on upgrade, so only a version bump makes the next build re-index it instead
// of skipping it as unchanged.
func TestPatternCorpusIndexesCodexItemPath(t *testing.T) {
	c := newCodexItemPathCorpus(t, sessionFixture{id: "s1", agent: "codex", branch: "main", checkpoint: "cp1", relPath: "sessions/main/s1.jsonl", transcript: codexItemPathTranscript(t, "/home/teammate/src/repo"), filesTouched: []string{"main.go"}})
	brain := c.brain
	c.build(t)
	if got := c.state(t); got != codexItemPathCorpusWant {
		t.Fatalf("corpus = %q, want %q", got, codexItemPathCorpusWant)
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
	c.build(t)
	if got := c.state(t); got != codexItemPathCorpusWant {
		t.Fatalf("version 11 corpus kept its missing failure and file ref after rebuild: corpus = %q, want %q", got, codexItemPathCorpusWant)
	}
}

// files_touched belongs to the checkpoint a session was exported from, and a
// later checkpoint of the same transcript can carry files the earlier one did
// not. A session first indexed without a root is re-indexed once its
// checkpoint changes, though its transcript did not.
func TestPatternCorpusReindexesCodexSessionForNewCheckpoint(t *testing.T) {
	s := sessionFixture{id: "s1", agent: "codex", branch: "main", checkpoint: "cp1", relPath: "sessions/main/s1.jsonl", transcript: codexItemPathTranscript(t, "/srv/build/repo")}
	c := newCodexItemPathCorpus(t, s)
	c.build(t)
	const noRoot = `go test exit_code=<nil> failed=1 exit_fails=1 files=["/srv/build/repo/main.go"] refs=edit /srv/build/repo/main.go shape=write symbols=0`
	if got := c.state(t); got != noRoot {
		t.Fatalf("corpus with no files_touched = %q, want %q", got, noRoot)
	}

	s.checkpoint, s.filesTouched = "cp2", []string{"main.go"}
	writeEpisodeFixtureManifest(t, c.brain, c.now, c.key, []sessionFixture{s})
	c.build(t)
	if got := c.state(t); got != codexItemPathCorpusWant {
		t.Fatalf("new checkpoint left the session as first indexed: corpus = %q, want %q", got, codexItemPathCorpusWant)
	}
}
