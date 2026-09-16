package agentsetup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuidanceModeLifecycle(t *testing.T) {
	for _, first := range []string{"graph", "brain"} {
		t.Run(first, func(t *testing.T) {
			repo := t.TempDir()
			second := "brain"
			if first == "brain" {
				second = "graph"
			}
			install := func(product string, mode Mode) string {
				t.Helper()
				preview, err := Preview(repo, product, Options{Mode: mode})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := InstallChanged(repo, func() (string, error) {
					return Preview(repo, product, Options{Mode: mode})
				}); err != nil {
					t.Fatal(err)
				}
				if got := readFileForTest(t, filepath.Join(repo, Path)); got != preview {
					t.Fatal("installed guide differs from preview")
				}
				return preview
			}
			strict := install(first, ModeStrict)
			if !strings.Contains(strict, `"mode":"strict"`) {
				t.Fatal("strict mode not persisted")
			}
			if again := install(first, ""); again != strict {
				t.Fatal("default regeneration lost strict mode")
			}
			combined := install(second, "")
			for _, product := range []string{first, second} {
				got, err := Preview(repo, product, Options{})
				if err != nil || got != combined || !strings.Contains(got, `"mode":"strict"`) {
					t.Fatal("peer lost saved mode", err)
				}
			}
			// One-off overrides do not change the persisted mode or managed pointers.
			before := map[string]string{}
			for _, name := range []string{Path, "AGENTS.md", "CLAUDE.md"} {
				before[name] = readFileForTest(t, filepath.Join(repo, name))
			}
			normal, err := Preview(repo, second, Options{Mode: ModeNormal})
			if err != nil || strings.Contains(normal, `"mode":"strict"`) || !strings.Contains(normal, "Skip ceremonial queries") {
				t.Fatal("normal override failed", err)
			}
			for name, contents := range before {
				if readFileForTest(t, filepath.Join(repo, name)) != contents {
					t.Fatal("preview wrote", name)
				}
			}
			if got := install(second, ModeNormal); got != normal {
				t.Fatal("normal reset differs from preview")
			}
			if got := install(first, ""); got != normal {
				t.Fatal("normal reset did not persist across products")
			}
			if got := install(first, ModeStrict); got != combined {
				t.Fatal("strict reactivation differs")
			}
			changed, err := InstallChanged(repo, func() (string, error) { return Preview(repo, second, Options{}) })
			if err != nil || len(changed) != 0 {
				t.Fatal("strict regeneration is not idempotent", changed, err)
			}
		})
	}
}

func TestStrictGuidanceContentAndScope(t *testing.T) {
	for _, active := range []map[string]bool{{"graph": true}, {"brain": true}, {"graph": true, "brain": true}} {
		guide := guideFor(active, ModeStrict)
		for _, forbidden := range []string{"Skip ceremonial", "Skip this when equivalent", "Do not ask both tools"} {
			if strings.Contains(guide, forbidden) {
				t.Errorf("strict guide retains normal exception %q", forbidden)
			}
		}
		for _, required := range []string{"Attempt the required tool first", "Every fallback must trace", "VERIFY before stopping", "untrusted"} {
			if !strings.Contains(guide, required) {
				t.Errorf("strict guide missing %q", required)
			}
		}
		if strings.Contains(guide, "ALWAYS run impact before editing") != active["graph"] {
			t.Error("Graph obligation does not match activation")
		}
		if strings.Contains(guide, "ALWAYS begin a substantive task") != active["brain"] {
			t.Error("Brain obligation does not match activation")
		}
		if active["graph"] && active["brain"] && !strings.Contains(guide, "They NEVER replace required neighbors or impact analysis") {
			t.Error("combined guide lost mandatory coordination")
		}
	}
}

func TestGuidanceModePreviewOutsideAndNewRepository(t *testing.T) {
	for _, product := range []string{"graph", "brain"} {
		for _, mode := range []Mode{"", ModeNormal, ModeStrict} {
			guide, err := Preview("", product, Options{Mode: mode})
			if err != nil || guide != guideFor(map[string]bool{product: true}, mode) {
				t.Fatal("standalone preview", product, mode, err)
			}
			repo := t.TempDir()
			if _, err := Preview(repo, product, Options{Mode: mode}); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(repo)
			if err != nil || len(entries) != 0 {
				t.Fatal("preview created files", err)
			}
		}
	}
	if _, err := Preview("", "graph", Options{Mode: "invalid"}); err == nil {
		t.Fatal("invalid mode accepted")
	}
}

func TestStrictMigrationAndModeValidation(t *testing.T) {
	for _, original := range []string{GraphGuide, BrainGuide(), CombinedGuide, renderActivation(map[string]bool{"graph": true}, ModeNormal)} {
		repo := t.TempDir()
		mkdirAllForTest(t, filepath.Join(repo, ".entire"))
		writeFileForTest(t, filepath.Join(repo, Path), original)
		if _, err := InstallChanged(repo, func() (string, error) { return Preview(repo, "graph", Options{Mode: ModeStrict}) }); err != nil {
			t.Fatal("strict migration", err)
		}
		got, err := Preview(repo, "graph", Options{})
		if err != nil || !strings.Contains(got, `"mode":"strict"`) {
			t.Fatal("migration lost strict mode", err)
		}
	}
	for _, value := range []string{`"invalid"`, `true`, `42`, `{}`} {
		repo := t.TempDir()
		mkdirAllForTest(t, filepath.Join(repo, ".entire"))
		original := activationPrefix + `{"schema_version":1,"enabled":["graph"],"mode":` + value + `}` + activationSuffix
		writeFileForTest(t, filepath.Join(repo, Path), original)
		for _, mode := range []Mode{"", ModeNormal, ModeStrict} {
			if _, err := InstallChanged(repo, func() (string, error) { return Preview(repo, "graph", Options{Mode: mode}) }); err == nil {
				t.Fatal("accepted malformed mode", value)
			}
			if readFileForTest(t, filepath.Join(repo, Path)) != original {
				t.Fatal("overwrote invalid metadata")
			}
			if _, err := os.Stat(filepath.Join(repo, "AGENTS.md")); !os.IsNotExist(err) {
				t.Fatal("partial write on invalid mode")
			}
		}
	}
}
