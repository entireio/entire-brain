package main

// These deliberate defects must fail their named regression assertions.
var regressionCases = []mutationCase{
	{
		Name:       "refresh must publish corpus",
		File:       "internal/cli/patterns_commands.go",
		Before:     "// Explicit refresh is the stricter surface: a corpus failure is fatal here.\n\tif err := buildPatternCorpus(brainDir, opts.Now().UTC(), patternCorpusGraphContext{Context: ctx, Options: opts, RepoDir: repoDir}); err != nil {\n\t\treturn nil, fmt.Errorf(\"pattern corpus: %w\", err)\n\t}",
		After:      "// MUTANT: omit corpus publication after the episode rebuild.\n\t_ = repoDir",
		Test:       "TestRootCommandPatternsLifecycleAndPrivacyFailures",
		Diagnostic: "refresh did not publish corpus",
		Package:    "./internal/cli",
	},
	{
		Name:       "mirror must delete facts of a removed checkpoint",
		File:       "internal/cli/facts_mirror.go",
		Before:     "\t\t\tif stripped > 0 && len(remaining) == 0 {",
		After:      "\t\t\tif false && stripped > 0 && len(remaining) == 0 {",
		Test:       "TestRefreshMirrorsCheckpointDeletion",
		Diagnostic: "anchored to the deleted session survived",
		Package:    "./internal/cli",
	},
	{
		Name:       "brief must emit precedent from fact anchors",
		File:       "internal/cli/agent_surface.go",
		Before:     "report.Precedent = brainBriefPrecedents(status.Brain.Path, report.Facts)",
		After:      "// MUTANT: drop the precedent section.\n\t\t\treport.Precedent = nil",
		Test:       "TestBrainBriefEmitsPrecedentFromFactAnchors",
		Diagnostic: "transcript-backed fact sources",
		Package:    "./internal/cli",
	},
}
