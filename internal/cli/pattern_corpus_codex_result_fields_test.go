package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/internal/patterntest"
)

// From cli 0.13.3 a current Codex rollout's compact result carries the
// command's exit code ("exitCode"), whether the user declined the call
// ("declined", with status still "error"), and the call's working directory
// ("cwd": a command's own, a FileChange's turn's).

// codexItemBash is one compact codex item-path command; result is the JSON
// of its result object.
func codexItemBash(id, result string) string {
	return `{"v":1,"agent":"codex","cli_version":"0.13.3","type":"assistant","ts":"t","id":"` + id + `","content":[{"id":"` + id + `","input":{"command":"go test ./..."},"name":"Bash","result":` + result + `,"type":"tool_use"}]}`
}

// codexFileChangeIn is a compact codex FileChange record whose result carries
// cwd as the turn's working directory.
func codexFileChangeIn(t *testing.T, cwd string, changes ...codexChange) string {
	t.Helper()
	input, err := json.Marshal(map[string]any{"changes": changes})
	if err != nil {
		t.Fatal(err)
	}
	result, err := json.Marshal(map[string]any{"output": "", "status": "success", "cwd": cwd})
	if err != nil {
		t.Fatal(err)
	}
	return `{"v":1,"agent":"codex","cli_version":"0.13.3","type":"assistant","ts":"t","id":"f1","content":[{"id":"f1","input":` + string(input) + `,"name":"FileChange","result":` + string(result) + `,"type":"tool_use"}]}`
}

// A format line's result fields reach toolUse as the cli decoded them, and a
// result that predates them leaves them unset.
func TestETFToolUsesCarryCodexResultFields(t *testing.T) {
	describe := func(line string) string {
		t.Helper()
		record, ok := decodeTranscriptRecord(line)
		if !ok {
			t.Fatalf("line did not decode: %s", line)
		}
		var out []string
		for _, use := range recordToolUses(record) {
			exit := "nil"
			if use.exitCode != nil {
				exit = fmt.Sprint(*use.exitCode)
			}
			out = append(out, fmt.Sprintf("%s status=%s exit=%s declined=%v cwd=%q", use.name, use.status, exit, use.declined, use.cwd))
		}
		return strings.Join(out, "; ")
	}
	cases := []struct{ name, line, want string }{
		{"exit code and cwd", codexItemBash("e1", `{"output":"FAIL\n","status":"error","exitCode":2,"cwd":"/w/r/sub"}`), `bash status=error exit=2 declined=false cwd="/w/r/sub"`},
		{"exit code 0", codexItemBash("e1", `{"output":"ok\n","status":"success","exitCode":0}`), `bash status=success exit=0 declined=false cwd=""`},
		{"declined", codexItemBash("e1", `{"output":"","status":"error","declined":true,"cwd":"/w/r"}`), `bash status=error exit=nil declined=true cwd="/w/r"`},
		{"before cli 0.13.3", codexItemBash("e1", `{"output":"FAIL\n","status":"error"}`), `bash status=error exit=nil declined=false cwd=""`},
		{"exit code null", codexItemBash("e1", `{"output":"","status":"error","exitCode":null}`), `bash status=error exit=nil declined=false cwd=""`},
		{"no result", `{"v":1,"agent":"codex","type":"assistant","ts":"t","content":[{"id":"e1","input":{"command":"ls"},"name":"Bash","type":"tool_use"}]}`, `bash status= exit=nil declined=false cwd=""`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := describe(c.line); got != c.want {
				t.Errorf("tool uses = %s, want %s", got, c.want)
			}
		})
	}
}

// A command's exit code is stored as written, and a nonzero one is a failure.
// Status "error" stays Codex's verdict: a command it failed with no exit code
// (one that could not start) or with exit code 0 is still failed. A declined
// command never ran, so it is neither failed nor given an exit code. A
// transcript written before cli 0.13.3 has no exitCode or declined, so its
// status "error" is a failure with no exit code, a declined command included.
func TestCorpusEpisodeOpsCodexResultFields(t *testing.T) {
	cases := []struct{ name, work, want string }{
		{"failing exit code", codexItemBash("e1", `{"output":"--- FAIL: TestX\n","status":"error","exitCode":1}`), `["go test exit=1 failed=true"]`},
		{"passing exit code", codexItemBash("e1", `{"output":"ok\n","status":"success","exitCode":0}`), `["go test exit=0 failed=false"]`},
		{"exit code wins over exit text in output", codexItemBash("e1", `{"output":"Exit code: 0\n","status":"error","exitCode":3}`), `["go test exit=3 failed=true"]`},
		{"exit text in output is not read", codexItemBash("e1", `{"output":"child process exited with code 1\nok\n","status":"success","exitCode":0}`), `["go test exit=0 failed=false"]`},
		{"failed with no exit code", codexItemBash("e1", `{"output":"","status":"error"}`), `["go test exit=nil failed=true"]`},
		{"failed with exit code 0", codexItemBash("e1", `{"output":"","status":"error","exitCode":0}`), `["go test exit=0 failed=true"]`},
		{"declined", codexItemBash("e1", `{"output":"","status":"error","declined":true}`), `["go test exit=nil failed=false"]`},
		{"declined with an exit code", codexItemBash("e1", `{"output":"","status":"error","declined":true,"exitCode":1}`), `["go test exit=nil failed=false"]`},
		{"declined before cli 0.13.3", codexItemBash("e1", `{"output":"","status":"error"}`), `["go test exit=nil failed=true"]`},
		{"response_item path exit code field wins over wrapper text", `{"v":1,"agent":"codex","type":"assistant","ts":"t","content":[{"type":"tool_use","id":"c1","name":"exec_command","input":{"cmd":"go test ./..."},"result":{"output":"Process exited with code 1\nFAIL","status":"success","exitCode":0}}]}`, `["go test exit=0 failed=false"]`},
		{"response_item path declined", `{"v":1,"agent":"codex","type":"assistant","ts":"t","content":[{"type":"tool_use","id":"c1","name":"exec_command","input":{"cmd":"go test ./..."},"result":{"output":"Process exited with code 1\nFAIL","status":"error","declined":true}}]}`, `["go test exit=nil failed=false"]`},
		{"one declined, one failed", strings.Join([]string{
			codexItemBash("e1", `{"output":"","status":"error","declined":true}`),
			codexItemBash("e2", `{"output":"FAIL\n","status":"error","exitCode":1}`),
		}, "\n"), `["go test exit=nil failed=false","go test exit=1 failed=true"]`},
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

// Claude Code and pi results are still left alone: the fields are Codex's,
// and whether their status "error" is a failure is still open (#567).
func TestCorpusEpisodeOpsNonCodexResultFieldsLeftAlone(t *testing.T) {
	for _, agent := range []string{"claude-code", "pi"} {
		t.Run(agent, func(t *testing.T) {
			work := `{"v":1,"agent":"` + agent + `","type":"assistant","ts":"t","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"go test ./..."},"result":{"output":"FAIL","status":"error","exitCode":1}}]}`
			_, commands, _ := corpusEpisodeOps(work, "")
			if got, want := describeCommands(commands), `["go test exit=nil failed=false"]`; got != want {
				t.Errorf("commands = %s, want %s", got, want)
			}
		})
	}
}

// With the turn's cwd on a FileChange result, the root is that directory or
// one of its parents, so paths outside them can't give it: a tie the paths
// alone leave ambiguous is settled, and a checkout at the filesystem root is
// found. A cwd that is relative or has a ".." segment says nothing, and the
// paths alone decide, as for a session written before cli 0.13.3.
func TestCodexSessionRootFromCWD(t *testing.T) {
	lines := func(records ...string) string { return strings.Join(records, "\n") + "\n" }
	cases := []struct {
		name, transcript string
		touched          []string
		want             string
	}{
		{"cwd is the root", lines(codexFileChangeIn(t, "/w/r", codexChange{Path: "/w/r/main.go", Kind: "update"})), []string{"main.go"}, "/w/r"},
		{"cwd in a subdirectory gives its parent", lines(codexFileChangeIn(t, "/w/r/sub", codexChange{Path: "/w/r/sub/main.go", Kind: "update"})), []string{"sub/main.go"}, "/w/r"},
		{"a tie with an unrelated root is settled", lines(
			codexFileChangeIn(t, "/work/repo", codexChange{Path: "/work/repo/main.go", Kind: "update"}),
			codexFileChangeIn(t, "/work/repo", codexChange{Path: "/tmp/job/main.go", Kind: "update"}, codexChange{Path: "/tmp/job/scratch.txt", Kind: "add"}),
		), []string{"main.go"}, "/work/repo"},
		{"a path ending in two touched files is settled", lines(codexFileChangeIn(t, "/w/r", codexChange{Path: "/w/r/cmd/main.go", Kind: "update"})), []string{"main.go", "cmd/main.go"}, "/w/r"},
		{"a tie between cwd and its parent stays ambiguous", lines(codexFileChangeIn(t, "/w/r/cmd", codexChange{Path: "/w/r/cmd/main.go", Kind: "update"})), []string{"main.go", "cmd/main.go"}, ""},
		{"a path outside the cwd gives no root", lines(codexFileChangeIn(t, "/w/r", codexChange{Path: "/tmp/job/main.go", Kind: "update"})), []string{"main.go"}, ""},
		{"a checkout below the cwd gives no root", lines(codexFileChangeIn(t, "/w", codexChange{Path: "/w/r/main.go", Kind: "update"})), []string{"main.go"}, ""},
		{"a checkout at the filesystem root", lines(codexFileChangeIn(t, "/", codexChange{Path: "/main.go", Kind: "update"})), []string{"main.go"}, "/"},
		{"a checkout at the filesystem root, cwd below it", lines(codexFileChangeIn(t, "/src", codexChange{Path: "/src/main.go", Kind: "update"})), []string{"src/main.go"}, "/"},
		{"windows cwd", lines(codexFileChangeIn(t, `C:\src\repo`, codexChange{Path: `C:\src\repo\pkg\a.go`, Kind: "update"})), []string{"pkg/a.go"}, "C:/src/repo"},
		{"windows drive root", lines(codexFileChangeIn(t, `C:\`, codexChange{Path: `C:\main.go`, Kind: "update"})), []string{"main.go"}, "C:"},
		{"a FileChange with no result follows the session's cwd", lines(
			codexFileChangeIn(t, "/work/repo", codexChange{Path: "/work/repo/main.go", Kind: "update"}),
			`{"v":1,"agent":"codex","type":"assistant","ts":"t","id":"f2","content":[{"id":"f2","input":{"changes":[{"path":"/tmp/job/main.go","kind":"update"}]},"name":"FileChange","type":"tool_use"}]}`,
		), []string{"main.go"}, "/work/repo"},
		{"two cwds", lines(
			codexFileChangeIn(t, "/w/r/a", codexChange{Path: "/w/r/a/x.go", Kind: "update"}),
			codexFileChangeIn(t, "/w/r/b", codexChange{Path: "/w/r/b/y.go", Kind: "update"}),
		), []string{"a/x.go", "b/y.go"}, "/w/r"},
		{"a .. path still gives no root", lines(codexFileChangeIn(t, "/w/r", codexChange{Path: "/w/r/x/../main.go", Kind: "update"})), []string{"main.go"}, ""},
		{"a relative cwd says nothing", lines(
			codexFileChangeIn(t, "repo", codexChange{Path: "/work/repo/main.go", Kind: "update"}),
			codexFileChangeIn(t, "repo", codexChange{Path: "/tmp/job/main.go", Kind: "update"}),
		), []string{"main.go"}, ""},
		{"a .. cwd says nothing", lines(codexFileChangeIn(t, "/w/x/../r", codexChange{Path: "/w/r/main.go", Kind: "update"})), []string{"main.go"}, "/w/r"},
		{"no files touched", lines(codexFileChangeIn(t, "/w/r", codexChange{Path: "/w/r/main.go", Kind: "update"})), nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := codexSessionRoot(c.transcript, c.touched); got != c.want {
				t.Errorf("root = %q, want %q", got, c.want)
			}
		})
	}
}

// A root of "/" makes a rooted path relative like any other root.
func TestCorpusEpisodeOpsFileChangeUnderFilesystemRoot(t *testing.T) {
	work := codexFileChangeIn(t, "/", codexChange{Path: "/main.go", Kind: "update"}, codexChange{Path: "/pkg/a.go", Kind: "add"})
	_, _, files := corpusEpisodeOps(work, "/")
	if got, want := describeFileRefs(files), `["edit main.go","add pkg/a.go"]`; got != want {
		t.Errorf("files = %s, want %s", got, want)
	}
}

// codexResultFieldsTranscript is a session as cli 0.13.3 writes a current
// Codex rollout: the user declines a go test, the next one fails with exit
// code 1, and main.go is edited along with a scratch copy under /tmp, all in a
// turn whose cwd is a subdirectory of the checkout at root.
func codexResultFieldsTranscript(t *testing.T, root string) string {
	t.Helper()
	cwd, err := json.Marshal(root + "/cmd")
	if err != nil {
		t.Fatal(err)
	}
	return `{"v":1,"agent":"codex","cli_version":"0.13.3","type":"user","ts":"2030-01-01T00:00:01.000Z","id":"u1","content":[{"text":"Fix the failing test in main.go"}]}
` + codexItemBash("e1", `{"output":"","status":"error","declined":true,"cwd":`+string(cwd)+`}`) + `
` + codexItemBash("e2", `{"output":"--- FAIL: TestX\nFAIL\n","status":"error","exitCode":1,"cwd":`+string(cwd)+`}`) + `
` + codexFileChangeIn(t, root+"/cmd", codexChange{Path: root + "/main.go", Kind: "update"}, codexChange{Path: "/tmp/job/main.go", Kind: "update"}) + `
{"v":1,"agent":"codex","cli_version":"0.13.3","type":"assistant","ts":"2030-01-01T00:00:04.000Z","id":"a1","content":[{"type":"text","text":"Fixed main.go."}]}
`
}

// Through the corpus build: the declined go test is stored, not failed and
// with no exit code; the failing one keeps its exit code 1; exit_fails counts
// only it; and main.go is stored relative to the checkout found from the
// turn's cwd, so it links to its 2 symbols. The paths alone would tie that
// checkout with /tmp/job, which the cwd rules out. A corpus indexed by
// IndexerVersion 13 counted the declined command as failed and stored no exit
// code, and the transcript bytes do not change on upgrade, so only a version
// bump makes the next build re-index it instead of skipping it as unchanged.
func TestPatternCorpusIndexesCodexResultFields(t *testing.T) {
	c := newCodexItemPathCorpus(t, sessionFixture{id: "s1", agent: "codex", branch: "main", checkpoint: "cp1", relPath: "sessions/main/s1.jsonl", transcript: codexResultFieldsTranscript(t, "/home/teammate/src/repo"), filesTouched: []string{"main.go", "cmd/main.go"}})
	const want = `commands=[exit_code=<nil> failed=0; exit_code=1 failed=1] n_cmds=2 exit_fails=1 files=["/tmp/job/main.go","main.go"] symbols=2`
	c.build(t)
	if got := codexResultFieldsCorpusState(t, c.brain); got != want {
		t.Fatalf("corpus = %q, want %q", got, want)
	}

	db := openCorpus(t, c.brain)
	for _, stmt := range []string{
		`UPDATE indexed_sessions SET parser_version=13`,
		`UPDATE meta SET value='13' WHERE key='pattern_indexer_version'`,
		`UPDATE episode_commands SET exit_code=NULL, failed=1`,
		`UPDATE episodes SET exit_fails=2`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	db.Close()
	c.build(t)
	if got := codexResultFieldsCorpusState(t, c.brain); got != want {
		t.Fatalf("version 13 corpus kept its declined failure after rebuild: corpus = %q, want %q", got, want)
	}
}

// codexResultFieldsCorpusState describes the corpus's commands, in order, and
// its one episode's command count, exit_fails, files and symbol links.
func codexResultFieldsCorpusState(t *testing.T, brain string) string {
	t.Helper()
	db := openCorpusReadSnapshot(t, brain)
	rows, err := db.Query(`SELECT exit_code, failed FROM episode_commands ORDER BY ord`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var cmds []string
	for rows.Next() {
		var exit, failed any
		if err := rows.Scan(&exit, &failed); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, fmt.Sprintf("exit_code=%v failed=%v", exit, failed))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	var exitFails, nCmds any
	var files string
	if err := db.QueryRow(`SELECT exit_fails, n_cmds, COALESCE(files,'') FROM episodes`).Scan(&exitFails, &nCmds, &files); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("commands=[%s] n_cmds=%v exit_fails=%v files=%s symbols=%d", strings.Join(cmds, "; "), nCmds, exitFails, files, patterntest.CorpusCount(t, db, "episode_symbols"))
}
