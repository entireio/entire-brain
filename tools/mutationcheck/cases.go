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
		Name:       "refresh must retain session exclusion",
		File:       "internal/cli/pattern_corpus.go",
		Before:     "\tsessions = bmanifest.GuardSessions(guard, sessions)",
		After:      "\t// Deliberate regression: skip retained exclusion filter.",
		Test:       "TestExcludedSessionSurvivesCancelledRefreshReopenAndRetry",
		Diagnostic: "cancelled refresh error = memory_state_unsafe: transcript sessions/main/20260802T000000Z_secret.jsonl is tombstoned",
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
		Name:       "privacy verification must detect excluded vector IDs",
		File:       "internal/cli/session_privacy_verify.go",
		Before:     "\t\tif _, current := wanted[id]; !current {",
		After:      "\t\tif _, current := wanted[id]; !current && false {",
		Test:       "TestPrivacyVectorSnapshotInspectsRealSQLiteMembership",
		Diagnostic: "want the excluded real SQLite record only",
		Package:    "./internal/cli",
	},
}
