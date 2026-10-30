package add

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/spf13/cobra"
)

type addFlags struct {
	dir             string
	build           bool
	checkpointLimit int
}

func New(opts base.Options) *cobra.Command {
	var flags addFlags
	cmd := &cobra.Command{
		Use:   "add <repo-url>",
		Short: "Clone a repository, fetch its Entire history, and build its brain",
		Long: `add readies a repository for exploration without a manual clone: it clones the
repository, fetches its Entire checkpoint history (refs/heads/entire/*), and
builds the brain by running ` + "`refresh`" + ` against the clone. Afterward explore it
with ` + "`entire brain dash`" + `, ` + "`status`" + `, or ` + "`search`" + ` from inside the clone.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBrainAdd(cmd, opts, flags, args[0])
		},
	}
	cmd.Flags().StringVar(&flags.dir, "dir", ".", "Directory to clone the repository into")
	cmd.Flags().BoolVar(&flags.build, "build", true, "Build the brain after cloning (use --build=false to clone only)")
	cmd.Flags().IntVar(&flags.checkpointLimit, "checkpoint-limit", 0, "Limit checkpoints scanned when building the brain (0 = refresh default)")
	return cmd
}

func runBrainAdd(cmd *cobra.Command, opts base.Options, flags addFlags, repoURL string) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()

	name := repoDirName(repoURL)
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("invalid_repo_url: cannot derive a safe directory name from %q", repoURL)
	}
	if strings.HasPrefix(repoURL, "-") {
		return fmt.Errorf("invalid_repo_url: %q must not start with '-'", repoURL)
	}
	if base.BrainNoEgressMode() {
		return fmt.Errorf("no_egress: add requires cloning or fetching repository history; unset ENTIRE_BRAIN_NO_EGRESS to allow it")
	}
	dest := filepath.Join(flags.dir, name)

	// 1. Clone (skip if the checkout already exists).
	if isGitCheckout(dest) {
		origin, _, err := opts.Runner.Run(ctx, dest, "git", "remote", "get-url", "origin")
		if err != nil || !sameCloneOrigin(repoURL, strings.TrimSpace(string(origin))) {
			return fmt.Errorf("repository_collision: %s already exists with a different or unreadable origin; choose another --dir", dest)
		}
		fmt.Fprintf(out, "clone: %s already present, skipping\n", dest)
	} else {
		fmt.Fprintf(out, "clone: %s -> %s\n", repoURL, dest)
		// "--" ends option parsing so a repo URL can never be read as a git flag.
		if _, stderr, err := opts.Runner.Run(ctx, "", "git", "clone", "--quiet", "--", repoURL, dest); err != nil {
			return fmt.Errorf("clone_failed: %s", cloneFailureMessage(repoURL, stderr, err))
		}
	}

	// 2. Fetch the Entire checkpoint history (best-effort: a repo may have none).
	if _, stderr, err := opts.Runner.Run(ctx, dest, "git", "fetch", "--quiet", "origin", "refs/heads/entire/*:refs/heads/entire/*"); err != nil {
		fmt.Fprintf(errOut, "note: no Entire history fetched (%v): %s\n", err, strings.TrimSpace(string(stderr)))
	}

	// An absolute path is needed for the build env (ENTIRE_REPO_ROOT) and the
	// manual-build guidance; a failure here (e.g. os.Getwd failing) would
	// otherwise silently produce an empty "ENTIRE_REPO_ROOT=" suggestion.
	absDest, err := filepath.Abs(dest)
	if err != nil {
		return fmt.Errorf("resolve_clone_path: %q: %w", dest, err)
	}

	// 3. Build the brain.
	if !flags.build {
		fmt.Fprintf(out, "added %s at %s (clone only; build with `ENTIRE_REPO_ROOT=%s "+base.BrainInvocation(os.LookupEnv, "refresh")+"`)\n", name, dest, absDest)
		return nil
	}
	if err := buildClonedBrain(ctx, cmd, flags, absDest); err != nil {
		// The clone is kept on disk; surface a non-zero exit so scripts/CI don't
		// treat a failed build as success, with manual-build guidance.
		fmt.Fprintf(errOut, "cloned to %s, but the brain build failed; build it manually:\n  ENTIRE_REPO_ROOT=%s "+base.BrainInvocation(os.LookupEnv, "refresh")+"\n", dest, absDest)
		return fmt.Errorf("brain_build_failed: %w", err)
	}
	fmt.Fprintf(out, "added %s at %s — brain built. Explore it with `"+base.BrainInvocation(os.LookupEnv, "dash")+"` (or `status`) from %s.\n", name, dest, dest)
	return nil
}

// cloneFailureMessage reduces a failed `git clone` to one human-readable line.
// The runner error already embeds the git argv and stderr, so formatting it
// verbatim dumped the argv and printed stderr twice (COR-2206); the message is
// built from stderr alone, falling back to the root cause when stderr is empty.
func cloneFailureMessage(repoURL string, stderr []byte, err error) string {
	lower := strings.ToLower(string(stderr))
	// Only genuine repo-missing or auth-denied signals classify. "repository ...
	// not found" covers git's own fatal line, which quotes the URL between the
	// two words; both words must sit on ONE line, or unrelated lines combine
	// (e.g. "git-upload-pack: command not found" plus the "Could not read from
	// remote repository." footer). "permission denied (publickey" is SSH auth
	// specifically — bare "permission denied" also appears in local failures
	// (e.g. creating the work tree dir), and git's "correct access rights"
	// footer accompanies most SSH transport failures (host key, connection
	// refused), so neither classifies.
	repoNotFound := false
	for _, line := range strings.Split(lower, "\n") {
		if strings.Contains(line, "repository") && strings.Contains(line, "not found") {
			repoNotFound = true
			break
		}
	}
	notFoundOrNoAccess := repoNotFound ||
		strings.Contains(lower, "authentication failed") ||
		strings.Contains(lower, "could not read username") ||
		strings.Contains(lower, "permission denied (publickey")
	if notFoundOrNoAccess {
		return repoURL + ": repository not found (or no access)"
	}
	seen := map[string]bool{}
	var lines, specific []string
	for _, line := range strings.Split(strings.TrimSpace(string(stderr)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || seen[line] {
			continue
		}
		seen[line] = true
		lines = append(lines, line)
		if !genericCloneFooterLine(line) {
			specific = append(specific, line)
		}
	}
	// Prefer the cause-bearing lines; the footer only survives when it is all
	// git printed.
	if len(specific) > 0 {
		lines = specific
	}
	if len(lines) == 0 {
		// The runner wraps the exec error under argv-bearing text; unwrap to the
		// root (e.g. "exit status 128") to keep the argv off the human surface.
		for root := errors.Unwrap(err); root != nil; root = errors.Unwrap(root) {
			err = root
		}
		return fmt.Sprintf("%s: %v", repoURL, err)
	}
	return repoURL + ": " + strings.Join(lines, "; ")
}

// genericCloneFooterLine reports whether a stderr line is part of the advice
// footer git appends to most SSH transport failures. The footer carries no
// cause, so it must not crowd out the specific error line above it.
func genericCloneFooterLine(line string) bool {
	lower := strings.ToLower(line)
	return strings.Contains(lower, "make sure you have the correct access rights") ||
		strings.Contains(lower, "and the repository exists") ||
		strings.Contains(lower, "could not read from remote repository")
}

// buildClonedBrain builds the cloned repo's brain by running `refresh` with the
// repo root pointed at the clone. It re-execs this binary so the refresh runs
// with exactly the same defaults as a normal `entire brain refresh` (the refresh
// stages shell the host `entire` CLI, which must be on PATH).
func buildClonedBrain(ctx context.Context, cmd *cobra.Command, flags addFlags, repoDir string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate self: %w", err)
	}
	args := []string{"refresh"}
	if flags.checkpointLimit > 0 {
		args = append(args, "--checkpoint-limit", strconv.Itoa(flags.checkpointLimit))
	}
	c := exec.CommandContext(ctx, self, args...)
	c.Dir = repoDir
	c.Env = append(os.Environ(), "ENTIRE_REPO_ROOT="+repoDir)
	c.Stdout = cmd.OutOrStdout()
	c.Stderr = cmd.ErrOrStderr()
	return c.Run()
}

// repoDirName derives a local directory name from a repo URL, preferring
// "<owner>_<repo>". It handles https and scp-like (git@host:owner/repo) URLs and
// strips a trailing .git.
func repoDirName(repoURL string) string {
	u := strings.TrimSuffix(strings.TrimSpace(repoURL), "/")
	u = strings.TrimSuffix(u, ".git")
	if i := strings.Index(u, "://"); i >= 0 {
		rest := u[i+3:]
		if s := strings.IndexByte(rest, '/'); s >= 0 {
			u = rest[s+1:] // drop host
		} else {
			u = rest
		}
	} else if i := strings.LastIndex(u, ":"); i >= 0 {
		u = u[i+1:] // scp-like git@host:owner/repo
	}
	parts := make([]string, 0, 3)
	for _, p := range strings.Split(u, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	switch {
	case len(parts) >= 2:
		return parts[len(parts)-2] + "_" + parts[len(parts)-1]
	case len(parts) == 1:
		return parts[0]
	default:
		return ""
	}
}

// isGitCheckout reports whether dir already holds a git checkout.
func isGitCheckout(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil && (info.IsDir() || info.Mode().IsRegular())
}

// Compare complete host/path identities without the lossy directory-name or
// store-key sanitization. HTTPS and ordinary SCP-style SSH URLs may name the
// same repository; non-default ports and path case remain significant.
func sameCloneOrigin(want, got string) bool {
	if got == "" {
		return false
	}
	identity := func(remote string) string {
		remote = strings.TrimSpace(remote)
		host, path := "", ""
		if u, err := url.Parse(remote); err == nil && u.Host != "" && u.Scheme != "" {
			host, path = strings.ToLower(u.Host), u.EscapedPath()
		} else if m := base.RemoteSCPRegex.FindStringSubmatch(remote); len(m) == 3 {
			host, path = strings.ToLower(m[1]), m[2]
		} else {
			return strings.TrimSuffix(strings.TrimSuffix(remote, "/"), ".git")
		}
		return host + "/" + strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	}
	return identity(want) == identity(got)
}
