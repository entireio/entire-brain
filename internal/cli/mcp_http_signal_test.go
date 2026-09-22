//go:build !windows

package cli

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMCPHTTPShutsDownOnSignal(t *testing.T) {
	if os.Getenv("ENTIRE_TEST_HTTP_SIGNAL_HELPER") == "1" {
		if err := runMCPHTTP(context.Background(), os.Stdout, Options{}, "127.0.0.1:0", false); err != nil {
			t.Fatal(err)
		}
		return
	}
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMCPHTTPShutsDownOnSignal$")
			cmd.Env = append(os.Environ(), "ENTIRE_TEST_HTTP_SIGNAL_HELPER=1", "ENTIRE_BRAIN_MCP_TOKEN=test-token")
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer cmd.Process.Kill()
			scanner := bufio.NewScanner(stdout)
			if !scanner.Scan() || !strings.HasPrefix(scanner.Text(), "brain MCP listening on http://") {
				t.Fatalf("server did not become ready: %s", scanner.Text())
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			// Keep draining output so the child can finish its startup banner.
			for scanner.Scan() {
			}
			if err := cmd.Wait(); err != nil {
				t.Fatalf("server did not shut down cleanly after %s: %v\n%s", sig, err, stderr.String())
			}
		})
	}
}
