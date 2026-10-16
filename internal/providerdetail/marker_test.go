package providerdetail

import "testing"

// TestMissingDetectsTheNewerDispatcherWording guards a marker that main added
// while this package was being extracted. Folding two implementations into one
// is exactly when a marker gets silently dropped.
func TestMissingDetectsTheNewerDispatcherWording(t *testing.T) {
	t.Parallel()
	if !Missing("graph: plugin is not installed") {
		t.Fatal("a host CLI that names the absent plugin must still be detected as missing")
	}
	if Missing("some unrelated failure") {
		t.Fatal("non-vacuity: an unrelated error must NOT be read as a missing provider")
	}
}
