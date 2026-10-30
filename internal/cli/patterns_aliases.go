package cli

import (
	patterncore "github.com/entireio/entire-brain/internal/cli/patterns"
)

// Aliases for pattern view and skill-memory types that moved into the patterns
// feature package (COR-2268). Types only — functions were renamed at every
// call site.
type (
	patternView       = patterncore.PatternView
	themeView         = patterncore.ThemeView
	skillMemoryRecord = patterncore.SkillMemoryRecord
	skillInstall      = patterncore.SkillInstall
)
