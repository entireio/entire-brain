package retrieve

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestUnifiedRetrievalCommandDefaultsMatchDocumentedContract(t *testing.T) {
	for _, command := range []*cobra.Command{
		NewQuery(Config{}),
		NewSearch(Config{}),
		NewVsearch(Config{}),
	} {
		for _, flag := range []string{"limit", "number"} {
			if got := command.Flags().Lookup(flag).DefValue; got != "10" {
				t.Errorf("%s --%s default = %s, want 10", command.Name(), flag, got)
			}
		}
	}
}

func TestQMDOutputFormatAlias(t *testing.T) {
	got, err := outputWantsJSON(false, "json")
	if err != nil || !got {
		t.Fatalf("--format json should request JSON, got %t err=%v", got, err)
	}
	got, err = outputWantsJSON(true, "cli")
	if err != nil || got {
		t.Fatalf("--format cli should force CLI output, got %t err=%v", got, err)
	}
	if _, err := outputWantsJSON(false, "text"); err == nil {
		t.Fatal("--format text should be rejected; supported formats are json or cli")
	}
	if _, err := outputWantsJSON(false, "xml"); err == nil {
		t.Fatal("unknown --format should error")
	}
}

// Gate 2: the id-shape test itself. A bare id with no prefix resolves as a fact
// everywhere else in this codebase, so it counts here too.
func TestMissingFactIDShapes(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		ids  []string
		want bool
	}{
		"prefixed fact":      {[]string{"fact:abc"}, true},
		"bare id":            {[]string{"abc123"}, true},
		"conversation":       {[]string{"conversation:abc"}, false},
		"history":            {[]string{"history:abc"}, false},
		"raw has a colon":    {[]string{"raw:abc"}, false},
		"mixed, one fact":    {[]string{"conversation:a", "fact:b"}, true},
		"mixed, no fact":     {[]string{"conversation:a", "doc:b"}, false},
		"empty slice":        {nil, false},
		"blank entries only": {[]string{"", "   "}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := missingFactIDs(tc.ids); got != tc.want {
				t.Errorf("missingFactIDs(%v) = %v, want %v", tc.ids, got, tc.want)
			}
		})
	}
}
