package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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
	// The fake drains stdin and stays alive until the probe closes it. A fake
	// that printf'd and exited immediately raced the probe's own write: the
	// script was gone before `initialize` was written, the write hit EPIPE,
	// and the test failed with "broken pipe" instead of exercising anything.
	//
	// The `exec 3<&0` is load-bearing and not a style choice. POSIX gives a
	// BACKGROUND job's stdin /dev/null before any explicit redirection, so a
	// plain `cat >/dev/null &` reads nothing, exits at once, and `wait`
	// returns immediately -- the script dies anyway. Reproduced in dash, which
	// is what /bin/sh is on the CI runners: the plain form returns from `wait`
	// immediately, the fd-dup form returns only once stdin closes. macOS's
	// /bin/sh happened to behave the other way, which is why the plain form
	// passed every local run and failed only on CI.
	script := "#!/bin/sh\nexec 3<&0\ncat <&3 >/dev/null &\n" + body + "\nwait\n"
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

// TestFakeServerDrainKeepsStdinOpen asserts the SHELL PROPERTY the fakes
// depend on, not the probe, because the probe end of it is a race that depends
// on machine load and cannot be made to fail on demand.
//
// POSIX gives a background job's stdin /dev/null before any explicit
// redirection. So `cat >/dev/null &` drains nothing, exits at once, and the
// following `wait` returns immediately -- the fake dies regardless of the
// drain, and the probe's write then hits EPIPE. `exec 3<&0` plus `cat <&3`
// gives the background reader an explicit redirection from the real stdin,
// which overrides that default.
//
// This is asserted against dash as well as /bin/sh, because the two disagree:
// the plain form passed every local run on macOS and failed only on the CI
// runners, where /bin/sh is dash. Measuring the difference directly is what
// turns that into a local guard.
func TestFakeServerDrainKeepsStdinOpen(t *testing.T) {
	t.Parallel()
	// The fakes this guards are POSIX shell scripts, and mcpFakeServer already
	// skips on Windows for that reason. This test executes /bin/sh directly,
	// so it needs the same guard -- without it, Windows CI runs a branch that
	// cannot work anywhere on that platform.
	if runtime.GOOS == "windows" {
		t.Skip("the fakes are POSIX shell scripts; mcpFakeServer skips here too")
	}
	shells := []string{"/bin/sh"}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh here")
	}
	if dash, err := exec.LookPath("dash"); err == nil {
		shells = append(shells, dash)
	}
	// waited reports whether `wait` blocked until stdin closed, which is the
	// property a fake needs to outlive the prober's writes.
	waited := func(t *testing.T, shell, drain string) bool {
		t.Helper()
		script := filepath.Join(t.TempDir(), "drain.sh")
		if err := os.WriteFile(script, []byte("#!"+shell+"\n"+drain+"\nwait\necho done\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(script)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		// Hold stdin open. A shell that drains correctly cannot finish yet.
		select {
		case <-done:
			_ = stdin.Close()
			return false
		case <-time.After(400 * time.Millisecond):
		}
		_ = stdin.Close()
		select {
		case <-done:
			return true
		case <-time.After(5 * time.Second):
			t.Fatal("the script never exited after stdin closed")
			return false
		}
	}
	for _, shell := range shells {
		if waited(t, shell, "cat >/dev/null &") {
			t.Logf("%s: the plain background drain happens to work here", shell)
		}
		if !waited(t, shell, "exec 3<&0\ncat <&3 >/dev/null &") {
			t.Fatalf("%s: the fd-dup drain did NOT keep stdin open, so a fake cannot outlive the prober's writes", shell)
		}
	}
}

// Unwiring the check from doctor left every other guard in this file green:
// they all call checkMCPSurfaceReachable directly, so none of them observes
// whether doctor runs it. This one does, through the seam, so it never spawns
// a process.
func TestDoctorActuallyRunsTheMCPSurfaceCheck(t *testing.T) {
	ran := false
	original := doctorMCPSurfaceCheck
	doctorMCPSurfaceCheck = func(ctx context.Context, getenv func(string) string, exePath func() (string, error), add func(name, state, detail string)) {
		ran = true
		if getenv == nil || exePath == nil || add == nil {
			t.Error("doctor passed a nil argument to the MCP surface check")
		}
	}
	t.Cleanup(func() { doctorMCPSurfaceCheck = original })

	// A real repo and brain, so doctor runs its whole report rather than
	// bailing out before or after the call under test.
	opts, repoDir, _ := factsAvailabilityFixture(t)
	brainDoctorReadOnlyReport(context.Background(), opts, repoDir)

	if !ran {
		t.Fatal("doctor did not run the MCP surface check at all")
	}
}

// --- the announced length is not the server's to be trusted with ---------
//
// readMCPFrame sizes its body buffer from a number the child process chose.
// The child is this project's own binary, which is exactly the reasoning that
// left it unbounded -- and exactly the reasoning that does not hold here: this
// check runs when something about that binary is ALREADY wrong, so a malformed
// frame is its expected input, not an exotic one. A declared length of two
// gigabytes has to be refused by the number, before the allocation, because a
// process killed by the allocator reports nothing at all and mcpProbeTimeout
// bounds waiting rather than allocating.
//
// These drive the reader directly instead of through a fake server: that the
// refusal happens before a single body byte is read is not observable from
// outside the process, and a fake cannot be made to announce a length its
// shell could not also be made to send.

// headerOnlyFrame yields a frame header with NO body, so any read of a body is
// a read that cannot succeed.
func headerOnlyFrame(contentLength string) *bufio.Reader {
	return bufio.NewReader(strings.NewReader("Content-Length: " + contentLength + "\r\n\r\n"))
}

// TestReadMCPFrameRefusesAnOversizedContentLength is the bound itself.
func TestReadMCPFrameRefusesAnOversizedContentLength(t *testing.T) {
	t.Parallel()
	// math.MaxInt is here to make the ORDER load-bearing, not for coverage of
	// another magnitude. The two small cases are refused identically whether
	// the cap is checked before or after the allocation -- 16 MiB allocates
	// harmlessly -- so with only those, moving `make` above the cap check
	// leaves this test and the probe's green while restoring the multi-GB
	// allocation the finding was about. make([]byte, math.MaxInt) cannot
	// succeed on any platform, so reaching it panics and this goes red.
	for _, length := range []int{maxMCPFrameBytes + 1, maxMCPFrameBytes * 4, math.MaxInt} {
		_, err := readMCPFrame(headerOnlyFrame(strconv.Itoa(length)))
		if err == nil {
			t.Fatalf("Content-Length %d was accepted", length)
		}
		var oversize *mcpOversizeFrameError
		if !errors.As(err, &oversize) {
			t.Fatalf("Content-Length %d: want an oversize frame error, got %T: %v", length, err, err)
		}
		// BOTH numbers, because a reader who is told only "too large" has to
		// go to the source to find out what the limit was and what the server
		// claimed.
		if !strings.Contains(err.Error(), strconv.Itoa(length)) {
			t.Fatalf("the error must name the announced length %d, got: %v", length, err)
		}
		if !strings.Contains(err.Error(), strconv.Itoa(maxMCPFrameBytes)) {
			t.Fatalf("the error must name the cap %d, got: %v", maxMCPFrameBytes, err)
		}
	}
}

// TestReadMCPFrameAcceptsALengthAtTheCap pins the comparison at `>` and not
// `>=`: a frame exactly at the limit is within it, so the only thing that may
// end this read is the body that never came.
func TestReadMCPFrameAcceptsALengthAtTheCap(t *testing.T) {
	t.Parallel()
	_, err := readMCPFrame(headerOnlyFrame(strconv.Itoa(maxMCPFrameBytes)))
	if err == nil {
		t.Fatal("a header with no body must not read clean")
	}
	var oversize *mcpOversizeFrameError
	if errors.As(err, &oversize) {
		t.Fatalf("a length exactly at the cap is within the cap, got: %v", err)
	}
}

// TestReadMCPFrameRefusesANegativeContentLength is the other half of the same
// defect. make([]byte, n) panics on a negative n, and the sentinel that kept
// it from getting there reported the wrong thing entirely: -1 came back as
// "response had no Content-Length header", blaming the server for omitting a
// header it had sent.
func TestReadMCPFrameRefusesANegativeContentLength(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"-1", "-2000000000"} {
		_, err := readMCPFrame(headerOnlyFrame(value))
		if err == nil {
			t.Fatalf("Content-Length %s was accepted", value)
		}
		if !strings.Contains(err.Error(), "must be non-negative") {
			t.Fatalf("Content-Length %s must be refused as negative, got: %v", value, err)
		}
		if strings.Contains(err.Error(), "no Content-Length header") {
			t.Fatalf("Content-Length %s was misreported as a MISSING header: %v", value, err)
		}
		if !strings.Contains(err.Error(), value) {
			t.Fatalf("the error must name the announced length, got: %v", err)
		}
	}
}

// TestReadMCPFrameStillReportsAMissingContentLength: the sentinel's own
// message has to survive the negative check that now precedes it, because a
// header that really is absent is a different diagnosis.
func TestReadMCPFrameStillReportsAMissingContentLength(t *testing.T) {
	t.Parallel()
	_, err := readMCPFrame(bufio.NewReader(strings.NewReader("Content-Type: application/json\r\n\r\n{}")))
	if err == nil {
		t.Fatal("a frame with no Content-Length must not read clean")
	}
	if !strings.Contains(err.Error(), "no Content-Length header") {
		t.Fatalf("a genuinely absent header must still be named as absent, got: %v", err)
	}
}

// TestReadMCPFrameKeepsAZeroLengthBodyLegal: zero is an empty body, which is
// well-formed framing, and readMCPMessage treats it the same way. Refusing it
// here would rename a JSON problem as a framing problem.
func TestReadMCPFrameKeepsAZeroLengthBodyLegal(t *testing.T) {
	t.Parallel()
	body, err := readMCPFrame(headerOnlyFrame("0"))
	if err != nil {
		t.Fatalf("a zero-length frame is legal framing: %v", err)
	}
	if len(body) != 0 {
		t.Fatalf("body = %q, want empty", body)
	}
}

// TestReadMCPFrameStillReadsAnOrdinaryFrame keeps the bound from being a
// blanket refusal: the frames this probe exists to read must still arrive
// byte for byte.
func TestReadMCPFrameStillReadsAnOrdinaryFrame(t *testing.T) {
	t.Parallel()
	payload := `{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"brain_brief"}]}}`
	body, err := readMCPFrame(bufio.NewReader(strings.NewReader(
		fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(payload), payload))))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(body) != payload {
		t.Fatalf("body = %q, want %q", body, payload)
	}
}

// TestMCPProbeRefusesAnOversizedFrameInsteadOfAllocatingIt is the same bound
// through the real process path, because a refusal the probe swallows is a
// refusal nobody reads.
func TestMCPProbeRefusesAnOversizedFrameInsteadOfAllocatingIt(t *testing.T) {
	t.Parallel()
	server := mcpFakeServer(t, fmt.Sprintf("printf 'Content-Length: %d\\r\\n\\r\\n'\n", maxMCPFrameBytes+1))
	_, err := probeMCPToolSurface(context.Background(), server, 10*time.Second)
	if err == nil {
		t.Fatal("a frame larger than the frame limit must not probe clean")
	}
	if !strings.Contains(err.Error(), "exceeds maximum frame size") {
		t.Fatalf("the probe must report the refused frame, got: %v", err)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(maxMCPFrameBytes+1)) {
		t.Fatalf("the probe must name the length the server announced, got: %v", err)
	}
}
