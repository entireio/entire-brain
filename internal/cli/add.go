package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

type addFlags struct {
	dir             string
	build           bool
	checkpointLimit int
}

func newAddCommand(opts Options) *cobra.Command {
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

func runBrainAdd(cmd *cobra.Command, opts Options, flags addFlags, repoURL string) error {
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
	dest := filepath.Join(flags.dir, name)

	// 1. Clone (skip if the checkout already exists).
	if isGitCheckout(dest) {
		fmt.Fprintf(out, "clone: %s already present, skipping\n", dest)
	} else {
		fmt.Fprintf(out, "clone: %s -> %s\n", repoURL, dest)
		// "--" ends option parsing so a repo URL can never be read as a git flag.
		if _, stderr, err := opts.Runner.Run(ctx, "", "git", "clone", "--quiet", "--", repoURL, dest); err != nil {
			return fmt.Errorf("clone_failed: %v: %s", err, strings.TrimSpace(string(stderr)))
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
		fmt.Fprintf(out, "added %s at %s (clone only; build with `ENTIRE_REPO_ROOT=%s entire brain refresh`)\n", name, dest, absDest)
		return nil
	}
	if err := buildClonedBrain(ctx, cmd, flags, absDest); err != nil {
		// The clone is kept on disk; surface a non-zero exit so scripts/CI don't
		// treat a failed build as success, with manual-build guidance.
		fmt.Fprintf(errOut, "cloned to %s, but the brain build failed; build it manually:\n  ENTIRE_REPO_ROOT=%s entire brain refresh\n", dest, absDest)
		return fmt.Errorf("brain_build_failed: %w", err)
	}
	fmt.Fprintf(out, "added %s at %s — brain built. Explore it with `entire brain dash` (or `status`) from %s.\n", name, dest, dest)
	return nil
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
