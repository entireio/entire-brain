package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOutcomeRequiresTheIntendedAssertion(t *testing.T) {
	c := mutationCase{Test: "TestTarget", Diagnostic: "expected invariant"}
	for _, tc := range []struct {
		name, output   string
		exit           int
		mutant, accept bool
	}{
		{"baseline passes", `{"Action":"pass","Test":"TestTarget"}`, 0, false, true},
		{"baseline skipped", `{"Action":"skip","Test":"TestTarget"}`, 0, false, false},
		{"no selected test", `{"Action":"pass","Test":"TestOther"}`, 0, false, false},
		{"baseline process fails", `{"Action":"pass","Test":"TestTarget"}`, 1, false, false},
		{"intended assertion", "{\"Action\":\"output\",\"Test\":\"TestTarget\",\"Output\":\"expected invariant\"}\n{\"Action\":\"fail\",\"Test\":\"TestTarget\"}\n", 1, true, true},
		{"subtest assertion", "{\"Action\":\"output\",\"Test\":\"TestTarget/case\",\"Output\":\"expected invariant\"}\n{\"Action\":\"fail\",\"Test\":\"TestTarget\"}\n", 1, true, true},
		{"wrong assertion", `{"Action":"fail","Test":"TestTarget","Output":"unrelated failure"}`, 1, true, false},
		{"compiler failure", `{"Action":"build-fail","Output":"expected invariant"}`, 1, true, false},
		{"raw compiler output", "expected invariant\nFAIL [build failed]", 1, true, false},
		{"different test output", "{\"Action\":\"output\",\"Test\":\"TestOther\",\"Output\":\"expected invariant\"}\n{\"Action\":\"fail\",\"Test\":\"TestTarget\"}\n", 1, true, false},
		{"mutant survives", `{"Action":"pass","Test":"TestTarget"}`, 0, true, false},
		{"failure but zero exit", `{"Action":"fail","Test":"TestTarget","Output":"expected invariant"}`, 0, true, false},
		{"test timeout", `{"Action":"fail","Test":"TestTarget","Output":"expected invariant; panic: test timed out"}`, 1, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateOutcome(processResult{Output: []byte(tc.output), ExitCode: tc.exit}, c, tc.mutant)
			if (err == nil) != tc.accept {
				t.Fatalf("accept=%v, error=%v", tc.accept, err)
			}
		})
	}
	if err := validateOutcome(processResult{Err: context.DeadlineExceeded}, c, true); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func mutationFixture(t *testing.T) (string, mutationCase) {
	t.Helper()
	t.Setenv("GOWORK", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	root := t.TempDir()
	for path, contents := range map[string]string{
		"go.mod":        "module example.com/mutationfixture\n\ngo 1.27\n",
		"value.go":      "package fixture\nfunc Value() int { return 1 }\n",
		"value_test.go": "package fixture\nimport \"testing\"\nfunc TestValue(t *testing.T) { if Value() != 1 { t.Fatalf(\"value invariant: %d\", Value()) } }\n",
	} {
		if err := os.WriteFile(filepath.Join(root, path), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return root, mutationCase{Name: "value invariant", File: "value.go", Before: "return 1", After: "return 2", Package: ".", Test: "TestValue", Diagnostic: "value invariant"}
}

func TestRealOverlayDetectsRegressionWithoutChangingSource(t *testing.T) {
	root, c := mutationFixture(t)
	original, err := os.ReadFile(filepath.Join(root, c.File))
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	var progress bytes.Buffer
	if err := runCases(root, out, []mutationCase{c}, &progress); err != nil {
		t.Fatalf("%v: %s", err, progress.String())
	}
	current, err := os.ReadFile(filepath.Join(root, c.File))
	if err != nil || !bytes.Equal(current, original) {
		t.Fatalf("source changed: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(out, "results.json"))
	if err != nil {
		t.Fatal(err)
	}
	var results []caseResult
	if err := json.Unmarshal(data, &results); err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].BaselinePassed || !results[0].MutationDetected || results[0].Error != "" {
		t.Fatalf("results=%s", data)
	}
	for _, name := range []string{"01-baseline.jsonl", "01-mutant.jsonl"} {
		data, err := os.ReadFile(filepath.Join(out, name))
		if err != nil || !bytes.Contains(data, []byte("TestValue")) {
			t.Fatalf("missing test evidence %s: %v", name, err)
		}
	}
	if err := runCases(root, out, []mutationCase{c}, &progress); err == nil {
		t.Fatal("accepted stale results")
	}
}

func TestRealOverlayRejectsCompileErrorsAndSurvivingMutations(t *testing.T) {
	for _, replacement := range []string{"return missingIdentifier", "return 1 + 0"} {
		t.Run(replacement, func(t *testing.T) {
			root, c := mutationFixture(t)
			c.After = replacement
			out := t.TempDir()
			var progress bytes.Buffer
			if err := runCases(root, out, []mutationCase{c}, &progress); err == nil {
				t.Fatalf("accepted %q", replacement)
			}
			data, err := os.ReadFile(filepath.Join(root, c.File))
			if err != nil || strings.Contains(string(data), replacement) {
				t.Fatalf("changed source: %s: %v", data, err)
			}
		})
	}
}

func TestRejectsStaleAmbiguousOrInvalidMutationsBeforeRunningTests(t *testing.T) {
	root, c := mutationFixture(t)
	for _, before := range []string{"", "absent", " "} {
		c.Before = before
		result := checkCase(root, t.TempDir(), 0, c)
		if result.Error == "" || result.BaselinePassed {
			t.Fatalf("accepted %q: %+v", before, result)
		}
	}
	c.File = "../outside.go"
	if result := checkCase(root, t.TempDir(), 0, c); result.Error == "" {
		t.Fatal("accepted outside source")
	}
}

func TestMissingBaselineStopsBeforeMutation(t *testing.T) {
	root, c := mutationFixture(t)
	c.Test = "TestDoesNotExist"
	out := t.TempDir()
	var progress bytes.Buffer
	if err := runCases(root, out, []mutationCase{c}, &progress); err == nil {
		t.Fatal("accepted a missing baseline test")
	}
	if _, err := os.Stat(filepath.Join(out, "01-mutant.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("ran mutation without a passing baseline: %v", err)
	}
}
