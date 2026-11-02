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
		Name:       "brief must preserve raw-only matches",
		File:       "internal/cli/agent_surface.go",
		Before:     "rawMatches, rawErr := rawHistoryMatcher(status.Brain.Path, task, nil, briefOpts.limit, rawProfile)",
		After:      "_ = rawProfile\n\t\t\trawMatches, rawErr := []brainTextMatch(nil), error(nil)",
		Test:       "TestRawHistoryFallbackRetainsFallbackOnlyMatch",
		Diagnostic: "end-to-end brief lost fallback-only match",
		Package:    "./internal/cli",
	},
	{
		Name:       "mirror must sweep abstracts of a removed checkpoint",
		File:       "internal/cli/facts_mirror.go",
		Before:     "\t\tif !liveRefs[entry.Artifact.SessionRef] {",
		After:      "\t\tif false && !liveRefs[entry.Artifact.SessionRef] {",
		Test:       "TestMirrorSweepsAbstractsOfMissingSessions",
		Diagnostic: "want one abstract and one receipt removed",
		Package:    "./internal/cli",
	},
}
