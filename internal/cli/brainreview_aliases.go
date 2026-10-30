package cli

import "github.com/entireio/entire-brain/internal/cli/command/brainreview"

// The diff-less review contract and the regression detector's result types
// live in internal/cli/command/brainreview (COR-2268). These aliases keep the
// package-local spellings compiling for the regression engine and the
// workspace surfaces that remain here; they retire with the cli package.
type (
	regressionAnomaly         = brainreview.Anomaly
	regressionScanCaps        = brainreview.ScanCaps
	regressionDetectorOptions = brainreview.DetectorOptions
	reviewFinding             = brainreview.Finding
	reviewReport              = brainreview.Report
	reviewPatternRef          = brainreview.PatternRef
)
