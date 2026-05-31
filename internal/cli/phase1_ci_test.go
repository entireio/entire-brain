package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestPhase1SemanticCommandJSONContracts(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	runner.responses[fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD")] = fakeCommandResponse{stdout: "M\tinternal/auth/token.go\n"}
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})

	if _, err := execute(t, cmd, "index", "--sem-binary", "entire"); err != nil {
		t.Fatalf("index: %v", err)
	}
	assertPhase1NoNetworkCommands(t, runner, repoDir)

	assertCommandJSONContains(t, cmd, "stale", []string{"--json"}, `"severity": "ok"`)
	assertCommandJSONContains(t, cmd, "query", []string{"ValidateToken", "--json", "--limit", "2", "--offset", "0"}, `"results"`)
	assertCommandJSONContains(t, cmd, "context", []string{"ValidateToken", "--json", "--limit", "2"}, `"relations"`)
	assertCommandJSONContains(t, cmd, "impact", []string{"ValidateToken", "--json", "--depth", "1", "--limit", "2"}, `"roots"`)
	assertCommandJSONContains(t, cmd, "changes", []string{"--json", "--limit", "2"}, `"internal/auth/token.go"`)

	bundlePath := filepath.Join(t.TempDir(), "brain.tar")
	exportOut, err := execute(t, cmd, "bundle", "export", "--output", bundlePath)
	if err != nil {
		t.Fatalf("bundle export: %v", err)
	}
	checksum := parseBundleChecksum(t, exportOut)
	if _, err := execute(t, cmd, "bundle", "import", bundlePath, "--sha256", checksum); err != nil {
		t.Fatalf("bundle import: %v", err)
	}
	if _, err := execute(t, cmd, "gc", "--older-than", "30d"); err != nil {
		t.Fatalf("gc: %v", err)
	}
	assertPhase1NoNetworkCommands(t, runner, repoDir)
}

func TestPhase1SemanticPerformanceSmoke(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticBoundaryFixtureSnapshot())
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})

	start := time.Now()
	if _, err := execute(t, cmd, "index", "--sem-binary", "entire"); err != nil {
		t.Fatalf("index: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("phase 1 semantic index exceeded smoke budget: %s", elapsed)
	}

	start = time.Now()
	if _, err := execute(t, cmd, "query", "ValidateToken", "--json", "--limit", "5"); err != nil {
		t.Fatalf("query: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("phase 1 semantic query exceeded smoke budget: %s", elapsed)
	}
}

func assertCommandJSONContains(t *testing.T, cmd *cobra.Command, name string, args []string, want string) {
	t.Helper()
	out, err := execute(t, cmd, append([]string{name}, args...)...)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var decoded any
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("%s did not return JSON: %v\n%s", name, err, out)
	}
	if !strings.Contains(out, want) {
		t.Fatalf("%s output missing %q:\n%s", name, want, out)
	}
}

func assertPhase1NoNetworkCommands(t *testing.T, runner *fakeCommandRunner, repoDir string) {
	t.Helper()
	if !fakeRunnerCalled(runner, "entire", "sem", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network") {
		t.Fatalf("semantic provider snapshot was not constrained with --no-network: %+v", runner.calls)
	}
	for _, call := range runner.calls {
		switch call.name {
		case "curl", "wget", "ssh", "scp", "gh":
			t.Fatalf("phase 1 test invoked network-capable command: %+v", call)
		case "git":
			if len(call.args) > 0 && call.args[0] == "fetch" {
				t.Fatalf("phase 1 test invoked git fetch: %+v", call)
			}
		}
	}
}

func parseBundleChecksum(t *testing.T, output string) string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if checksum, ok := strings.CutPrefix(line, "sha256: "); ok {
			return strings.TrimSpace(checksum)
		}
	}
	t.Fatalf("bundle export did not print checksum:\n%s", output)
	return ""
}
