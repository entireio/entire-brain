package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

type rememberCommandOptions struct {
	path         string
	kind         string
	branch       string
	agent        string
	model        string
	agentCommand []string
	json         bool
	global       bool
	run          distillAgentRunner
}

func newRememberCommand(opts Options) *cobra.Command {
	rememberOpts := rememberCommandOptions{agent: "auto"}
	cmd := &cobra.Command{
		Use:   "remember <fact>",
		Short: "Record a durable fact about this repository",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRemember(cmd.Context(), cmd, opts, rememberOpts, args[0])
		},
	}
	cmd.Flags().StringVar(&rememberOpts.path, "path", "", "Taxonomy path(s), comma-separated (e.g. preferences.coding.style). If omitted, the agent classifies the fact")
	cmd.Flags().StringVar(&rememberOpts.kind, "kind", "", "Fact kind: decision|invariant|gotcha|preference|convention. If omitted, it is inferred")
	cmd.Flags().StringVar(&rememberOpts.branch, "branch", "", "Branch to store the fact on (default: current branch)")
	cmd.Flags().StringVar(&rememberOpts.agent, "agent", "auto", "Agent used to classify when --path is omitted: auto, codex, claude-code, ollama, command, or none")
	cmd.Flags().StringVar(&rememberOpts.model, "model", "", "Model for codex/claude-code/ollama classification")
	cmd.Flags().StringArrayVar(&rememberOpts.agentCommand, "agent-command", nil, "Agent command argv for --agent command")
	cmd.Flags().BoolVar(&rememberOpts.json, "json", false, "Emit the created fact as JSON")
	cmd.Flags().BoolVar(&rememberOpts.global, "global", false, "Record the fact globally, so every repository recalls it")
	return cmd
}

// rememberAnchorCommit resolves the commit an authored fact should cite.
//
// `remember --branch <other>` files the fact on ANOTHER branch without moving
// the working tree, so HEAD is the wrong evidence: it is the CURRENT branch's
// tip, and it may not be on <other> at all. The stored fact then reads "on
// <other>, evidenced by <a commit that branch never had>" — and verification
// launders it, because verifyCommitUncached accepts reachability from ANY local
// ref rather than from the fact's branch.
//
// With an explicit override the named branch's tip is the only honest answer,
// and a branch git cannot resolve yields NO commit: an absent anchor is honest
// (verify reports it as unverifiable), a wrong one is not. Without an override
// the branch was derived from HEAD in the first place, so HEAD stays the
// answer — which also keeps a detached HEAD, where there is no branch to
// resolve, citing the commit actually checked out.
func rememberAnchorCommit(ctx context.Context, opts Options, repoDir, branch string, explicit bool) (string, bool) {
	args := []string{"rev-parse", "HEAD"}
	if explicit {
		args = []string{"rev-parse", "--verify", branch + "^{commit}"}
	}
	commit, err := gitScalar(ctx, opts.Runner, repoDir, args...)
	if err != nil {
		return "", false
	}
	commit = strings.TrimSpace(commit)
	return commit, commit != ""
}

func runRemember(ctx context.Context, cmd *cobra.Command, opts Options, rememberOpts rememberCommandOptions, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("fact text must not be empty")
	}
	// A fact that is not about one repository has nowhere to go in a per-repo
	// store, and outside a checkout there is no store at all. Both cases file
	// globally: explicitly when asked, and automatically rather than refusing,
	// because "you are not in a repository" is a reason to pick a different
	// store, not a reason to discard what somebody just typed.
	global := rememberOpts.global
	if !global && strings.TrimSpace(rememberOpts.branch) == "" {
		here, err := inARepository(ctx, opts)
		if err != nil {
			// An undecidable repository identity is not an invitation to file
			// the fact somewhere else. Surfacing it is the whole point of the
			// guard that raised it.
			return err
		}
		if !here {
			global = true
			fmt.Fprintln(cmd.ErrOrStderr(), globalFactNotice(false))
		}
	}

	var (
		repoDir  string
		brainDir string
		branch   string
		err      error
	)
	if global {
		if strings.TrimSpace(rememberOpts.branch) != "" {
			// A global fact is not on a branch, and quietly ignoring --branch
			// would file it somewhere the user did not ask for.
			return fmt.Errorf("--global and --branch cannot be combined: a global fact is not on a branch")
		}
		brainDir, branch, err = resolveGlobalFactsTarget(opts.Env)
		if err != nil {
			return err
		}
		// repoDir stays empty on purpose: classification and the commit anchor
		// below both degrade to "no repository", which is the truth.
		repoDir = ""
	} else {
		repoDir, brainDir, branch, err = resolveFactsTarget(ctx, opts, agentSurfaceTarget(opts, nil), rememberOpts.branch)
		if err != nil {
			return err
		}
	}
	now := opts.Now().UTC()
	taxonomy, err := loadFactTaxonomy(brainDir, now)
	if err != nil {
		return err
	}

	paths, err := resolveRememberPaths(ctx, opts, rememberOpts, repoDir, text, taxonomy)
	if err != nil {
		return err
	}

	if err := validateKindFlag(rememberOpts.kind); err != nil {
		return err
	}
	explicitKind := strings.ToLower(strings.TrimSpace(rememberOpts.kind))
	kind := explicitKind
	if kind == "" {
		kind = inferFactKind(paths, text)
	}

	anchor := factAnchor{}
	// A global fact has no commit to cite. An anchor pointing at whatever
	// happened to be checked out would be worse than none: verification would
	// resolve it and report the fact as evidenced by a commit it has nothing to
	// do with.
	if !global {
		if commit, ok := rememberAnchorCommit(ctx, opts, repoDir, branch, strings.TrimSpace(rememberOpts.branch) != ""); ok {
			anchor.Commit = commit
		}
	}

	record := factRecord{
		ID:         factRecordID(text, paths),
		Paths:      paths,
		Kind:       kind,
		Locus:      factLocus(text),
		Text:       text,
		Branch:     branch,
		Origin:     factOriginAuthored,
		Status:     factStatusActive,
		Provenance: []factAnchor{anchor},
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	storedKind := kind
	if err := withBrainWriteLock(brainDir, func() error {
		facts, err := loadFacts(brainDir, branch)
		if err != nil {
			return err
		}
		facts = upsertFact(facts, record)
		if i := indexOfFact(facts, record.ID); i >= 0 {
			// Re-authoring is a deliberate human assertion that the statement is
			// true again. A fact id is content-derived, so `remember` of a
			// statement that was previously retracted (or superseded) matches the
			// retired record, and upsertFact only unions provenance — it never
			// revives a status, which is exactly right for a re-distill but wrong
			// here: without this the command prints "remembered ..." while the
			// fact stays invisible to recall, and the author's write is silently
			// lost. Reviving keeps the record and its history; it only restores
			// the status the author just asserted.
			if facts[i].Status != factStatusActive {
				facts[i].Status = factStatusActive
				facts[i].SupersededBy = ""
				facts[i].UpdatedAt = now
			}
			// An explicit --kind is a deliberate human correction and must win even
			// when the fact already exists, where upsertFact's anti-thrash rule would
			// otherwise keep the stored kind. The reported kind is then always the one
			// actually persisted, never a discarded request.
			if explicitKind != "" {
				facts[i].Kind = explicitKind
			}
			storedKind = factKindOrInferred(facts[i])
			record = facts[i]
		}
		if err := writeFacts(brainDir, branch, facts); err != nil {
			return err
		}
		if err := writeFactTaxonomy(brainDir, taxonomy); err != nil {
			return err
		}
		return updateFactSourceManifestLocked(brainDir, now)
	}); err != nil {
		return err
	}

	record.Kind = storedKind // report what was actually persisted
	// After the write, never before: a subscriber must not be told about a fact
	// that failed to persist. Delivery cannot fail this command — the fact is
	// already on disk and the author's write must not be undone by an
	// unreachable endpoint.
	if global {
		notifyGlobalFactWebhook(ctx, cmd.ErrOrStderr(), WebhookFactRecorded, record, now)
	} else {
		notifyFactWebhook(ctx, cmd.ErrOrStderr(), opts, WebhookFactRecorded, branch, record, now)
	}
	if rememberOpts.json {
		return writeJSON(cmd, record)
	}
	where := "on " + branch
	if global {
		where = "globally"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "remembered %s [%s] %s %s\n", record.ID, storedKind, strings.Join(paths, ","), where)
	return nil
}

// resolveRememberPaths returns the taxonomy paths for an authored fact: the
// explicit --path when given, otherwise the agent's classification. Without an
// agent and without --path it errors, mirroring the plan's contract.
func resolveRememberPaths(ctx context.Context, opts Options, rememberOpts rememberCommandOptions, repoDir, text string, taxonomy factTaxonomy) ([]string, error) {
	if strings.TrimSpace(rememberOpts.path) != "" {
		paths := normalizeFactPaths(strings.Split(rememberOpts.path, ","))
		if len(paths) == 0 {
			return nil, fmt.Errorf("--path %q is not a valid taxonomy path (category.subcategory.type)", rememberOpts.path)
		}
		// Explicit paths must all sit under a known top-level category, exactly
		// like agent-classified ones — otherwise --path can mint
		// immediately-orphaned facts the rest of the system treats as invalid.
		// Unlike the agent path (where unknown categories are dropped as noise),
		// an explicit --path is deliberate, so any unknown category is a hard
		// error rather than a silent drop that would hide a typo. New
		// three-level paths under an existing category are still allowed.
		var warnings []string
		filtered := filterFactPathsByTaxonomy(paths, taxonomy, &warnings)
		if len(filtered) != len(paths) {
			return nil, fmt.Errorf("--path %q is not under a known taxonomy category (%s)", rememberOpts.path, strings.Join(sortedTaxonomyTopLevels(taxonomy), ", "))
		}
		return filtered, nil
	}

	agent := rememberOpts.agent
	if agent == "auto" {
		agent = defaultRefreshAgent(ctx, opts.Runner, repoDir)
	}
	if agent == "none" || agent == "" {
		return nil, fmt.Errorf("no agent available to classify; pass --path <category.subcategory.type>")
	}
	args, err := distillAgentCommandArgs(agent, rememberOpts.agentCommand, classifyPrompt(taxonomy))
	if err != nil {
		return nil, err
	}
	args = injectAgentModel(args, agent, rememberOpts.model)
	run := rememberOpts.run
	if run == nil {
		run = defaultDistillAgentRunner(agent)
	}
	out, err := run(ctx, repoDir, args, []byte(text), defaultDistillTimeout)
	if err != nil {
		return nil, fmt.Errorf("classify fact: %w; pass --path to classify manually", err)
	}
	paths := parseClassifyOutput(out, taxonomy)
	if len(paths) == 0 {
		return nil, fmt.Errorf("agent did not return a valid taxonomy path; pass --path")
	}
	return paths, nil
}

// classifyPrompt instructs the agent to assign a taxonomy path to a single fact
// supplied on stdin. Like the other agent prompts it must not begin with a dash.
func classifyPrompt(taxonomy factTaxonomy) string {
	return "Classify the durable repository fact on stdin into the taxonomy below. " +
		"Output exactly one line: one taxonomy path, or two comma-separated paths only if the fact genuinely spans two top-level categories. " +
		"Each path is three lowercase segments: category.subcategory.type. Invent a new three-level path only under an existing top-level category. " +
		"Output only the path line, no prose.\n\n" + factTaxonomyBlock(taxonomy)
}

// parseClassifyOutput extracts taxonomy paths from the classifier's response:
// the first line containing a valid path, comma- or whitespace-separated,
// filtered to known top-level categories and capped at two.
func parseClassifyOutput(output string, taxonomy factTaxonomy) []string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.FieldsFunc(line, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
		paths := normalizeFactPaths(fields)
		var warnings []string
		paths = filterFactPathsByTaxonomy(paths, taxonomy, &warnings)
		if len(paths) > 0 {
			return paths
		}
	}
	return nil
}
