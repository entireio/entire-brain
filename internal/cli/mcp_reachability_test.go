package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The probe spawns a real process, so these drive real processes. A fake
// in-memory server would test the parser and leave the thing COR-2159 is about
// -- can a host actually reach the surface -- unproven.

// mcpFakeServer writes a shell script that behaves like an MCP server in one
// specific way, and returns its path.
func mcpFakeServer(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake server is a POSIX shell script")
	}
	path := filepath.Join(t.TempDir(), "fake-brain")
	// The fake drains stdin in the background and stays alive until the probe
	// closes it. A fake that printf'd and exited immediately raced the probe's
	// own write: on a fast machine the script was gone before `initialize` was
	// written, the write hit EPIPE, and the test failed with "broken pipe"
	// instead of exercising anything. It passed locally on timing luck and
	// failed on CI, which is the whole reason this comment exists.
	script := "#!/bin/sh\ncat >/dev/null &\n" + body + "\nwait\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// frame emits one Content-Length framed payload from shell.
func frame(payload string) string {
	return "printf 'Content-Length: " + itoa(len(payload)) + "\\r\\n\\r\\n" + payload + "'\n"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// TestMCPProbeReportsTheToolsTheServerActuallyServed is the happy path: the
// surface is reachable and the probe says what it reached.
func TestMCPProbeReportsTheToolsTheServerActuallyServed(t *testing.T) {
	t.Parallel()
	server := mcpFakeServer(t,
		frame(`{"jsonrpc":"2.0","id":1,"result":{}}`)+
			frame(`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"brain_brief"},{"name":"brain_recall"}]}}`))

	probe, err := probeMCPToolSurface(context.Background(), server, 10*time.Second)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if got := strings.Join(probe.Tools, ","); got != "brain_brief,brain_recall" {
		t.Fatalf("served tools = %q", got)
	}
}

// TestMCPProbeNamesAMissingBinary is the CONNECTION_CLOSED case, which is the
// reason this check exists: a plugin pointing at a purged directory must say
// the path is gone, not that the connection closed.
func TestMCPProbeNamesAMissingBinary(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "purged", "entire-brain")
	_, err := probeMCPToolSurface(context.Background(), missing, 5*time.Second)
	if err == nil {
		t.Fatal("a missing binary must not probe clean")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Fatalf("the error must name the path that is gone, got: %v", err)
	}
	if !strings.Contains(err.Error(), "could not start") {
		t.Fatalf("the error must say it could not start the server, got: %v", err)
	}
}

// TestMCPProbeSurfacesTheServersOwnStderr: a server that dies on startup knows
// why. Reporting a closed pipe instead of its message is how a configuration
// error gets read as a crash.
func TestMCPProbeSurfacesTheServersOwnStderr(t *testing.T) {
	t.Parallel()
	server := mcpFakeServer(t, "echo 'brain store is locked by another process' >&2\nexit 9\n")
	_, err := probeMCPToolSurface(context.Background(), server, 10*time.Second)
	if err == nil {
		t.Fatal("a server that exits 9 must not probe clean")
	}
	if !strings.Contains(err.Error(), "brain store is locked by another process") {
		t.Fatalf("the server's own reason must reach the reader, got: %v", err)
	}
}

// TestMCPProbeTimesOutInsteadOfHanging: a host gives up quietly, so the probe
// must give up loudly and bounded.
func TestMCPProbeTimesOutInsteadOfHanging(t *testing.T) {
	t.Parallel()
	server := mcpFakeServer(t, "sleep 30\n")
	started := time.Now()
	_, err := probeMCPToolSurface(context.Background(), server, 1200*time.Millisecond)
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("a silent server must not probe clean")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("probe hung for %s instead of honouring its timeout", elapsed)
	}
}

// TestMCPProbeRejectsANonMCPBinary: the resolved path existing is not the same
// as it being the server. A binary that prints something else must be refused
// with a reason, not parsed hopefully.
func TestMCPProbeRejectsANonMCPBinary(t *testing.T) {
	t.Parallel()
	server := mcpFakeServer(t, "echo 'usage: some other tool'\n")
	_, err := probeMCPToolSurface(context.Background(), server, 10*time.Second)
	if err == nil {
		t.Fatal("a binary that does not speak MCP must not probe clean")
	}
}

// TestMCPProbeReportsAToolsListRefusal: a server that answers with a JSON-RPC
// error has not served a surface, and must not be read as serving zero tools.
func TestMCPProbeReportsAToolsListRefusal(t *testing.T) {
	t.Parallel()
	server := mcpFakeServer(t,
		frame(`{"jsonrpc":"2.0","id":1,"result":{}}`)+
			frame(`{"jsonrpc":"2.0","id":2,"error":{"code":-32603,"message":"brain not initialised for this repository"}}`))
	_, err := probeMCPToolSurface(context.Background(), server, 10*time.Second)
	if err == nil {
		t.Fatal("a refused tools/list must not probe clean")
	}
	if !strings.Contains(err.Error(), "brain not initialised for this repository") {
		t.Fatalf("the refusal's reason must reach the reader, got: %v", err)
	}
}

// TestMissingMCPToolsNamesTheGap turns "declared" into "reachable": the check
// is only useful if it says WHICH tools a host cannot see.
func TestMissingMCPToolsNamesTheGap(t *testing.T) {
	t.Parallel()
	declared := mcpToolDefinitions()
	if len(declared) == 0 {
		t.Fatal("no declared tools; the comparison would pass vacuously")
	}
	if missing := missingMCPTools(nil); len(missing) != len(declared) {
		t.Fatalf("a server that served nothing must report every declared tool missing: got %d of %d",
			len(missing), len(declared))
	}
	all := make([]string, 0, len(declared))
	for _, definition := range declared {
		if name, _ := definition["name"].(string); name != "" {
			all = append(all, name)
		}
	}
	if missing := missingMCPTools(all); len(missing) != 0 {
		t.Fatalf("a server that served everything must report nothing missing, got %v", missing)
	}
	// Drop exactly one and prove it is named, so the comparison cannot be a
	// length check that happens to agree.
	if missing := missingMCPTools(all[1:]); len(missing) != 1 || missing[0] != all[0] {
		t.Fatalf("dropping %q must name exactly that tool, got %v", all[0], missing)
	}
}

// TestMCPProbeReachesTHISBinarysRealSurface is the end-to-end case: build this
// package's own server and prove every declared tool is reachable through a
// real process over real pipes. This is the assertion COR-2159 asks for -- the
// GA surface reachable, not declared.
func TestMCPProbeReachesTHISBinarysRealSurface(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain to build the server with")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "entire-brain-probe")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.Command("go", "build", "-o", binary, "github.com/entireio/entire-brain/cmd/entire-brain")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("could not build the server here: %v: %s", err, out)
	}
	probe, err := probeMCPToolSurface(context.Background(), binary, 90*time.Second)
	if err != nil {
		t.Fatalf("this binary's own MCP surface is not reachable: %v", err)
	}
	if len(probe.Tools) == 0 {
		t.Fatal("the server served no tools at all")
	}
	if missing := missingMCPTools(probe.Tools); len(missing) > 0 {
		t.Fatalf("declared but NOT reachable: %v", missing)
	}
	t.Logf("%d declared tools all reachable through a real process", len(probe.Tools))
}

// TestMCPSurfaceCheckIsOptIn: the probe spawns a subprocess, so it must not run
// in an ordinary doctor pass. A check that silently costs a process launch on
// every invocation is one someone will disable.
func TestMCPSurfaceCheckIsOptIn(t *testing.T) {
	t.Parallel()
	var names []string
	add := func(name, state, detail string) { names = append(names, name) }

	// A fake server, NOT os.Executable. Under `go test` the executable is the
	// test binary, and spawning it re-runs this whole package -- recursively.
	// The first version of this test did exactly that and forked until the
	// machine gave up; on CI it took dozens of unrelated tests down with it.
	server := mcpFakeServer(t,
		frame(`{"jsonrpc":"2.0","id":1,"result":{}}`)+
			frame(`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"brain_brief"}]}}`))
	exe := func() (string, error) { return server, nil }

	checkMCPSurfaceReachable(context.Background(), func(string) string { return "" }, exe, add)
	if len(names) != 0 {
		t.Fatalf("the check ran without the opt-in: %v", names)
	}
	checkMCPSurfaceReachable(context.Background(), func(key string) string {
		if key == "ENTIRE_BRAIN_DOCTOR_PROBE_MCP" {
			return "1"
		}
		return ""
	}, exe, add)
	if len(names) == 0 {
		t.Fatal("the check did not run with the opt-in set")
	}
}

// TestMCPSurfaceCheckNeverSpawnsTheTestBinary is the guard for the fork bomb
// itself. os.Executable() under `go test` is the test binary, so a check that
// resolves it and spawns it re-runs the package recursively. This asserts the
// resolver is injected and honoured, so no future edit can quietly reach for
// os.Executable() inside the test path again.
func TestMCPSurfaceCheckNeverSpawnsTheTestBinary(t *testing.T) {
	t.Parallel()
	self, err := os.Executable()
	if err != nil {
		t.Skip("cannot resolve the test binary here")
	}
	var spawned string
	exe := func() (string, error) {
		spawned = self
		return filepath.Join(t.TempDir(), "definitely-not-a-server"), nil
	}
	var details []string
	checkMCPSurfaceReachable(context.Background(), func(string) string { return "1" }, exe,
		func(name, state, detail string) { details = append(details, state+": "+detail) })

	if spawned == "" {
		t.Fatal("the injected resolver was never called, so the check reached os.Executable() directly")
	}
	if len(details) != 1 || !strings.HasPrefix(details[0], "error: ") {
		t.Fatalf("a missing binary must report one error, got %v", details)
	}
	if strings.Contains(details[0], self) {
		t.Fatalf("the check probed the TEST BINARY, which re-runs this package: %v", details[0])
	}
}

// TestMCPProbeWaitsForAServersLateReason makes the stderr wait provable
// locally instead of only on CI.
//
// os/exec copies stderr on its own goroutine, so whether a reason has arrived
// by the time an error path reads it is pure timing. The flaw this guards
// shipped once and passed every local run: on macOS the copier happened to win
// the race, and only CI's timing exposed it. This fake removes the race from
// the other side -- it closes stdout FIRST, so the probe is already in its
// error path, and only then writes the reason. Without the wait, the probe
// reads an empty buffer and reports the symptom (EOF / broken pipe) instead of
// the cause, which is the one thing this whole check exists to do.
func TestMCPProbeWaitsForAServersLateReason(t *testing.T) {
	t.Parallel()
	server := mcpFakeServer(t, "exec 1>&-\nsleep 0.4\necho 'the brain store is locked by another process' >&2\nexit 9\n")
	_, err := probeMCPToolSurface(context.Background(), server, 10*time.Second)
	if err == nil {
		t.Fatal("a server that closed stdout and exited 9 must not probe clean")
	}
	if !strings.Contains(err.Error(), "the brain store is locked by another process") {
		t.Fatalf("the server's reason arrived after its stdout closed and was lost; got: %v", err)
	}
}
