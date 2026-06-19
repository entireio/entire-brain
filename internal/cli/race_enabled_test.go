//go:build race

package cli

// raceEnabled reports whether the test binary was built with the race detector.
// Heap-bounds tests are skipped under -race: the detector instruments every
// allocation, which both skews heap measurements and makes large synthetic
// streams slow enough to push the package toward the CI timeout.
const raceEnabled = true
