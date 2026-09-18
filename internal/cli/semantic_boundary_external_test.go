package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// semanticBoundaryFixtureSnapshotExternalNodes models the shape the real
// `entire graph` provider emits: route/tool boundaries are external endpoint
// nodes (no file path) referenced by HANDLES_* relations from in-repo handler
// symbols, rather than kind-tagged in-repo symbols. The external records are
// dropped at SQLite generation (only file/symbol/relation are ingested), so the
// boundary identity must be recovered from the relations' external endpoints.
func semanticBoundaryFixtureSnapshotExternalNodes() string {
	return `{"schema_version":"1.0","provider":"entire-graph","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111","capabilities":["go","routes","tools","workflows"],"warnings":[],"partial_failures":[]}
{"record_type":"symbol","id":"gh/example/repo:go:internal/auth/token.go:function:auth.ValidateToken","kind":"function","name":"ValidateToken","qualified_name":"auth.ValidateToken","file_path":"internal/auth/token.go","start_line":10,"end_line":20,"signature":"func ValidateToken(token string) error","language":"Go","stable_id_version":"1"}
{"record_type":"symbol","id":"gh/example/repo:go:internal/cli/root.go:function:cli.Refresh","kind":"function","name":"Refresh","qualified_name":"cli.Refresh","file_path":"internal/cli/root.go","start_line":50,"end_line":60,"signature":"func Refresh()","language":"Go","stable_id_version":"1"}
{"record_type":"external","id":"external:route:/tokens/{id}","kind":"route","name":""}
{"record_type":"external","id":"external:tool:brain refresh","kind":"tool","name":""}
{"record_type":"external","id":"external:import:../lib/util","kind":"import","name":""}
{"record_type":"relation","from_id":"gh/example/repo:go:internal/auth/token.go:function:auth.ValidateToken","to_id":"external:route:/tokens/{id}","type":"HANDLES_ROUTE","confidence":0.9}
{"record_type":"relation","from_id":"gh/example/repo:go:internal/cli/root.go:function:cli.Refresh","to_id":"external:tool:brain refresh","type":"HANDLES_TOOL","confidence":0.8}
{"record_type":"summary"}
`
}

func TestSemanticBoundaryCommandsResolveExternalNodes(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticBoundaryFixtureSnapshotExternalNodes())
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
	if _, err := execute(t, cmd, "refresh", "index", "--graph-binary", "entire"); err != nil {
		t.Fatalf("index: %v", err)
	}

	type boundaryReport struct {
		Boundary semanticBoundaryResult `json:"boundary"`
	}
	run := func(kind string) boundaryReport {
		t.Helper()
		out, err := execute(t, cmd, "inspect", "boundaries", "--kind", kind, "--json")
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		var report boundaryReport
		if err := json.Unmarshal([]byte(out), &report); err != nil {
			t.Fatalf("parse %s json: %v\n%s", kind, err, out)
		}
		return report
	}

	// routes: the external:route node becomes the boundary (kind route, no file
	// path, name from the endpoint value); the in-repo handler is listed.
	routes := run("route")
	if len(routes.Boundary.Boundaries) != 1 {
		t.Fatalf("routes boundaries = %+v", routes.Boundary.Boundaries)
	}
	if b := routes.Boundary.Boundaries[0]; b.Kind != "route" || b.Name != "/tokens/{id}" || b.FilePath != "" {
		t.Fatalf("unexpected route boundary: %+v", b)
	}
	if len(routes.Boundary.Handlers) != 1 || routes.Boundary.Handlers[0].Name != "ValidateToken" {
		t.Fatalf("routes handlers = %+v", routes.Boundary.Handlers)
	}
	if len(routes.Boundary.Relations) != 1 {
		t.Fatalf("routes relations = %+v", routes.Boundary.Relations)
	}

	// tools: separate external:tool node; the route boundary/handler must not
	// leak into the tool view (kind filtering).
	tools := run("tool")
	if len(tools.Boundary.Boundaries) != 1 {
		t.Fatalf("tools boundaries = %+v", tools.Boundary.Boundaries)
	}
	if b := tools.Boundary.Boundaries[0]; b.Kind != "tool" || b.Name != "brain refresh" {
		t.Fatalf("unexpected tool boundary: %+v", b)
	}
	if len(tools.Boundary.Handlers) != 1 || tools.Boundary.Handlers[0].Name != "Refresh" {
		t.Fatalf("tools handlers = %+v", tools.Boundary.Handlers)
	}

	// workflows: no workflow relations -> legitimately empty (not an error,
	// and the import external node must never surface as a boundary).
	workflows := run("workflow")
	if len(workflows.Boundary.Boundaries) != 0 || len(workflows.Boundary.Relations) != 0 {
		t.Fatalf("workflows should be empty: %+v", workflows.Boundary)
	}

	// An empty boundary view must say so in text mode rather than print nothing,
	// so "no workflows" is distinguishable from a broken command. Use a fresh
	// root command so the earlier --json flag does not carry over.
	textCmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
	out, err := execute(t, textCmd, "inspect", "boundaries", "--kind", "workflow")
	if err != nil {
		t.Fatalf("workflows text: %v", err)
	}
	if !strings.Contains(out, "no workflows found in the semantic index") {
		t.Fatalf("workflows empty-state message missing:\n%s", out)
	}
}
