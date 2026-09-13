package agentsetup

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixtureOptions(t *testing.T, plugins ...string) Options {
	t.Helper()
	return Options{StateDir: t.TempDir(), ConfigDir: t.TempDir(), DataDir: t.TempDir(), ListPlugins: func() (string, error) {
		if len(plugins) == 0 {
			return "No plugins installed in /fixture.\nInstall one with 'entire plugin install <name|url|path>', or drop an entire-<name> binary anywhere on $PATH.\n", nil
		}
		text := "Managed plugin directory: /fixture\n\n"
		for _, plugin := range plugins {
			text += "  " + plugin + " v1.0.0 → /fixture/entire-" + plugin + "\n"
		}
		return text, nil
	}}
}
func fixtureSetup(t *testing.T, repo string, opts Options, content string) string {
	t.Helper()
	canonical, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(opts.StateDir, "repos", filepath.FromSlash(localKey(canonical)), "setup.json")
	mkdirAllForTest(t, filepath.Dir(path))
	writeFileForTest(t, path, content)
	return path
}
func TestCoordinationModes(t *testing.T) {
	for _, product := range []string{"graph", "brain"} {
		for _, installed := range []bool{false, true} {
			for _, configured := range []bool{false, true} {
				t.Run(product+"/installed="+boolText(installed)+"/configured="+boolText(configured), func(t *testing.T) {
					repo := t.TempDir()
					var plugins []string
					if installed {
						plugins = []string{"graph", "brain"}
					}
					opts := fixtureOptions(t, plugins...)
					if configured {
						fixtureSetup(t, repo, opts, `{"schema_version":1,"updated_at":"2000-01-01T00:00:00Z"}`)
					}
					guide, err := Preview(repo, product, opts)
					if err != nil {
						t.Fatal(err)
					}
					want := GraphGuide
					if product == "brain" {
						want = BrainGuide()
					}
					if installed && (product == "brain" || configured) {
						want = CombinedGuide
					}
					if guide != want {
						t.Fatalf("unexpected mode:\n%s", guide)
					}
				})
			}
		}
	}
}
func boolText(v bool) string {
	if v {
		return "true"
	}
	return "false"
}
func TestCoordinationActivationOrdersAndStableMigration(t *testing.T) {
	for _, first := range []string{"graph", "brain"} {
		t.Run(first, func(t *testing.T) {
			repo := t.TempDir()
			opts := fixtureOptions(t, "brain", "graph")
			setup := fixtureSetup(t, repo, opts, `{"schema_version":1}`)
			original := "USER PREFIX\n<!-- entire-graph:begin -->\nold Graph\n<!-- entire-graph:end -->\nMIDDLE\n<!-- entire-brain:begin -->\nold Brain\n<!-- entire-brain:end -->\nUSER SUFFIX\n"
			writeFileForTest(t, filepath.Join(repo, "AGENTS.md"), original)
			writeFileForTest(t, filepath.Join(repo, "CLAUDE.md"), "@AGENTS.md\n")
			for _, legacy := range []string{"graph-agent.md", "brain-agent.md"} {
				mkdirAllForTest(t, filepath.Join(repo, ".entire"))
				writeFileForTest(t, filepath.Join(repo, ".entire", legacy), "Your FIRST action must be old retrieval\n")
			}
			second := "brain"
			if first == "brain" {
				second = "graph"
			}
			var before string
			for _, product := range []string{first, second, first, second} {
				render := func() (string, error) { return Preview(repo, product, opts) }
				if err := Install(repo, render, io.Discard); err != nil {
					t.Fatal(err)
				}
				displayed, err := render()
				if err != nil {
					t.Fatal(err)
				}
				if got := readFileForTest(t, filepath.Join(repo, Path)); got != displayed || got != CombinedGuide {
					t.Fatal("installed/displayed guides differ")
				}
				text := readFileForTest(t, filepath.Join(repo, "AGENTS.md"))
				for _, want := range []string{"USER PREFIX\n", "\nMIDDLE\n", "\nUSER SUFFIX\n"} {
					if !strings.Contains(text, want) {
						t.Fatalf("lost user text %q", want)
					}
				}
				if strings.Count(text, agentPointerBegin) != 1 || strings.Contains(text, "entire-graph:begin") || strings.Contains(text, "entire-brain:begin") {
					t.Fatal("duplicate managed instructions")
				}
				if before != "" && before != text {
					t.Fatal("repeat generation changed bytes")
				}
				before = text
				for _, legacy := range []string{"graph-agent.md", "brain-agent.md"} {
					if got := readFileForTest(t, filepath.Join(repo, ".entire", legacy)); got != legacyRedirect {
						t.Fatal("legacy instructions still active")
					}
				}
			}
			os.Remove(setup)
			if err := Install(repo, func() (string, error) { return Preview(repo, "graph", opts) }, io.Discard); err != nil {
				t.Fatal(err)
			}
			if got := readFileForTest(t, filepath.Join(repo, Path)); got != GraphGuide {
				t.Fatal("configuration removal was not reconciled")
			}
		})
	}
}
func TestCoordinationErrorsBeforeWrites(t *testing.T) {
	for _, state := range []string{"malformed", "null", "wrong-field-type", "unreadable", "directory", "symlink", "future-schema"} {
		t.Run(state, func(t *testing.T) {
			repo := t.TempDir()
			opts := fixtureOptions(t, "brain")
			contents := `{"schema_version":1}`
			switch state {
			case "malformed":
				contents = "{"
			case "null":
				contents = "null"
			case "wrong-field-type":
				contents = `{"schema_version":1,"workspace":4}`
			case "future-schema":
				contents = `{"schema_version":2}`
			}
			path := fixtureSetup(t, repo, opts, contents)
			switch state {
			case "unreadable":
				os.Chmod(path, 0000)
				defer os.Chmod(path, 0600)
			case "directory":
				os.Remove(path)
				os.Mkdir(path, 0700)
			case "symlink":
				os.Remove(path)
				symlinkForTest(t, "missing", path)
			}
			if err := Install(repo, func() (string, error) { return Preview(repo, "graph", opts) }, io.Discard); err == nil {
				t.Fatal("invalid setup state accepted")
			}
			if _, err := os.Stat(filepath.Join(repo, Path)); !os.IsNotExist(err) {
				t.Fatal("partial guide written")
			}
		})
	}
	repo := t.TempDir()
	opts := fixtureOptions(t)
	opts.ListPlugins = func() (string, error) { return "", errors.New("fixture failure") }
	if err := Install(repo, func() (string, error) { return Preview(repo, "brain", opts) }, io.Discard); err == nil {
		t.Fatal("listing error ignored")
	}
}
func TestCoordinationBrainManifestWithoutSetup(t *testing.T) {
	repo := t.TempDir()
	opts := fixtureOptions(t, "brain")
	canonical, _ := filepath.EvalSymlinks(repo)
	key := localKey(canonical)
	path := filepath.Join(opts.DataDir, "repos", filepath.FromSlash(key), "manifest.json")
	mkdirAllForTest(t, filepath.Dir(path))
	writeFileForTest(t, path, `{"schema_version":3,"repo_key":"`+key+`"}`)
	guide, err := Preview(repo, "graph", opts)
	if err != nil || guide != CombinedGuide {
		t.Fatal("manual Brain not recognized", err)
	}
	// No index exists: index freshness must not affect setup mode.
	os.Remove(path)
	guide, err = Preview(repo, "graph", opts)
	if err != nil || guide != GraphGuide {
		t.Fatal("removed Brain still recognized", err)
	}
}
func TestCoordinationNoRuntimeProbes(t *testing.T) {
	for _, guide := range []string{GraphGuide, BrainGuide(), CombinedGuide} {
		for _, bad := range []string{"entire plugin list", "entire graph version", "entire brain version", "command -v", "setup.json", "if Brain is installed", "FIRST action", "SEARCH FIRST"} {
			if strings.Contains(guide, bad) {
				t.Errorf("guide contains %q", bad)
			}
		}
		for _, want := range []string{"sufficient locations", "focused tests", "untrusted", "Do not automatically install"} {
			if !strings.Contains(guide, want) {
				t.Errorf("guide missing %q", want)
			}
		}
	}
	for _, want := range []string{`entire brain brief "<task>" --json`, "equivalent task context", "redundant", "identified gap", "entities history", "memory-informed review", "workspace", "working tree", "stored index"} {
		if !strings.Contains(CombinedGuide, want) {
			t.Errorf("combined guide missing %q", want)
		}
	}
}
func TestCoordinationPluginListFormat(t *testing.T) {
	for _, raw := range []string{"", "[]", "Managed plugin directory: /fixture\n", "Managed plugin directory: /fixture\nbrain", "Managed plugin directory: /fixture\n  brain /a\n  brain /b\n"} {
		if _, err := parsePluginList(raw); err == nil {
			t.Errorf("accepted malformed listing %q", raw)
		}
	}
	raw := "Managed plugin directory: /fixture\n\n  brain                                    → /path with spaces/brain\n  graph                v0.4.0 (pinned)     /fixture/graph\n"
	got, err := parsePluginList(raw)
	if err != nil || !got["brain"] || !got["graph"] {
		t.Fatal(got, err)
	}
}
func TestCoordinationOutsideRepository(t *testing.T) {
	t.Chdir(t.TempDir())
	root, err := Context("", "")
	if err != nil || root != "" {
		t.Fatal(root, err)
	}
	opts := Options{ListPlugins: func() (string, error) { t.Fatal("outside-repo preview queried plugins"); return "", nil }}
	for _, product := range []string{"graph", "brain"} {
		if _, err := Preview(root, product, opts); err != nil {
			t.Fatal(err)
		}
	}
}
func TestCoordinationLegacyMarkerValidation(t *testing.T) {
	for _, product := range []string{"graph", "brain"} {
		repo := t.TempDir()
		writeFileForTest(t, filepath.Join(repo, "CLAUDE.md"), "<!-- entire-"+product+":begin -->\n")
		if err := Install(repo, func() (string, error) { return CombinedGuide, nil }, io.Discard); err == nil {
			t.Fatal("malformed legacy markers accepted")
		}
		if _, err := os.Stat(filepath.Join(repo, Path)); !os.IsNotExist(err) {
			t.Fatal("partial install")
		}
	}
}

func TestCoordinationLegacyAliasesAndProtectedTargets(t *testing.T) {
	t.Run("non-markdown generated legacy landing", func(t *testing.T) {
		repo := t.TempDir()
		mkdirAllForTest(t, filepath.Join(repo, ".entire"))
		writeFileForTest(t, filepath.Join(repo, "GUIDE"), "# entire-graph — instructions for coding agents (follow directly)\nold workflow\n")
		symlinkForTest(t, "../GUIDE", filepath.Join(repo, ".entire/graph-agent.md"))
		if err := Install(repo, func() (string, error) { return CombinedGuide, nil }, io.Discard); err != nil {
			t.Fatal(err)
		}
		if got := readFileForTest(t, filepath.Join(repo, "GUIDE")); got != legacyRedirect {
			t.Fatal("legacy alias not migrated")
		}
	})
	for _, kind := range []string{"outside", "git", "instruction-alias"} {
		t.Run(kind, func(t *testing.T) {
			repo := t.TempDir()
			mkdirAllForTest(t, filepath.Join(repo, ".entire"))
			var target string
			switch kind {
			case "outside":
				target = filepath.Join(t.TempDir(), "rules.md")
			case "git":
				target = filepath.Join(repo, ".git", "config")
				mkdirAllForTest(t, filepath.Dir(target))
			case "instruction-alias":
				target = filepath.Join(repo, "AGENTS.md")
			}
			writeFileForTest(t, target, "PRESERVE")
			symlinkForTest(t, target, filepath.Join(repo, ".entire/brain-agent.md"))
			if err := Install(repo, func() (string, error) { return CombinedGuide, nil }, io.Discard); err == nil {
				t.Fatal("accepted protected legacy target")
			}
			if got := readFileForTest(t, target); got != "PRESERVE" {
				t.Fatal("modified protected target")
			}
			if _, err := os.Stat(filepath.Join(repo, Path)); !os.IsNotExist(err) {
				t.Fatal("partial shared guide")
			}
		})
	}
}

func TestCoordinationLegacyRootIdentity(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	mkdirAllForTest(t, filepath.Join(real, "repo"))
	ancestor := filepath.Join(base, "ancestor")
	symlinkForTest(t, real, ancestor)
	lexical := filepath.Join(ancestor, "repo")
	root := filepath.Join(base, "root")
	symlinkForTest(t, lexical, root)
	opts := fixtureOptions(t, "brain")
	record := filepath.Join(opts.StateDir, "repos", filepath.FromSlash(localKey(lexical)), "setup.json")
	mkdirAllForTest(t, filepath.Dir(record))
	writeFileForTest(t, record, `{"schema_version":1}`)
	guide, err := Preview(root, "graph", opts)
	if err != nil || guide != CombinedGuide {
		t.Fatal("legacy root setup was missed", err)
	}
}

func TestInstallChangedReport(t *testing.T) {
	repo := t.TempDir()
	render := func() (string, error) { return "guide\n", nil }
	changed, err := InstallChanged(repo, render)
	if err != nil || len(changed) != 3 {
		t.Fatalf("first: %v, %v", changed, err)
	}
	changed, err = InstallChanged(repo, render)
	if err != nil || len(changed) != 0 {
		t.Fatalf("repeat: %v, %v", changed, err)
	}
}

// A directly invoked binary must generate its own instructions without a host.
func TestCoordinationWithoutEntireHost(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, product := range []string{"graph", "brain"} {
		t.Run(product, func(t *testing.T) {
			repo := t.TempDir()
			opts := Options{StateDir: t.TempDir(), ConfigDir: t.TempDir(), DataDir: t.TempDir()}
			want := GraphGuide
			if product == "brain" {
				want = BrainGuide()
			}
			render := func() (string, error) { return Preview(repo, product, opts) }
			got, err := render()
			if err != nil || got != want {
				t.Fatalf("standalone preview: %v", err)
			}
			if err := Install(repo, render, io.Discard); err != nil {
				t.Fatal(err)
			}
			if got := readFileForTest(t, filepath.Join(repo, Path)); got != want {
				t.Fatal("installed guide differs from preview")
			}
			changed, err := InstallChanged(repo, render)
			if err != nil || len(changed) != 0 {
				t.Fatalf("repeat generation: %v, %v", changed, err)
			}
		})
	}
}
