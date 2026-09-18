package agentsetup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestBrainDirectoryHonorsExplicitAndEnvironmentLocations(t *testing.T) {
	explicit := filepath.Join(t.TempDir(), "state")
	if got, err := brainDirectory(explicit, "TEST_UNUSED", "TEST_UNUSED_XDG", "fallback"); err != nil || got != explicit {
		t.Fatalf("explicit directory = %q, %v", got, err)
	}
	t.Setenv("AGENTSETUP_STATE", filepath.Join(t.TempDir(), "configured"))
	if got, err := brainDirectory("", "AGENTSETUP_STATE", "AGENTSETUP_XDG", "fallback"); err != nil || got != os.Getenv("AGENTSETUP_STATE") {
		t.Fatalf("override directory = %q, %v", got, err)
	}
	t.Setenv("AGENTSETUP_STATE", "relative")
	if _, err := brainDirectory("", "AGENTSETUP_STATE", "AGENTSETUP_XDG", "fallback"); err == nil {
		t.Fatal("relative explicit state directory accepted")
	}
	t.Setenv("AGENTSETUP_STATE", "")
	t.Setenv("AGENTSETUP_XDG", filepath.Join(t.TempDir(), "xdg"))
	if got, err := brainDirectory("", "AGENTSETUP_STATE", "AGENTSETUP_XDG", "fallback"); err != nil || got != filepath.Join(os.Getenv("AGENTSETUP_XDG"), "entire") {
		t.Fatalf("XDG directory = %q, %v", got, err)
	}
}

func TestBrainKeysCanonicalizesRemoteAndConfiguredSlugs(t *testing.T) {
	tests := []struct {
		name, remote, want string
	}{
		{name: "github", remote: "https://GitHub.com/Team/Repo.git", want: "gh/team/repo"},
		{name: "scp", remote: "git@gitlab.com:Team/Repo.git", want: "gl/team/repo"},
		{name: "entire", remote: "entire://Org/Team/Repo/Work", want: "team/repo/work"},
		{name: "host shorthand", remote: "code.storage/Team/Repo", want: "cs/team/repo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keys, err := brainKeys(t.TempDir(), tt.remote, t.TempDir())
			if err != nil || len(keys) != 1 || keys[0] != tt.want {
				t.Fatalf("brainKeys(%q) = %#v, %v; want %q", tt.remote, keys, err, tt.want)
			}
		})
	}
	if _, err := brainKeys(t.TempDir(), "entire://too/short", t.TempDir()); err == nil {
		t.Fatal("short Entire identity accepted")
	}
	config := t.TempDir()
	if err := os.WriteFile(filepath.Join(config, "brain.json"), []byte(`{"domain_slugs":{"forge.example":"forge"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	keys, err := brainKeys(t.TempDir(), "forge.example/team/repo.git", config)
	if err != nil || len(keys) != 1 || keys[0] != "forge/team/repo" {
		t.Fatalf("configured host key = %#v, %v", keys, err)
	}
	if err := os.WriteFile(filepath.Join(config, "brain.json"), []byte(`{"domain_slugs":{"forge.example":"bad slug"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := brainKeys(t.TempDir(), "forge.example/team/repo", config); err == nil {
		t.Fatal("unsafe configured slug accepted")
	}
	for _, body := range []string{"null", "{"} {
		if err := os.WriteFile(filepath.Join(config, "brain.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := brainKeys(t.TempDir(), "forge.example/team/repo", config); err == nil {
			t.Fatalf("malformed Brain configuration %q accepted", body)
		}
	}
}

func TestBrainKeysLocalIdentityIncludesCanonicalAndLegacySpellings(t *testing.T) {
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "repo-link")
	if err := os.Symlink(root, alias); err != nil {
		t.Skip("host cannot represent directory symlinks")
	}
	keys, err := brainKeys(alias, "", t.TempDir())
	if err != nil || len(keys) < 2 {
		t.Fatalf("local alias keys = %#v, %v; want canonical and legacy spellings", keys, err)
	}
}

func TestOpenRecordRejectsAliasesAndNonReadableState(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "valid.json"), []byte(`{"ok":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "folder"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Run("alias is rejected", func(t *testing.T) {
		if err := os.Symlink(filepath.Join(dir, "valid.json"), filepath.Join(dir, "alias.json")); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		if _, present, err := openRecord(dir, "alias.json"); err == nil || present {
			t.Fatalf("alias accepted: present=%v err=%v", present, err)
		}
	})
	for _, name := range []string{"folder", "valid.json/child"} {
		t.Run(name, func(t *testing.T) {
			if file, present, err := openRecord(dir, name); present || file != nil || (name == "folder" && err == nil) {
				t.Fatalf("openRecord(%q) returned a record: err=%v", name, err)
			}
		})
	}
	if data, present, err := readRecord(dir, "valid.json"); err != nil || !present || string(data) != `{"ok":true}` {
		t.Fatalf("valid record = %q, %v, %v", data, present, err)
	}
	if data, present, err := readRecord(filepath.Join(dir, "missing"), "valid.json"); err != nil || present || data != nil {
		t.Fatalf("missing state root = %q, %v, %v", data, present, err)
	}
	if data, present, err := readRecord(dir, "missing.json"); err != nil || present || data != nil {
		t.Fatalf("missing record = %q, %v, %v", data, present, err)
	}
}

func TestRepositoryBrainReadsConfiguredSetupRecordWithoutWriting(t *testing.T) {
	repo := t.TempDir()
	canonical, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{StateDir: t.TempDir(), ConfigDir: t.TempDir(), DataDir: t.TempDir()}
	key := localKey(canonical)
	setupDir := filepath.Join(opts.StateDir, "repos", filepath.FromSlash(key))
	if err := os.MkdirAll(setupDir, 0o755); err != nil {
		t.Fatal(err)
	}
	setup := `{"schema_version":1,"workspace":""}`
	if err := os.WriteFile(filepath.Join(setupDir, "setup.json"), []byte(setup), 0o600); err != nil {
		t.Fatal(err)
	}
	found, err := repositoryBrain(repo, opts)
	if err != nil || !found {
		t.Fatalf("repositoryBrain = %v, %v", found, err)
	}
	if got, err := os.ReadFile(filepath.Join(setupDir, "setup.json")); err != nil || string(got) != setup {
		t.Fatalf("setup record changed: %q, %v", got, err)
	}
}

func TestRepositoryBrainRejectsConflictingAndMalformedSetupState(t *testing.T) {
	for _, tc := range []struct {
		name, setup string
	}{
		{name: "bad schema", setup: `{"schema_version":2}`},
		{name: "bad json", setup: `{"schema_version":1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			canonical, err := filepath.EvalSymlinks(repo)
			if err != nil {
				t.Fatal(err)
			}
			opts := Options{StateDir: t.TempDir(), ConfigDir: t.TempDir(), DataDir: t.TempDir()}
			dir := filepath.Join(opts.StateDir, "repos", localKey(canonical))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "setup.json"), []byte(tc.setup), 0o600); err != nil {
				t.Fatal(err)
			}
			if found, err := repositoryBrain(repo, opts); err == nil || found {
				t.Fatalf("malformed setup accepted: found=%v err=%v", found, err)
			}
		})
	}
}

func TestRepositoryBrainAcceptsLegacyManifestAndRejectsIdentityMismatch(t *testing.T) {
	repo := t.TempDir()
	canonical, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{StateDir: t.TempDir(), ConfigDir: t.TempDir(), DataDir: t.TempDir()}
	dir := filepath.Join(opts.DataDir, "repos", localKey(canonical))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(map[string]any{"schema_version": 3, "repo_key": localKey(canonical), "repo_root": canonical})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	if found, err := repositoryBrain(repo, opts); err != nil || !found {
		t.Fatalf("legacy manifest = %v, %v", found, err)
	}
	bad := `{"schema_version":3,"repo_key":"local/other-123456","repo_root":"x"}`
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if found, err := repositoryBrain(repo, opts); err == nil || found {
		t.Fatalf("identity mismatch accepted: %v, %v", found, err)
	}
}
