package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// The usual registration, `entire brain mcp`, resolves `brain` as a plugin at
// spawn time. When that lookup fails the host reports only CONNECTION_CLOSED,
// naming neither the command nor the cause. This entry names the binary, so
// there is no lookup to fail.
func TestPrintMCPServerConfigNamesThisBinaryDirectly(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	if err := printMCPServerConfig(context.Background(), &out, io.Discard, Options{Runner: ExecRunner{}}); err != nil {
		t.Fatalf("print config: %v", err)
	}

	var config struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(out.Bytes(), &config); err != nil {
		t.Fatalf("output must be valid JSON a host can paste: %v\n%s", err, out.String())
	}

	server, ok := config.MCPServers[mcpServerName]
	if !ok {
		t.Fatalf("no %q entry: %s", mcpServerName, out.String())
	}
	if server.Type != "stdio" {
		t.Errorf("type = %q, want stdio", server.Type)
	}
	if !filepath.IsAbs(server.Command) {
		t.Errorf("command must be absolute so it does not depend on PATH order, got %q", server.Command)
	}
	if _, err := os.Stat(server.Command); err != nil {
		t.Errorf("command must exist on disk: %v", err)
	}
	if len(server.Args) != 1 || server.Args[0] != "mcp" {
		t.Errorf("args = %v, want [mcp]; anything longer reintroduces a subcommand lookup", server.Args)
	}
}
