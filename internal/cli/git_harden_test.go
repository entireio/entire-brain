package cli

import (
	"slices"
	"strings"
	"testing"
)

func TestHardenedGitArgs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "diff gains both flags after the subcommand",
			in:   []string{"diff", "--binary", "HEAD"},
			want: []string{"-c", "core.fsmonitor=false", "diff", "--no-ext-diff", "--no-textconv", "--binary", "HEAD"},
		},
		{
			name: "flags land before the pathspec separator",
			in:   []string{"diff", "--cached", "--binary", "HEAD", "--", ".", ":(exclude).env"},
			want: []string{"-c", "core.fsmonitor=false", "diff", "--no-ext-diff", "--no-textconv", "--cached", "--binary", "HEAD", "--", ".", ":(exclude).env"},
		},
		{
			name: "existing caller flags are not duplicated",
			in:   []string{"diff", "--unified=0", "--no-ext-diff", "--no-color", "HEAD"},
			want: []string{"-c", "core.fsmonitor=false", "diff", "--no-textconv", "--unified=0", "--no-ext-diff", "--no-color", "HEAD"},
		},
		{
			name: "a leading -c global option is stepped over",
			in:   []string{"-c", "core.quotepath=false", "diff", "--name-only", "-z"},
			want: []string{"-c", "core.fsmonitor=false", "-c", "core.quotepath=false", "diff", "--no-ext-diff", "--no-textconv", "--name-only", "-z"},
		},
		{
			name: "diff-tree is in the family",
			in:   []string{"-c", "core.quotepath=false", "diff-tree", "--no-commit-id", "-r", "deadbeef"},
			want: []string{"-c", "core.fsmonitor=false", "-c", "core.quotepath=false", "diff-tree", "--no-ext-diff", "--no-textconv", "--no-commit-id", "-r", "deadbeef"},
		},
		{
			name: "status rejects the diff flags so it only gets config",
			in:   []string{"status", "--porcelain", "--untracked-files=all"},
			want: []string{"-c", "core.fsmonitor=false", "status", "--porcelain", "--untracked-files=all"},
		},
		{
			name: "ls-files rejects the diff flags so it only gets config",
			in:   []string{"ls-files", "--others", "--exclude-standard"},
			want: []string{"-c", "core.fsmonitor=false", "ls-files", "--others", "--exclude-standard"},
		},
		{
			name: "clone keeps its argv intact after the config prefix",
			in:   []string{"clone", "--quiet", "--", "https://example.invalid/r.git", "dest"},
			want: []string{"-c", "core.fsmonitor=false", "clone", "--quiet", "--", "https://example.invalid/r.git", "dest"},
		},
		{
			name: "empty input yields nothing to run",
			in:   nil,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := hardenedGitArgs(tc.in...)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("hardenedGitArgs(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

// Every git invocation must end up with the fsmonitor neutralizer, and every
// invocation that can render patch text must end up with both diff flags. This
// guards the mapping itself rather than any one call site.
func TestHardenedGitArgsCoversEveryInvocation(t *testing.T) {
	t.Parallel()
	invocations := [][]string{
		{"status", "--porcelain"},
		{"status", "--porcelain", "--untracked-files=all"},
		{"diff", "--binary", "HEAD"},
		{"diff", "--cached", "--binary", "HEAD"},
		{"diff", "--name-status", "-M", "-C", "HEAD"},
		{"diff", "--unified=0", "--no-ext-diff", "--no-color", "--no-prefix", "HEAD"},
		{"diff", "--shortstat", "HEAD"},
		{"-c", "core.quotepath=false", "diff", "--no-ext-diff", "--no-textconv", "--name-only", "-z"},
		{"-c", "core.quotepath=false", "diff-tree", "--no-commit-id", "--name-only", "-z", "-r", "abc"},
		{"-c", "core.quotepath=false", "log", "--no-merges", "--format=%H"},
		{"log", "--no-walk", "--format=%H"},
		{"log", "--first-parent", "--format=%H", "--max-count=10", "main"},
		{"rev-list", "--count", "--first-parent", "main"},
		{"ls-files"},
		{"ls-files", "--others", "--exclude-standard"},
		{"ls-tree", "-r", "--name-only", "HEAD"},
		{"cat-file", "-p", "HEAD:a.txt"},
		{"rev-parse", "--show-toplevel"},
		{"merge-base", "--is-ancestor", "a", "b"},
		{"for-each-ref", "--format=%(refname:short)", "refs/heads"},
		{"branch", "--show-current"},
		{"remote", "get-url", "origin"},
		{"show-ref", "--verify", "--quiet", "refs/heads/main"},
		{"clone", "--quiet", "--", "https://example.invalid/r.git", "d"},
		{"fetch", "--quiet", "origin", "refs/heads/entire/*:refs/heads/entire/*"},
	}
	for _, args := range invocations {
		got := hardenedGitArgs(args...)
		joined := strings.Join(got, " ")
		if !strings.HasPrefix(joined, "-c core.fsmonitor=false ") {
			t.Errorf("%q: missing fsmonitor neutralizer, got %q", args, got)
		}
		sub := args[gitSubcommandIndex(args)]
		if gitHardenDiffFamily[sub] {
			for _, flag := range gitHardenDiffFlags {
				if !slices.Contains(got, flag) {
					t.Errorf("%q: diff-family invocation missing %s, got %q", args, flag, got)
				}
			}
		}
	}
}

// A repository controls .brainignore, and .brainignore feeds exclusion
// pathspecs into gitDiffBinary. A pathspec that spells a neutralizer flag must
// not be mistaken for the caller having already passed it.
func TestHardenedGitArgsIgnoresPathspecsWhenDeduplicating(t *testing.T) {
	t.Parallel()
	got := hardenedGitArgs("diff", "--binary", "HEAD", "--", ".", "--no-ext-diff", "--no-textconv")
	want := []string{
		"-c", "core.fsmonitor=false", "diff", "--no-ext-diff", "--no-textconv",
		"--binary", "HEAD", "--", ".", "--no-ext-diff", "--no-textconv",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("neutralizer suppressed by a pathspec\n got %q\nwant %q", got, want)
	}
}
