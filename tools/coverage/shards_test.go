package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestGroupsCoverTestsExamplesAndFuzzSeedsExactlyOnce(t *testing.T) {
	inventory := "TestZulu\nBenchmarkIgnored\nTestAlpha\nExampleUsage\nFuzzDecode\ncoverage: 0.0% of statements\n"
	seen := map[string]bool{}
	for index := range 3 {
		all, selected, err := selectRoots(inventory, index, 3)
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 4 {
			t.Fatal(all)
		}
		for _, name := range selected {
			if seen[name] {
				t.Fatalf("duplicate assignment: %s", name)
			}
			seen[name] = true
		}
	}
	if len(seen) != 4 {
		t.Fatal(seen)
	}
	for _, input := range []string{"", "TestA\nTestA", "unexpected output", "TestA/child"} {
		if _, _, err := selectRoots(input, 0, 1); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
	if _, _, err := selectRoots("TestA", 1, 2); err == nil {
		t.Fatal("accepted an empty group")
	}
}

func TestRootOutcomesRejectMissingDuplicateUnexpectedAndFailedTests(t *testing.T) {
	for _, tc := range []struct {
		events string
		fail   bool
	}{
		{`{"Action":"pass","Test":"TestA"}`, false},
		{`{"Action":"skip","Test":"TestA"}`, false},
		{`{"Action":"pass"}`, true},
		{`{"Action":"fail","Test":"TestA"}`, true},
		{`{"Action":"pass","Test":"TestB"}`, true},
		{`{"Action":"pass","Test":"TestA/subtest"}`, true},
		{"{\"Action\":\"pass\",\"Test\":\"TestA\"}\n{\"Action\":\"pass\",\"Test\":\"TestA\"}", true},
		{"truncated json", true},
	} {
		path := filepath.Join(t.TempDir(), "events")
		if err := os.WriteFile(path, []byte(tc.events), 0600); err != nil {
			t.Fatal(err)
		}
		if err := verifyRootOutcomes(path, []string{"TestA"}); (err != nil) != tc.fail {
			t.Fatalf("events=%s err=%v", tc.events, err)
		}
	}
}

func TestMergeRequiresEveryGroupAndConsistentInventory(t *testing.T) {
	t.Setenv("GOWORK", "off")
	root := t.TempDir()
	for path, contents := range map[string]string{
		"go.mod":   "module example.com/mergefixture\n\ngo 1.27\n",
		"value.go": "package fixture\nfunc Value() int { return 1 }\n",
	} {
		if err := os.WriteFile(filepath.Join(root, path), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, scenario := range []string{"valid", "missing group", "different inventory", "missing profile", "missing outcome", "different commit", "different toolchain", "failed group", "missing metadata"} {
		t.Run(scenario, func(t *testing.T) {
			input, output := t.TempDir(), t.TempDir()
			for i := range 2 {
				if i == 1 && scenario == "missing group" {
					continue
				}
				dir := filepath.Join(input, fmt.Sprintf("group-%d", i))
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				all := []string{"TestA", "TestB"}
				if i == 1 && scenario == "different inventory" {
					all[1] = "TestC"
				}
				_, selected, err := selectRoots(strings.Join(all, "\n"), i, 2)
				if err != nil {
					t.Fatal(err)
				}
				if err := writeJSON(filepath.Join(dir, "shard-plan.json"), map[string]any{"index": i, "count": 2, "all": all, "selected": selected}); err != nil {
					t.Fatal(err)
				}
				env := environment{Commit: "same", GoVersion: "go version fixture", Platform: "windows/amd64", ShardCount: 2, ShardIndex: i, TestsPassed: true}
				if i == 1 {
					switch scenario {
					case "different commit":
						env.Commit = "other"
					case "different toolchain":
						env.GoVersion = "other"
					case "failed group":
						env.TestsPassed = false
					}
				}
				if i != 1 || scenario != "missing metadata" {
					if err := writeJSON(filepath.Join(dir, "environment.json"), env); err != nil {
						t.Fatal(err)
					}
				}
				event := fmt.Sprintf("{\"Action\":\"pass\",\"Test\":%q}\n", selected[0])
				if i == 1 && scenario == "missing outcome" {
					event = ""
				}
				if err := os.WriteFile(filepath.Join(dir, "cli.jsonl"), []byte(event), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "tests.jsonl"), []byte(event), 0600); err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"cli.out", "binary.out", "tests.out"} {
					if i == 1 && name == "cli.out" && scenario == "missing profile" {
						continue
					}
					if err := os.WriteFile(filepath.Join(dir, name), []byte("mode: atomic\nexample.com/mergefixture/value.go:2.18,2.26 1 1\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			err := mergeArtifacts(root, input, output)
			if (err == nil) != (scenario == "valid") {
				t.Fatalf("%s: %v", scenario, err)
			}
			if err == nil {
				data, err := os.ReadFile(filepath.Join(output, "environment.json"))
				if err != nil || !strings.Contains(string(data), "windows/amd64") {
					t.Fatalf("lost target platform: %s: %v", data, err)
				}
				data, err = os.ReadFile(filepath.Join(output, "combined.out"))
				if err != nil || !slices.Equal(strings.Split(strings.TrimSpace(string(data)), "\n"), []string{"mode: set", "example.com/mergefixture/value.go:2.18,2.26 1 1"}) {
					t.Fatalf("merged profile: %s: %v", data, err)
				}
			}
		})
	}
}

func TestSplitGroupsExecuteExamplesAndFuzzSeeds(t *testing.T) {
	t.Setenv("GOWORK", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "internal", "cli"), 0700); err != nil {
		t.Fatal(err)
	}
	for path, contents := range map[string]string{
		"go.mod":                "module example.com/groupsfixture\n\ngo 1.27\n",
		"root.go":               "package fixture\nfunc Value() int { return 1 }\n",
		"internal/cli/value.go": "package cli\nfunc Value() int { return 2 }\n",
		"internal/cli/value_test.go": `package cli
import ("fmt"; "testing")
func TestValue(t *testing.T) { if Value() != 2 { t.Fatal("value") } }
func ExampleValue() { fmt.Println(Value()); // Output: 2
}
func FuzzValue(f *testing.F) { f.Add("seed"); f.Fuzz(func(t *testing.T, s string) { _ = Value() }) }
func BenchmarkNeverRun(b *testing.B) { panic("benchmark ran in a test group") }
`,
	} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(path)), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 2 {
		out := t.TempDir()
		var log strings.Builder
		if err := splitTests(root, out, "30s", nil, &log, i, 2); err != nil {
			t.Fatalf("group %d: %v: %s", i, err, log.String())
		}
		_, err := os.Stat(filepath.Join(out, "tests.out"))
		if i == 0 && err != nil {
			t.Fatal(err)
		}
		if i == 1 && !os.IsNotExist(err) {
			t.Fatalf("non-CLI packages repeated in group 1: %v", err)
		}
	}
}
