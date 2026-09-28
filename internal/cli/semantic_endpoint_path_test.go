package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// TestSemanticEndpointPathRecoversFieldsAroundColons pins the endpoint parse
// against every ID shape entire-graph emits, including the ones where a field
// legitimately contains ":" — a file path, a local repo key, and a route
// symbol's qualified name.
func TestSemanticEndpointPathRecoversFieldsAroundColons(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		repoKey string
		id      string
		want    string
	}{
		{"symbol", "gh/entireio/entire-brain", "gh/entireio/entire-brain:Go:cmd/entire-brain/main.go:function:main", "cmd/entire-brain/main.go"},
		{"symbol lowercase language", "gh/example/repo", "gh/example/repo:go:secret/config.go:function:Secret", "secret/config.go"},
		{"kind marker in path before future kind", "gh/example/repo", "gh/example/repo:Go:dir:function:file.go:new_kind:F", ""},
		{"ambiguous qualified name", "gh/example/repo", "gh/example/repo:Go:file.go:function:Name:Part", ""},
		{"future kind", "gh/example/repo", "gh/example/repo:Go:main.go:future_kind:F", "main.go"},
		{"multiple colons in file path", "gh/example/repo", "gh/example/repo:Go:dir:a:b/file.go:function:F", "dir:a:b/file.go"},
		{"ambiguous kind markers", "gh/example/repo", "gh/example/repo:Go:dir:function:file.go:function:F", ""},
		{"colon in file path", "gh/example/repo", "gh/example/repo:Python:od:d/mod.py:function:f", "od:d/mod.py"},
		{"colon in file path, dotted leaf", "gh/example/repo", "gh/example/repo:Python:od:mod.py:function:f", "od:mod.py"},
		{"colon in repo key", "local/My:Repo", "local/My:Repo:Go:main.go:function:main", "main.go"},
		{"colon in qualified name", "gh/example/repo", "gh/example/repo:JavaScript:src/routes.js:route:GET /users/:id", "src/routes.js"},
		{"colon in path and qualified name", "gh/example/repo", "gh/example/repo:JavaScript:od:d/routes.js:route:GET /users/:id", "od:d/routes.js"},
		{"file endpoint", "gh/example/repo", "gh/example/repo:file:internal/auth/token.go", "internal/auth/token.go"},
		{"file endpoint with colon in path", "gh/example/repo", "gh/example/repo:file:od:d/mod.py", "od:d/mod.py"},
		{"external import", "gh/example/repo", "external:import:archive/tar", ""},
		{"external route", "gh/example/repo", "external:route:/repo", ""},
		{"endpoint from another repo", "gh/example/repo", "gh/other/repo:Go:main.go:function:main", ""},
		{"opaque endpoint", "gh/example/repo", "public", ""},
		{"empty id", "gh/example/repo", "", ""},
		{"unknown repo key", "", "gh/example/repo:Go:main.go:function:main", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := semanticEndpointPath(tc.repoKey, tc.id); got != tc.want {
				t.Fatalf("semanticEndpointPath(%q, %q) = %q, want %q", tc.repoKey, tc.id, got, tc.want)
			}
		})
	}
}

// semanticColonPathSnapshot is a provider snapshot whose Python file lives at a
// path containing ":" — legal on every filesystem Brain supports. Nothing in it
// is ignored by "od": the real path is "od:d/mod.py", not "od/…".
func semanticColonPathSnapshot() string {
	return `{"schema_version":"1.0","provider":"entire-graph","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111","capabilities":["go"],"warnings":[],"partial_failures":[]}
{"record_type":"file","id":"gh/example/repo:file:od:d/mod.py","path":"od:d/mod.py","blob":"abc123","language":"Python"}
{"record_type":"symbol","id":"gh/example/repo:Python:od:d/mod.py:function:load","kind":"function","name":"load","qualified_name":"load","file_path":"od:d/mod.py","start_line":1,"end_line":4,"language":"Python","stable_id_version":"1"}
{"record_type":"symbol","id":"gh/example/repo:Go:internal/auth/token.go:function:auth.ValidateToken","kind":"function","name":"ValidateToken","qualified_name":"auth.ValidateToken","file_path":"internal/auth/token.go","start_line":10,"end_line":20,"language":"Go","stable_id_version":"1"}
{"record_type":"relation","from_id":"gh/example/repo:Go:internal/auth/token.go:function:auth.ValidateToken","to_id":"gh/example/repo:Python:od:d/mod.py:function:load","type":"CALLS","confidence":1}
{"record_type":"summary","warnings":[],"partial_failures":[]}
`
}

// semanticIgnoredFileEndpointSnapshot references two ignored files only through
// file endpoint IDs. entire-graph resolves imports and co-change pairs to
// "<repo-key>:file:<path>" endpoints for files it never emits a record for, so
// the ignored-ID set cannot see them and the endpoint ID is the only signal
// that the relation touches an ignored file.
func semanticIgnoredFileEndpointSnapshot() string {
	return `{"schema_version":"1.0","provider":"entire-graph","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111","capabilities":["go"],"warnings":[],"partial_failures":[]}
{"record_type":"file","id":"gh/example/repo:file:internal/auth/token.go","path":"internal/auth/token.go","blob":"abc123","language":"Go"}
{"record_type":"relation","from_id":"gh/example/repo:file:internal/auth/token.go","to_id":"gh/example/repo:file:secret/config.go","type":"IMPORTS","confidence":1}
{"record_type":"relation","from_id":"gh/example/repo:file:internal/auth/token.go","to_id":"gh/example/repo:file:od:d/secret.py","type":"IMPORTS","confidence":1}
{"record_type":"summary","warnings":[],"partial_failures":[]}
`
}

func semanticEndpointPathIndex(t *testing.T, brainignore, snapshot string) (*exportManifest, string) {
	t.Helper()
	repoDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoDir, ".brainignore"), []byte(brainignore), 0o600); err != nil {
		t.Fatalf("write .brainignore: %v", err)
	}
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, snapshot)
	cmd := &cobra.Command{Use: "index"}
	if err := runSemanticIndex(cmd.Context(), cmd, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(manifest.Sources.Semantic.SnapshotPath)))
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	return manifest, string(data)
}

// A ".brainignore" rule for the directory "od" must not redact a file named
// "od:d/mod.py", which is not inside it. Reading the path out of a fixed
// colon-separated field of the symbol ID saw "od" and dropped every relation
// touching the file, while keeping the file and its symbols — a silent,
// one-sided loss of the relation graph.
func TestSemanticIndexKeepsRelationsForColonPathOutsideIgnoredDirectory(t *testing.T) {
	manifest, snapshot := semanticEndpointPathIndex(t, "od\n", semanticColonPathSnapshot())
	if manifest.Sources.Semantic.Symbols != 2 {
		t.Fatalf("symbols in a non-ignored file were dropped: %+v", manifest.Sources.Semantic)
	}
	if manifest.Sources.Semantic.Relations != 1 {
		t.Fatalf("relation between two non-ignored files was redacted: %+v", manifest.Sources.Semantic)
	}
	if !strings.Contains(snapshot, `"type":"CALLS"`) {
		t.Fatalf("relation missing from the persisted snapshot:\n%s", snapshot)
	}
}

// The same file path, now genuinely ignored, must be redacted.
func TestSemanticIndexDropsRelationsForIgnoredColonPath(t *testing.T) {
	manifest, snapshot := semanticEndpointPathIndex(t, "od:d/\n", semanticColonPathSnapshot())
	if manifest.Sources.Semantic.Relations != 0 {
		t.Fatalf("relation touching an ignored file survived: %+v", manifest.Sources.Semantic)
	}
	if strings.Contains(snapshot, "od:d/mod.py") {
		t.Fatalf("ignored path leaked into the snapshot:\n%s", snapshot)
	}
}

// A file endpoint ID has three colon-separated fields, so the old "at least
// five fields" guard skipped every one of them: a relation naming an ignored
// file through "<repo-key>:file:<path>" was written into the brain verbatim.
func TestSemanticIndexDropsRelationsToIgnoredFileEndpoints(t *testing.T) {
	manifest, snapshot := semanticEndpointPathIndex(t, "secret/\nod:d/\n", semanticIgnoredFileEndpointSnapshot())
	if manifest.Sources.Semantic.Relations != 0 {
		t.Fatalf("relations to ignored file endpoints survived: %+v", manifest.Sources.Semantic)
	}
	if strings.Contains(snapshot, "secret/config.go") || strings.Contains(snapshot, "od:d/secret.py") {
		t.Fatalf("ignored path leaked into the snapshot:\n%s", snapshot)
	}
}

// filterSemanticSnapshot re-filters an already persisted snapshot and shares the
// endpoint parse, so it must reach the same verdicts.
func TestFilterSemanticSnapshotHonorsColonPathEndpoints(t *testing.T) {
	t.Parallel()
	kept := brainIgnore{patterns: []string{"od"}}
	_, counts, _, err := filterSemanticSnapshot([]byte(semanticColonPathSnapshot()), kept, t.TempDir())
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if counts.Relations != 1 {
		t.Fatalf("relation between two non-ignored files was redacted: %+v", counts)
	}

	ignored := brainIgnore{patterns: []string{"od:d/"}}
	_, counts, filtered, err := filterSemanticSnapshot([]byte(semanticColonPathSnapshot()), ignored, t.TempDir())
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if counts.Relations != 0 {
		t.Fatalf("relation touching an ignored file survived: %+v", counts)
	}
	if strings.Contains(string(filtered), "od:d/mod.py") {
		t.Fatalf("ignored path leaked into the filtered snapshot:\n%s", filtered)
	}
}

func TestSemanticMultipleColonPathIgnoreDecisions(t *testing.T) {
	snapshot := strings.ReplaceAll(semanticColonPathSnapshot(), "od:d/mod.py", "dir:a:b/file.go")
	manifest, _ := semanticEndpointPathIndex(t, "dir\n", snapshot)
	if manifest.Sources.Semantic.Relations == 0 {
		t.Fatal("allowed relation was dropped")
	}
	manifest, _ = semanticEndpointPathIndex(t, "dir:a:b/\n", snapshot)
	if manifest.Sources.Semantic.Relations != 0 {
		t.Fatal("ignored relation retained")
	}
}

func TestSemanticDuplicateIDCannotClearIgnoreVerdict(t *testing.T) {
	lines := strings.Split(semanticColonPathSnapshot(), "\n")
	duplicate := strings.Replace(lines[2], `"file_path":"od:d/mod.py"`, `"file_path":"secret/hidden.py"`, 1)
	snapshot := lines[0] + "\n" + duplicate + "\n" + strings.Join(lines[1:], "\n")
	manifest, _ := semanticEndpointPathIndex(t, "secret/\n", snapshot)
	if manifest.Sources.Semantic.Relations != 0 {
		t.Fatal("streaming ingest cleared the earlier ignore verdict")
	}
	_, counts, _, err := filterSemanticSnapshot([]byte(snapshot), brainIgnore{patterns: []string{"secret/"}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if counts.Relations != 0 {
		t.Fatal("snapshot filtering cleared the earlier ignore verdict")
	}
}
