package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Proving the MCP surface is REACHABLE, not merely declared.
//
// mcpToolDefinitions() is what this binary would serve. It says nothing about
// whether a host can reach it, and every failure in this class has been silent:
//
//   - `claude --safe-mode` strips every MCP server, so the tools are simply
//     absent and nothing says so.
//   - CLAUDE_CODE_PLUGIN_DIRS is a single path, not a list; one comma breaks
//     every plugin named in it.
//   - A plugin pointing at a purged directory reports CONNECTION_CLOSED, which
//     reads as a server crash rather than a missing file.
//
// In each case the declared surface is perfect and the reachable surface is
// empty, so a check that reads the declaration passes while the product is
// broken. This spawns the resolved binary, completes the handshake a host
// completes, and reports what the server actually served -- and when it cannot,
// names the cause instead of the symptom.

// mcpProbeTimeout bounds the handshake. A host gives up quietly; this must not.
const mcpProbeTimeout = 20 * time.Second

// mcpSurfaceProbe is one probe's outcome. Tools is what the server served;
// Cause is a reader-facing explanation of why it served nothing.
type mcpSurfaceProbe struct {
	Tools []string
	Cause string
}

// probeMCPToolSurface spawns `<binary> mcp`, performs initialize then
// tools/list over the same Content-Length framing a host uses, and returns the
// tool names served. The returned error always names what went wrong at the
// step it went wrong, because "CONNECTION_CLOSED" is the message this exists
// to replace.
func probeMCPToolSurface(ctx context.Context, binary string, timeout time.Duration) (mcpSurfaceProbe, error) {
	if strings.TrimSpace(binary) == "" {
		return mcpSurfaceProbe{}, errors.New("no plugin binary resolved: nothing to probe")
	}
	if timeout <= 0 {
		timeout = mcpProbeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, binary, "mcp")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return mcpSurfaceProbe{}, fmt.Errorf("open stdin to %s: %w", binary, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return mcpSurfaceProbe{}, fmt.Errorf("open stdout from %s: %w", binary, err)
	}
	// os/exec spawns its own goroutine to copy into a Stderr that is not an
	// *os.File, and documents that Wait must happen before the destination is
	// read. The error paths below read it BEFORE Wait on purpose -- the
	// server's own message is the most useful thing it leaves behind -- so the
	// sink has to be safe for a concurrent read. It was a plain
	// strings.Builder first, which `go test -race` caught as a real data race
	// between exec's copier and mcpProbeExitError.
	stderr := &lockedBuilder{}
	cmd.Stderr = stderr
	// Bound Wait as well as the context. CommandContext kills the process it
	// started, but not that process's children, and a surviving grandchild
	// keeps the stdout pipe open -- so a drain-and-Wait in the defer blocks on
	// the grandchild long after the probe's own deadline passed. A `sleep 30`
	// behind a one-second timeout hung this for the full thirty, which a test
	// caught. WaitDelay forces the pipes closed and reaps the group, so the
	// probe cannot outlive its deadline by more than this.
	cmd.WaitDelay = 2 * time.Second

	if err := cmd.Start(); err != nil {
		// The single most common real cause, and the one CONNECTION_CLOSED
		// hides: the path does not exist any more.
		return mcpSurfaceProbe{}, fmt.Errorf("could not start the MCP server at %s: %w", binary, err)
	}
	// settle closes the pipes and reaps the process, at most once. The error
	// paths below call it BEFORE reading stderr, which matters: os/exec copies
	// stderr on its own goroutine, so a probe that failed in microseconds (a
	// server that answered and exited, say) would otherwise read an empty
	// buffer and report "broken pipe" instead of the reason the server gave.
	// Naming the cause is the entire point of this check, so it waits for the
	// cause to arrive. WaitDelay bounds that wait.
	var settled bool
	settle := func() {
		if settled {
			return
		}
		settled = true
		// Close stdin first so a well-behaved server exits on EOF, then let
		// WaitDelay deal with one that does not. Nothing drains stdout:
		// draining is what made this unbounded.
		_ = stdin.Close()
		_ = stdout.Close()
		_ = cmd.Wait()
	}
	defer settle()

	request := func(id int, method string) error {
		payload := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":{}}`, id, method)
		_, err := io.WriteString(stdin, fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(payload), payload))
		return err
	}
	if err := request(1, "initialize"); err != nil {
		settle()
		return mcpSurfaceProbe{}, mcpProbeExitError("write initialize", binary, err, stderr)
	}
	if err := request(2, "tools/list"); err != nil {
		settle()
		return mcpSurfaceProbe{}, mcpProbeExitError("write tools/list", binary, err, stderr)
	}

	reader := bufio.NewReader(stdout)
	if _, err := readMCPFrame(reader); err != nil {
		settle()
		return mcpSurfaceProbe{}, mcpProbeExitError("read the initialize response", binary, err, stderr)
	}
	frame, err := readMCPFrame(reader)
	if err != nil {
		settle()
		return mcpSurfaceProbe{}, mcpProbeExitError("read the tools/list response", binary, err, stderr)
	}

	var envelope struct {
		Error  *struct{ Message string } `json:"error"`
		Result struct {
			Tools []struct{ Name string } `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(frame, &envelope); err != nil {
		return mcpSurfaceProbe{}, fmt.Errorf("the MCP server at %s answered tools/list with something that is not JSON-RPC: %w", binary, err)
	}
	if envelope.Error != nil {
		return mcpSurfaceProbe{}, fmt.Errorf("the MCP server at %s refused tools/list: %s", binary, envelope.Error.Message)
	}
	tools := make([]string, 0, len(envelope.Result.Tools))
	for _, tool := range envelope.Result.Tools {
		if tool.Name != "" {
			tools = append(tools, tool.Name)
		}
	}
	sort.Strings(tools)
	return mcpSurfaceProbe{Tools: tools}, nil
}

// lockedBuilder is a strings.Builder that tolerates the concurrent write
// os/exec performs from its copier goroutine while a caller reads what has
// arrived so far.
type lockedBuilder struct {
	mu      sync.Mutex
	builder strings.Builder
}

func (b *lockedBuilder) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.builder.Write(p)
}

func (b *lockedBuilder) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.builder.String()
}

// mcpProbeExitError turns a pipe failure into the reason the server went away,
// because a closed pipe is the symptom and the server's own stderr is usually
// the cause.
func mcpProbeExitError(step, binary string, cause error, stderr *lockedBuilder) error {
	detail := firstLine(strings.TrimSpace(stderr.String()))
	if detail == "" {
		return fmt.Errorf("could not %s from the MCP server at %s: %w", step, binary, cause)
	}
	return fmt.Errorf("could not %s from the MCP server at %s: %w; the server said: %s", step, binary, cause, detail)
}

// readMCPFrame reads one Content-Length framed message, the framing a host
// speaks.
//
// THE ANNOUNCED LENGTH IS NOT TRUSTED, and the reason is the whole premise of
// this check: it runs when something is already wrong with the server, so a
// malformed or hostile frame is the expected case, not the exotic one. The body
// buffer is sized from a number the child process chose, so a child that
// announces `Content-Length: 2000000000` and sends nothing would have doctor
// allocate two gigabytes and get itself OOM-killed -- before mcpProbeTimeout
// has anything to say about it, because a timeout bounds waiting, not
// allocating. Both bounds below are therefore checked BEFORE the make.
//
// The ceiling is maxMCPFrameBytes, the same one readMCPMessage enforces on the
// server's own inbound frames: one number for one wire format, and no new
// constant to drift. It is generous for this reader by construction, because
// the server's own outbound budget cannot exceed it -- 32 KiB by default, and
// resolveMCPToolResponseMaxBytes clamps any override to this same limit -- so
// a legitimate tools/list response cannot be the thing this refuses.
//
// A zero length is legal framing with an empty body and stays accepted, which
// matches readMCPMessage: the empty payload then fails JSON decoding, where it
// is named accurately, rather than being refused here as a framing error it is
// not.
func readMCPFrame(reader *bufio.Reader) ([]byte, error) {
	const noContentLength = -1
	length := noContentLength
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" {
			break
		}
		name, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			parsed, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return nil, fmt.Errorf("unparsable Content-Length %q", strings.TrimSpace(value))
			}
			// A negative length is the other half of the same defect, and it
			// was reported as the wrong thing entirely: -1 landed on the
			// sentinel below and came back as "no Content-Length header",
			// blaming a header the server did send. Refusing it here is also
			// what keeps that sentinel's message true, and it is what keeps a
			// negative out of make([]byte, n), which panics.
			if parsed < 0 {
				return nil, fmt.Errorf("Content-Length %d must be non-negative", parsed)
			}
			length = parsed
		}
	}
	// `< 0`, not `== noContentLength`: the only negative that can reach here is
	// the sentinel, because the parse above refuses a negative the server sent
	// -- but a total comparison keeps make([]byte, n) unreachable for ANY
	// negative, so a future edit that loses the parse check is misnamed rather
	// than a panic.
	if length < 0 {
		return nil, errors.New("response had no Content-Length header")
	}
	if length > maxMCPFrameBytes {
		return nil, &mcpOversizeFrameError{length: length}
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, err
	}
	return body, nil
}

// missingMCPTools reports which of the declared tools the server did not serve,
// which is the difference between "declared" and "reachable".
func missingMCPTools(served []string) []string {
	have := make(map[string]bool, len(served))
	for _, name := range served {
		have[name] = true
	}
	var missing []string
	for _, definition := range mcpToolDefinitions() {
		name, _ := definition["name"].(string)
		if name != "" && !have[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

// checkMCPSurfaceReachable is the doctor check COR-2159 asks for: it proves the
// GA tool surface is REACHABLE through a real process, and when it is not, it
// names the cause.
//
// Opt-in via ENTIRE_BRAIN_DOCTOR_PROBE_MCP, because it spawns a subprocess and
// doctor's other checks read state. The documented one-command form is
//
//	ENTIRE_BRAIN_DOCTOR_PROBE_MCP=1 entire brain doctor
//
// and CI runs the same assertion directly as
// TestMCPProbeReachesTHISBinarysRealSurface, which needs no environment at all.
// exePath is injected rather than calling os.Executable() directly, and that
// is not a style preference. Under `go test`, os.Executable() IS the test
// binary, and spawning it re-runs the whole package -- each of those runs
// spawning again. A test of this check therefore forked a process tree until
// the machine ran out, which on CI cascaded into dozens of failures in
// unrelated tests and locally only showed up as the probe taking its full
// 20-second timeout. Injecting the resolver is what makes the check testable
// without spawning anything real.
// doctorMCPSurfaceCheck is the seam doctor calls through, so a test can prove
// the check is WIRED without spawning anything. Injecting getenv and exePath
// made the check itself testable; it did not make its presence in doctor
// testable, and unwiring the call left every guard here green. A var, matching
// how the read bounds in internal/base are overridable for the same reason.
var doctorMCPSurfaceCheck = checkMCPSurfaceReachable

func checkMCPSurfaceReachable(ctx context.Context, getenv func(string) string, exePath func() (string, error), add func(name, state, detail string)) {
	if getenv == nil || strings.TrimSpace(getenv("ENTIRE_BRAIN_DOCTOR_PROBE_MCP")) == "" {
		return
	}
	if exePath == nil {
		exePath = os.Executable
	}
	resolved, err := exePath()
	if err != nil || strings.TrimSpace(resolved) == "" {
		add("mcp surface", "warn", "could not resolve this binary's own path, so the MCP surface cannot be probed")
		return
	}
	probe, probeErr := probeMCPToolSurface(ctx, resolved, mcpProbeTimeout)
	if probeErr != nil {
		// The cause, not the symptom. This is the line that replaces
		// CONNECTION_CLOSED.
		add("mcp surface", "error", probeErr.Error())
		return
	}
	if missing := missingMCPTools(probe.Tools); len(missing) > 0 {
		add("mcp surface", "error", fmt.Sprintf(
			"%d of %d declared tools are NOT reachable through %s: %s",
			len(missing), len(mcpToolDefinitions()), resolved, strings.Join(missing, ", ")))
		return
	}
	add("mcp surface", "ok", fmt.Sprintf("all %d declared tools reachable through %s", len(probe.Tools), resolved))
}
