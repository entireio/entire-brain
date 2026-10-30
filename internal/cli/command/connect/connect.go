package connect

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/entireio/entire-brain/internal/apiurl"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/cmdutil"
	"github.com/entireio/entire-brain/internal/providerdetail"
	"github.com/spf13/cobra"
	"golang.org/x/mod/semver"
)

// Config supplies the shared command options and the assembly package's
// storage and hosted-sync operations. Dependencies are per invocation, never
// global hooks.
type Config struct {
	base.Options
	Deps Deps
}

// Deps keeps the connect command independent of command assembly, repo
// identity resolution, and the hosted sync engine. The assembly package
// supplies these operations so connect drives the same sync loop and push-hook
// management as the background triggers.
type Deps struct {
	RepoStoragePaths   func(context.Context, base.CommandRunner, base.EntireEnv, string) (base.RepoStorage, error)
	AgentSurfaceTarget func(base.Options, []string) string
	// InstallPushHook installs the pre-push upload hook; sharedHooksDir
	// reports a core.hooksPath shared across repos, where the hook is not
	// managed.
	InstallPushHook func(ctx context.Context, opts base.Options, repoDir string) (sharedHooksDir bool, hookPath string, err error)
	// HostedFactsSyncAndPull runs one hosted sync for the branch; attempted
	// reports whether the gates passed and the server was contacted,
	// pullSucceeded whether the pull half completed.
	HostedFactsSyncAndPull func(ctx context.Context, errW io.Writer, opts base.Options, repoDir string, storage base.RepoStorage, branch string) (attempted, pullSucceeded bool, err error)
	HostedSyncBranch       func(context.Context, base.CommandRunner, string) string
}

// minHostCLIVersion is the first host CLI release that ships `entire repo view
// --json` (entireio/cli#2758), which placement resolution depends on. Only
// connect's placement shell-out checks it: local-only brain use and explicit
// --repo-id/--api-url never touch the host CLI version. Empty disables the
// gate explicitly. A var so tests can exercise the gate.
var minHostCLIVersion = "0.11.6"

// hostedPlacement is the slice of the host CLI's `entire repo view --json`
// output this plugin needs (entireio/cli#2758): the repo's region-local ULID
// and its cell API base URL, both top-level fields.
type hostedPlacement struct {
	RepoID string `json:"repoId"`
	APIURL string `json:"apiUrl"`
}

type connectOptions struct {
	repoID string
	apiURL string
}

func New(cfg Config) *cobra.Command {
	var connectOpts connectOptions
	cmd := &cobra.Command{
		Use:   "connect",
		Short: "Bind this repository to its hosted brain explicitly",
		Long: `connect binds this repository to its hosted brain: it records the repo id and
API base URL (non-secrets only — the host CLI's own login mints a fresh token
per sync), then runs one immediate sync so the first pull happens right away.

Running connect (or the onboarding flow) is the ONLY way a repository starts
syncing: by default memories stay local and background triggers never connect
a repo on their own. Placement resolves through the host CLI (entire repo
view); pass --repo-id and --api-url to bypass resolution — a non-default cell
or a staging target. Re-running connect re-joins after 'entire brain
disconnect'.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConnect(cmd, cfg, connectOpts)
		},
	}
	cmd.Flags().StringVar(&connectOpts.repoID, "repo-id", "", "Hosted repo id (region-local ULID)")
	cmd.Flags().StringVar(&connectOpts.apiURL, "api-url", "", "Entire API base URL, https://")
	return cmd
}

func runConnect(cmd *cobra.Command, cfg Config, connectOpts connectOptions) error {
	ctx := cmd.Context()
	opts := cfg.Options
	repoDir, local, err := base.ResolveLocalTargetRepoDir(ctx, opts.Runner, cfg.Deps.AgentSurfaceTarget(opts, nil))
	if err != nil {
		return err
	}
	if !local {
		return fmt.Errorf("connect requires a local repository path")
	}
	storage, err := cfg.Deps.RepoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}

	repoID := strings.TrimSpace(connectOpts.repoID)
	apiURL := strings.TrimSpace(connectOpts.apiURL)
	if repoID == "" || apiURL == "" {
		resolved, placementErr := resolveHostedPlacement(ctx, opts.Runner, repoDir)
		if placementErr != nil {
			// An already-connected (or disconnected — the opt-out keeps its
			// coordinates) repo stays re-connectable without flags: re-connect
			// means re-sync, not re-resolve.
			if existing, present, readErr := brainstore.ReadHostedRepoBinding(storage.BrainDir); readErr == nil && present && existing.RepoID != "" {
				resolved = hostedPlacement{RepoID: existing.RepoID, APIURL: existing.BaseURL}
			} else {
				// The hint sits on its own line: the host CLI's message ends
				// with a newline of its own, so a "; pass ..." suffix used to
				// dangle on a line starting with ";".
				return fmt.Errorf("connect: %w\nOr pass --repo-id and --api-url explicitly", placementErr)
			}
		}
		if repoID == "" {
			repoID = resolved.RepoID
		}
		if apiURL == "" {
			apiURL = resolved.APIURL
		}
	}
	// Whether this run re-affirms an existing connection decides the success
	// headline: "connected" must not read like a state change when nothing
	// changed.
	existing, existingPresent, existingErr := brainstore.ReadHostedRepoBinding(storage.BrainDir)
	binding, err := bindHostedRepo(storage.BrainDir, repoID, apiURL)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	already := existingErr == nil && existingPresent && !existing.OptedOut &&
		existing.RepoID == binding.RepoID && existing.BaseURL == binding.BaseURL
	// A connected repo wants the push-beat upload: install our pre-push hook
	// (chained beside whatever hook owns the slot). Best-effort — a hook
	// problem must not fail the connect.
	if sharedHooksDir, hookPath, hookErr := cfg.Deps.InstallPushHook(ctx, opts, repoDir); hookErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "connect: push hook not installed (uploads ride the daemon tick): %v\n", hookErr)
	} else if sharedHooksDir {
		fmt.Fprintf(cmd.ErrOrStderr(), "connect: core.hooksPath (%s) is shared across repos; push hook not managed — uploads ride the daemon tick\n", hookPath)
	}
	// The kill switch suppresses every sync this command is about to promise,
	// so say that instead of "now syncing". The binding still gets recorded:
	// the environment only subtracts, and unsetting it resumes syncing.
	if base.BrainNoEgressMode() {
		cmdutil.RenderHostedBanner(cmd.OutOrStdout(),
			"Connected: "+storage.Key+" — but nothing syncs while "+base.NoEgressToggleName()+" is set",
			"The binding is recorded and memories stay local. Unset "+base.NoEgressToggleName()+" to start syncing.")
		return nil
	}
	// Connect doubles as the initial pull so a fresh clone receives team
	// memories immediately. The sync runs BEFORE the banner so the headline
	// reports what actually happened: a connect whose first pull failed has
	// not started syncing, and claiming so hides a dead connection.
	attempted, pullSucceeded, err := cfg.Deps.HostedFactsSyncAndPull(ctx, cmd.ErrOrStderr(), opts, repoDir, storage, cfg.Deps.HostedSyncBranch(ctx, opts.Runner, repoDir))
	if err != nil {
		return err
	}
	if !pullSucceeded {
		verdict := "failed"
		if !attempted {
			verdict = "did not run"
		}
		return fmt.Errorf("connect: %s is bound, but the first sync %s (details above); memories stay local until a sync succeeds — fix the cause and re-run '%s'",
			storage.Key, verdict, base.BrainInvocation(os.LookupEnv, "connect"))
	}
	headline := "Connected: " + storage.Key + " is now syncing memories"
	if already {
		headline = "Already connected: " + storage.Key + " — memories are syncing"
	}
	cmdutil.RenderHostedBanner(cmd.OutOrStdout(), headline,
		"Memories sync periodically, at session end, and on git push. Teammates' memories merge into this repo's local brain.")
	return nil
}

// normalizeHostedBinding validates the target and strips a quoted /api/v1
// suffix. Cell URLs are commonly quoted with that prefix (repo view's apiUrl,
// docs examples), but the factsync client appends /api/v1/repos/... itself —
// storing the suffix would 404 every sync. OptedOut is false: a fresh binding
// is an opt-in.
func normalizeHostedBinding(repoID, apiURL string) (brainstore.HostedRepoBinding, error) {
	if strings.TrimSpace(repoID) == "" {
		return brainstore.HostedRepoBinding{}, fmt.Errorf("hosted repo id is empty")
	}
	validated, err := apiurl.Validate(apiURL)
	if err != nil {
		return brainstore.HostedRepoBinding{}, err
	}
	return brainstore.HostedRepoBinding{RepoID: strings.TrimSpace(repoID), BaseURL: strings.TrimSuffix(validated, "/api/v1")}, nil
}

// bindHostedRepo is the EXPLICIT connect write: it overwrites whatever is
// there, including an opt-out — running connect is how the user opts back in.
func bindHostedRepo(brainDir, repoID, apiURL string) (brainstore.HostedRepoBinding, error) {
	binding, err := normalizeHostedBinding(repoID, apiURL)
	if err != nil {
		return brainstore.HostedRepoBinding{}, err
	}
	if err := brainstore.WriteHostedRepoBinding(brainDir, binding); err != nil {
		return brainstore.HostedRepoBinding{}, err
	}
	return binding, nil
}

// ResolveBinding resolves this repo's CURRENT hosted placement through the
// host CLI and normalizes it to a writable binding. The background sync uses
// it to detect a deleted-and-reonboarded repo behind a stale binding
// (COR-2279); it resolves only — writing a binding stays with connect and the
// sync's own guarded rebind.
func ResolveBinding(ctx context.Context, runner base.CommandRunner, repoDir string) (brainstore.HostedRepoBinding, error) {
	placement, err := resolveHostedPlacement(ctx, runner, repoDir)
	if err != nil {
		return brainstore.HostedRepoBinding{}, err
	}
	return normalizeHostedBinding(placement.RepoID, placement.APIURL)
}

// resolveHostedPlacement shells out to the host CLI for this repo's hosted
// placement.
func resolveHostedPlacement(ctx context.Context, runner base.CommandRunner, repoDir string) (hostedPlacement, error) {
	ctx, cancel := context.WithTimeout(ctx, base.HostedShellOutTimeout)
	defer cancel()
	if err := checkHostCLIVersion(ctx, runner, repoDir, minHostCLIVersion); err != nil {
		return hostedPlacement{}, err
	}
	stdout, stderr, err := runner.Run(ctx, repoDir, base.HostEntireBinary, "repo", "view", "--json")
	if err != nil {
		return hostedPlacement{}, placementFailureError(err, stderr)
	}
	var result hostedPlacement
	if err := base.DecodeHostCLIJSON(stdout, &result); err != nil {
		return hostedPlacement{}, fmt.Errorf("parse repo view json: %w", err)
	}
	return result, nil
}

// placementFailureError reduces a failed `entire repo view --json` to the line
// the host CLI actually said. The runner's wrap carries debug framing — an
// argv echo plus the host's "Using context '…'." login banner — that a human
// reading connect's failure never needs. Drop the banner, then let
// providerdetail.Summary keep the host's Error:-marked or first stderr line.
func placementFailureError(runErr error, stderr []byte) error {
	var kept []string
	for _, line := range strings.Split(string(stderr), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Using context ") {
			continue
		}
		kept = append(kept, line)
	}
	if summary := providerdetail.Summary(strings.Join(kept, "\n")); summary != "" {
		return errors.New(summary)
	}
	// Nothing usable on stderr: name the command and the cause under the
	// runner's argv echo (exit status, or a missing host CLI).
	cause := runErr
	if unwrapped := errors.Unwrap(runErr); unwrapped != nil {
		cause = unwrapped
	}
	return fmt.Errorf("`entire repo view --json`: %v", cause)
}

// checkHostCLIVersion refuses placement resolution on a host CLI older than
// min, so the failure says "upgrade" instead of repo view's unknown-command
// noise. A missing CLI or an unrecognized version line (dev builds) passes:
// the repo view call itself is the authority on whether the command exists.
func checkHostCLIVersion(ctx context.Context, runner base.CommandRunner, repoDir, min string) error {
	if min == "" {
		return nil
	}
	stdout, _, err := runner.Run(ctx, repoDir, base.HostEntireBinary, "version")
	if err != nil {
		return nil
	}
	// First line is "Entire CLI <version>", on some builds with a trailing
	// "(<commit>)". Take the first field that parses as semver, not a fixed
	// position, so a build suffix cannot fail-open the gate.
	line, _, _ := strings.Cut(strings.TrimSpace(string(stdout)), "\n")
	got := ""
	for _, field := range strings.Fields(line) {
		if v := "v" + strings.TrimPrefix(field, "v"); semver.IsValid(v) {
			got = v
			break
		}
	}
	if got == "" {
		return nil
	}
	// Compare the release core only: a prerelease or pseudo-version of min
	// (nightly, git describe) ranks below min in full semver order yet may
	// already ship repo view — those builds pass and repo view stays the
	// authority.
	core := strings.TrimSuffix(got, semver.Build(got))
	core = strings.TrimSuffix(core, semver.Prerelease(core))
	if semver.Compare(core, "v"+min) >= 0 {
		return nil
	}
	return fmt.Errorf("entire CLI %s does not support `entire repo view` (needs %s or newer); upgrade the entire CLI and re-run connect", strings.TrimPrefix(got, "v"), min)
}
