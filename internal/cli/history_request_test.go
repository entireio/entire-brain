package cli

import "testing"

func TestExtractUserRequestFragments(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string // "" means no fragment expected
	}{
		{"codex user_message", `{"type":"event_msg","payload":{"type":"user_message","message":"Add a subcommand to entire brain"}}`, "Add a subcommand to entire brain"},
		{"codex response_item user", `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"text","text":"Why are there two brains"}]}}`, "Why are there two brains"},
		{"claude user string", `{"type":"user","message":{"role":"user","content":"Fix the failing test"}}`, "Fix the failing test"},
		{"claude user text block", `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Rename the variable"}]}}`, "Rename the variable"},
		{"claude tool_result skipped", `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"exit 0"}]}}`, ""},
		{"assistant not a request", `{"type":"agent_message","message":"I implemented the fix"}`, ""},
		{"codex assistant not a request", `{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"text","text":"done"}]}}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frags := extractUserRequestFragments(tc.line)
			if tc.want == "" {
				if len(frags) != 0 {
					t.Fatalf("expected no fragment, got %+v", frags)
				}
				return
			}
			if len(frags) != 1 || frags[0].Source != "user_request" || frags[0].Text != tc.want {
				t.Fatalf("got %+v, want one user_request %q", frags, tc.want)
			}
		})
	}
}
