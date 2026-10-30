package cli

import bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"

// The brain-manifest schema lives in internal/cli/internal/manifest
// (COR-2268). These aliases keep the package-local spellings compiling
// while command clusters move out; they retire with the cli package.
type (
	exportManifest           = bmanifest.Manifest
	exportBranch             = bmanifest.Branch
	exportSession            = bmanifest.Session
	exportAuthor             = bmanifest.Author
	brainSources             = bmanifest.Sources
	sessionSourceManifest    = bmanifest.SessionSource
	seedSourceManifest       = bmanifest.SeedSource
	seedDocument             = bmanifest.SeedDocument
	seedCommand              = bmanifest.SeedCommand
	seedHistoryBaseline      = bmanifest.SeedHistoryBaseline
	seedHistoryCoverage      = bmanifest.SeedHistoryCoverage
	seedCoveredCommit        = bmanifest.SeedCoveredCommit
	seedAgentManifest        = bmanifest.SeedAgent
	seedAgentPhase           = bmanifest.SeedAgentPhase
	semanticSourceManifest   = bmanifest.SemanticSource
	semanticWarning          = bmanifest.SemanticWarning
	semanticHeader           = bmanifest.SemanticHeader
	graphStoreDescriptor     = bmanifest.GraphStoreDescriptor
	historySourceManifest    = bmanifest.HistorySource
	factSourceManifest       = bmanifest.FactSource
	docSourceManifest        = bmanifest.DocSource
	patternSourceManifest    = bmanifest.PatternSource
	checkpointTokenUsage     = bmanifest.CheckpointTokenUsage
	checkpointSummary        = bmanifest.CheckpointSummary
	distillTokenUsageSummary = bmanifest.DistillTokenUsageSummary
)
