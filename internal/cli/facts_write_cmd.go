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
	agentCommand []string
	json         bool
	run          distillAgentRunner
}

func newRememberCommand(opts Options) *cobra.Command {
	rememberOpts := rememberCommandOptions{agent: "auto"}
	cmd := &cobra.Command{
		Use:   "remember <fact>",
		Short: "Author a durable fact about this repository",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRemember(cmd.Context(), cmd, opts, rememberOpts, args[0])
		},
	}
	cmd.Flags().StringVar(&rememberOpts.path, "path", "", "Taxonomy path(s), comma-separated (e.g. preferences.coding.style). If omitted, the agent classifies the fact")
	cmd.Flags().StringVar(&rememberOpts.kind, "kind", "", "Fact kind: decision|invariant|gotcha|preference|convention. If omitted, it is inferred")
	cmd.Flags().StringVar(&rememberOpts.branch, "branch", "", "Branch to store the fact on (default: current branch)")
	cmd.Flags().StringVar(&rememberOpts.agent, "agent", "auto", "Agent used to classify when --path is omitted: auto, codex, claude-code, command, or none")
	cmd.Flags().StringArrayVar(&rememberOpts.agentCommand, "agent-command", nil, "Agent command argv for --agent command")
	cmd.Flags().BoolVar(&rememberOpts.json, "json", false, "Emit the created fact as JSON")
	return cmd
}

func runRemember(ctx context.Context, cmd *cobra.Command, opts Options, rememberOpts rememberCommandOptions, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("fact text must not be empty")
	}
	repoDir, brainDir, branch, err := resolveFactsTarget(ctx, opts, agentSurfaceTarget(opts, nil), rememberOpts.branch)
	if err != nil {
		return err
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
	if commit, gitErr := gitScalar(ctx, opts.Runner, repoDir, "rev-parse", "HEAD"); gitErr == nil {
		anchor.Commit = strings.TrimSpace(commit)
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

	facts, err := loadFacts(brainDir, branch)
	if err != nil {
		return err
	}
	facts = upsertFact(facts, record)
	// An explicit --kind is a deliberate human correction and must win even when
	// the fact already exists, where upsertFact's anti-thrash rule would
	// otherwise keep the stored kind. The reported kind is then always the one
	// actually persisted, never a discarded request.
	storedKind := kind
	if i := indexOfFact(facts, record.ID); i >= 0 {
		if explicitKind != "" {
			facts[i].Kind = explicitKind
		}
		storedKind = factKindOrInferred(facts[i])
	}
	if err := writeFacts(brainDir, branch, facts); err != nil {
		return err
	}
	if err := writeFactTaxonomy(brainDir, taxonomy); err != nil {
		return err
	}
	if err := updateFactSourceManifest(brainDir, now); err != nil {
		return err
	}

	record.Kind = storedKind // report what was actually persisted
	if rememberOpts.json {
		return writeJSON(cmd, record)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "remembered %s [%s] %s on %s\n", record.ID, storedKind, strings.Join(paths, ","), branch)
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
	run := rememberOpts.run
	if run == nil {
		run = execDistillAgent
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
