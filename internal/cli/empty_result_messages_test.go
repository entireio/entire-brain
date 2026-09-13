package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A command that prints nothing and exits 0 is the one result a caller cannot
// act on. "No symbol matched", "the index was never built" and "this command is
// broken" all look identical, so the honest reading of silence is "something
// went wrong" — which is why an agent re-runs it, and a human reaches for
// --json to find out whether the tool works at all.
//
// Seven commands did exactly that on a no-match query while their --json
// siblings correctly emitted an empty result set, and while `inspect
// boundaries` ("no routes found in the semantic index"), `inspect changes`
// ("no changes since the indexed HEAD") and `inspect snippet` already said so
// in prose. This pins the human path for all seven: an empty result must be
// stated, exit 0 must stay exit 0, and --json must not move.
func TestEmptySemanticResultsAreStatedNotSilent(t *testing.T) {
	const noMatch = "zzzz-no-such-symbol"

	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	opts := Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now}
	if _, err := execute(t, NewRootCommand(opts), "refresh", "index", "--graph-binary", "entire"); err != nil {
		t.Fatalf("index: %v", err)
	}

	// The idiom every one of these must speak, copied from `inspect boundaries`.
	const idiom = "found in the semantic index"

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "inspect code", args: []string{"inspect", "code", noMatch}, want: "no symbols " + idiom},
		{name: "inspect search-graph", args: []string{"inspect", "search-graph", noMatch}, want: "no symbols " + idiom},
		{name: "inspect context", args: []string{"inspect", "context", noMatch}, want: "no context " + idiom},
		{name: "inspect impact", args: []string{"inspect", "impact", noMatch}, want: "no impact " + idiom},
		{name: "inspect tests", args: []string{"inspect", "tests", noMatch}, want: "no tests " + idiom},
		{name: "inspect query-graph", args: []string{"inspect", "query-graph", "type:NOPE " + noMatch}, want: "no relations " + idiom},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := execute(t, NewRootCommand(opts), tc.args...)
			if err != nil {
				t.Fatalf("%s failed: %v\n%s", tc.name, err, out)
			}
			if strings.TrimSpace(out) == "" {
				t.Fatalf("%s printed nothing on a no-match query", tc.name)
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("%s should say %q, got:\n%s", tc.name, tc.want, out)
			}
			// The message has to be about THIS query, or an empty index and an
			// unlucky search term still read the same.
			if !strings.Contains(out, noMatch) {
				t.Fatalf("%s empty-state line does not name the query:\n%s", tc.name, out)
			}
		})
	}

	// A match still renders exactly as before: the empty-state line is an
	// addition to the silent path, not a replacement for the result path.
	t.Run("a matching query is unaffected", func(t *testing.T) {
		out, err := execute(t, NewRootCommand(opts), "inspect", "code", "ValidateToken")
		if err != nil {
			t.Fatalf("inspect code: %v\n%s", err, out)
		}
		if strings.Contains(out, idiom) {
			t.Fatalf("a matching query must not print an empty-state line:\n%s", out)
		}
		if !strings.Contains(out, "ValidateToken") {
			t.Fatalf("inspect code lost its result:\n%s", out)
		}
	})

	// --json was already correct, and MCP reads it. It must not move.
	t.Run("--json still emits an empty result set", func(t *testing.T) {
		out, err := execute(t, NewRootCommand(opts), "inspect", "code", noMatch, "--json")
		if err != nil {
			t.Fatalf("inspect code --json: %v\n%s", err, out)
		}
		var report struct {
			Results []json.RawMessage `json:"results"`
		}
		if decodeErr := json.Unmarshal([]byte(out), &report); decodeErr != nil {
			t.Fatalf("--json output is not JSON (an empty-state line leaked into it?): %v\n%s", decodeErr, out)
		}
		if len(report.Results) != 0 {
			t.Fatalf("--json results should be empty, got %d", len(report.Results))
		}
	})
}

// TestPrivacyListEmptyStateIsStated covers the seventh silent command. It is
// not a semantic read, so it gets the plain-language sibling of the same rule:
// the one command a privacy-conscious user reaches for must never answer "what
// have you captured about me?" with zero bytes.
func TestPrivacyListEmptyStateIsStated(t *testing.T) {
	f := newVerifyFixture(t)
	f.writeSessions(t, nil)

	out, err := execute(t, NewRootCommand(f.opts), "privacy", "list")
	if err != nil {
		t.Fatalf("privacy list: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatal("privacy list printed nothing for a brain with no captured sessions")
	}
	if !strings.Contains(out, "no captured sessions") {
		t.Fatalf("privacy list should state the empty case, got:\n%s", out)
	}

	// --json keeps its shape.
	jsonOut, jsonErr := execute(t, NewRootCommand(f.opts), "privacy", "list", "--json")
	if jsonErr != nil {
		t.Fatalf("privacy list --json: %v\n%s", jsonErr, jsonOut)
	}
	var report struct {
		Sessions []json.RawMessage `json:"sessions"`
	}
	if decodeErr := json.Unmarshal([]byte(jsonOut), &report); decodeErr != nil {
		t.Fatalf("--json output is not JSON: %v\n%s", decodeErr, jsonOut)
	}
	if len(report.Sessions) != 0 {
		t.Fatalf("--json sessions should be empty, got %d", len(report.Sessions))
	}

	// A listed session still renders, so the empty-state line cannot be
	// masking the real one.
	f.writeSessions(t, []exportSession{{
		SessionID: "session-1", Branch: "main", Agent: "claude-code",
		CreatedAt: f.now, TranscriptPath: "sessions/session-1.jsonl",
	}})
	populated, populatedErr := execute(t, NewRootCommand(f.opts), "privacy", "list")
	if populatedErr != nil {
		t.Fatalf("privacy list: %v\n%s", populatedErr, populated)
	}
	if strings.Contains(populated, "no captured sessions") {
		t.Fatalf("privacy list reported empty while holding a session:\n%s", populated)
	}
	if !strings.Contains(populated, "session-1") {
		t.Fatalf("privacy list lost its row:\n%s", populated)
	}
}
