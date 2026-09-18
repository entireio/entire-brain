package cli

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspacePatternStatusRejectsMalformedLegacyState(t *testing.T) {
	for _, tc := range []struct {
		name string
		rel  string
		want string
	}{
		{name: "procedures", rel: patternsProceduresPath, want: "parse procedure line"},
		{name: "practices", rel: patternsPracticesPath, want: "parse practice line"},
		{name: "runs", rel: patternRunsRelPath, want: "parse pattern run history"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, _, manifest := workspaceCommandFixture(t)
			wsDir, err := workspaceDir(opts.Env, manifest.Name)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(wsDir, filepath.FromSlash(tc.rel))
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("{broken\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := execute(t, NewRootCommand(opts), "workspace", "patterns", "status", manifest.Name)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("status err=%v, want %q\n%s", err, tc.want, out)
			}
			if strings.Contains(out, "workspace:") {
				t.Fatalf("malformed %s emitted partial success summary: %q", tc.name, out)
			}
		})
	}
}

func TestWorkspacePatternStatusCountsMembersWithLegacyPatterns(t *testing.T) {
	opts, env, manifest := workspaceCommandFixture(t)
	memberDir, err := brainDirForKey(env, manifest.Repos[0].RepoKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeBrainProceduresFile(memberDir, []procedureRecord{{ID: "procedure:member", Type: "procedure", Scope: "repo"}}); err != nil {
		t.Fatal(err)
	}
	out, err := execute(t, NewRootCommand(opts), "workspace", "patterns", "status", manifest.Name)
	if err != nil || !strings.Contains(out, "member repos: 2 (1 with patterns)") {
		t.Fatalf("status err=%v\n%s", err, out)
	}
}

func TestWorkspacePatternRefreshWriteFailuresPreserveUnrelatedState(t *testing.T) {
	for _, tc := range []struct {
		name string
		rel  string
		want string
	}{
		{name: "procedures", rel: patternsProceduresPath, want: "write procedures"},
		{name: "practices", rel: patternsPracticesPath, want: "write practices"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, _, manifest := workspaceCommandFixture(t)
			wsDir, err := workspaceDir(opts.Env, manifest.Name)
			if err != nil {
				t.Fatal(err)
			}
			sentinelPath := filepath.Join(wsDir, "unrelated.keep")
			const sentinel = "preserve this state"
			if err := os.MkdirAll(wsDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(sentinelPath, []byte(sentinel), 0o600); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(wsDir, filepath.FromSlash(tc.rel))
			if err := os.MkdirAll(destination, 0o700); err != nil {
				t.Fatal(err)
			}
			out, err := execute(t, NewRootCommand(opts), "workspace", "patterns", "refresh", manifest.Name)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("refresh err=%v, want %q\n%s", err, tc.want, out)
			}
			got, readErr := os.ReadFile(sentinelPath)
			if readErr != nil || string(got) != sentinel {
				t.Fatalf("unrelated state changed: %q err=%v", got, readErr)
			}
			if strings.Contains(out, "cross-repo V2 pattern") {
				t.Fatalf("failed refresh emitted success output: %q", out)
			}
		})
	}
}

func TestWorkspaceSkillFormRejectsUnreadableFamilyTableBeforeProvider(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	opts, _, manifest := workspaceCommandFixture(t)
	wsDir, err := workspaceDir(opts.Env, manifest.Name)
	if err != nil {
		t.Fatal(err)
	}
	corpus := filepath.Join(wsDir, filepath.FromSlash(patternCorpusPath))
	if err := os.Remove(corpus); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(sqliteDriverName, corpus)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE unrelated(id TEXT)`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := execute(t, NewRootCommand(opts), "workspace", "patterns", "skills", "form", manifest.Name, "family:any", "--agent", "codex", "--yes")
	if err == nil || !strings.Contains(err.Error(), "deep_dossiers") {
		t.Fatalf("form err=%v\n%s", err, out)
	}
	if out != "" {
		t.Fatalf("family lookup failure emitted output: %q", out)
	}
	assertWorkspaceSkillNotWritten(t, wsDir)
}
