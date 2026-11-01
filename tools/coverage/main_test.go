package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMergeProfilesUnionsSharedBlocks(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.out")
	b := filepath.Join(dir, "b.out")
	out := filepath.Join(dir, "merged.out")
	for path, data := range map[string]string{a: "mode: atomic\np/a.go:1.1,2.2 3 0\np/b.go:1.1,2.2 1 1\n", b: "mode: atomic\np/a.go:1.1,2.2 3 2\n"} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := mergeProfiles([]string{a, b}, out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "mode: set\np/a.go:1.1,2.2 3 1\np/b.go:1.1,2.2 1 1\n" {
		t.Fatalf("profile=%s", data)
	}
	if err := os.WriteFile(b, []byte("mode: atomic\np/a.go:1.1,2.2 4 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := mergeProfiles([]string{a, b}, out); err == nil {
		t.Fatal("accepted inconsistent statement counts")
	}
}

func TestMergeProfilesRejectsInvalidEvidence(t *testing.T) {
	for _, data := range []string{"", "mode: atomic\n", "garbage", "mode: set\na.go:1.1,2.2 1 -1\n"} {
		path := filepath.Join(t.TempDir(), "profile")
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if err := mergeProfiles([]string{path}, filepath.Join(t.TempDir(), "out")); err == nil {
			t.Fatalf("accepted %q", data)
		}
	}
}

func TestFailureLogPreservesDiagnostics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tests.jsonl")
	if err := os.WriteFile(path, []byte("{\"Action\":\"output\",\"Output\":\"panic: failure\\n\"}\ncompiler diagnostic\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := printFailureLog(path, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "panic: failure\ncompiler diagnostic\n" {
		t.Fatal(out.String())
	}
}

func TestRunRejectsStaleOutput(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stale"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := runCoverage(t.TempDir(), options{output: dir}); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("error=%v", err)
	}
}

func TestCoveragePreservesTestFailuresAndSupportsBinaryOnly(t *testing.T) {
	t.Setenv("GOWORK", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	root := t.TempDir()
	dir := filepath.Join(root, "cmd", "entire-brain")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"go.mod": "module example.com/coveragefixture\n\ngo 1.27\n",
		"cmd/entire-brain/main.go": `package main
import ("fmt"; "os")
func main() {
 switch os.Args[1] {
 case "--help": fmt.Println("Usage: fixture")
 case "version": fmt.Println("dev")
 case "capabilities": fmt.Println(` + "`" + `{"schema_version":1,"build":{"brain_cgo":false}}` + "`" + `)
 default: fmt.Fprintln(os.Stderr,"unknown command"); os.Exit(1)
 }
}
`,
		"cmd/entire-brain/main_test.go": "package main\nimport \"testing\"\nfunc TestFailure(t *testing.T) { t.Fatal(\"intentional suite failure\") }\n",
	}
	for path, data := range files {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(path)), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, binaryOnly := range []bool{true, false} {
		output := filepath.Join(t.TempDir(), "coverage")
		err := runCoverage(root, options{output: output, binaryOnly: binaryOnly, timeout: "30s"})
		if (err == nil) != binaryOnly {
			t.Fatalf("binaryOnly=%v: %v", binaryOnly, err)
		}
		if _, err := os.Stat(filepath.Join(output, "binary.out")); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(output, "summary.json"))
		var report coverageReport
		if err != nil || json.Unmarshal(data, &report) != nil || report.Environment.TestsPassed != binaryOnly || report.ProfileSHA256 == "" {
			t.Fatalf("invalid coverage report: %s: %v", data, err)
		}
		data, err = os.ReadFile(filepath.Join(output, "test-results.json"))
		var results testSummary
		if err != nil || json.Unmarshal(data, &results) != nil || results.RunPassed != binaryOnly || (!binaryOnly && results.Tests.Failed != 1) {
			t.Fatalf("invalid test summary: %s: %v", data, err)
		}
		if !binaryOnly {
			log, err := os.ReadFile(filepath.Join(output, "tests.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(log, []byte("intentional suite failure")) {
				t.Fatal("failure diagnostics were lost")
			}
		}
	}
}

func TestSplitTestsRetainsBinaryAndFailureEvidence(t *testing.T) {
	t.Setenv("GOWORK", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "internal", "cli"), 0700); err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string]string{
		"go.mod":                   "module example.com/splitfixture\n\ngo 1.27\n",
		"root.go":                  "package fixture\nfunc Value() int { return 1 }\n",
		"internal/cli/cli.go":      "package cli\nfunc Value() int { return 2 }\n",
		"internal/cli/cli_test.go": "package cli\nimport \"testing\"\nfunc TestFailure(t *testing.T) { _ = Value(); t.Fatal(\"split failure\") }\n",
	} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(path)), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	out := t.TempDir()
	var log bytes.Buffer
	if err := splitTests(root, out, "30s", nil, &log, 0, 1); err == nil {
		t.Fatal("lost CLI test failure")
	}
	if !strings.Contains(log.String(), "split failure") || !strings.Contains(log.String(), `"Action":"fail"`) {
		t.Fatalf("missing failure evidence: %s", log.String())
	}
	for _, name := range []string{"tests.out", "cli.out", "cli.test.exe"} {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Fatal(err)
		}
	}
}
