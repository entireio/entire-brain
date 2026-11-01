// Command mutationcheck proves that regression tests reject four deliberate bugs.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type mutationCase struct {
	Name, File, Before, After, Package, Test, Diagnostic string
}

type processResult struct {
	Output   []byte
	ExitCode int
	Err      error
}

type caseResult struct {
	Name             string `json:"name"`
	Test             string `json:"test"`
	BaselinePassed   bool   `json:"baseline_passed"`
	MutationDetected bool   `json:"mutation_detected"`
	Error            string `json:"error,omitempty"`
}

func main() {
	output := flag.String("output", "", "empty directory for logs and results (default: a new temporary directory)")
	flag.Parse()
	root, err := os.Getwd()
	if err == nil && *output == "" {
		*output, err = os.MkdirTemp("", "brain-mutation-results-")
	}
	if err == nil {
		fmt.Println("Mutation results:", *output)
		err = runCases(root, *output, regressionCases, os.Stdout)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runCases(root, output string, cases []mutationCase, progress io.Writer) error {
	if len(cases) == 0 {
		return errors.New("no mutation cases")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	output, err = filepath.Abs(output)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(output, 0755); err != nil {
		return err
	}
	entries, err := os.ReadDir(output)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("output directory must be empty")
	}
	results := make([]caseResult, 0, len(cases))
	var failures []error
	for i, c := range cases {
		result := checkCase(root, output, i, c)
		results = append(results, result)
		if result.Error != "" {
			fmt.Fprintf(progress, "FAIL: %s: %s\n", c.Name, result.Error)
			failures = append(failures, fmt.Errorf("%s: %s", c.Name, result.Error))
		} else {
			fmt.Fprintf(progress, "PASS: %s: baseline passes; intended assertion detects mutation\n", c.Name)
		}
	}
	data, err := json.MarshalIndent(results, "", "  ")
	if err == nil {
		err = os.WriteFile(filepath.Join(output, "results.json"), append(data, '\n'), 0644)
	}
	return errors.Join(append(failures, err)...)
}

func checkCase(root, output string, index int, c mutationCase) (result caseResult) {
	result.Name, result.Test = c.Name, c.Test
	fail := func(err error) caseResult { result.Error = err.Error(); return result }
	if !filepath.IsLocal(c.File) || c.Before == "" || c.Before == c.After || c.Test == "" || c.Diagnostic == "" {
		return fail(errors.New("invalid mutation definition"))
	}
	source := filepath.Join(root, filepath.FromSlash(c.File))
	original, err := os.ReadFile(source)
	if err != nil {
		return fail(err)
	}
	if bytes.Count(original, []byte(c.Before)) != 1 {
		return fail(errors.New("mutation anchor is stale or ambiguous"))
	}
	// Overlays substitute a temporary copy during compilation. The source file
	// is never written, including on test failure or cancellation.
	defer func() {
		current, err := os.ReadFile(source)
		if err != nil || !bytes.Equal(current, original) {
			result.MutationDetected = false
			result.Error = "source changed during the check; inspect concurrent edits"
		}
	}()
	args := []string{"test", "-json", "-count=1", "-timeout=2m", c.Package, "-run", "^" + regexp.QuoteMeta(c.Test) + "$"}
	baseline := runGoTest(root, args)
	if err := os.WriteFile(filepath.Join(output, fmt.Sprintf("%02d-baseline.jsonl", index+1)), baseline.Output, 0644); err != nil {
		return fail(err)
	}
	if err := validateOutcome(baseline, c, false); err != nil {
		return fail(fmt.Errorf("baseline: %w", err))
	}
	result.BaselinePassed = true
	temporary, err := os.MkdirTemp("", "brain-mutation-overlay-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(temporary)
	replacement := filepath.Join(temporary, filepath.Base(source))
	mutated := bytes.Replace(original, []byte(c.Before), []byte(c.After), 1)
	if err := os.WriteFile(replacement, mutated, 0600); err != nil {
		return fail(err)
	}
	overlay := filepath.Join(temporary, "overlay.json")
	data, err := json.Marshal(map[string]any{"Replace": map[string]string{source: replacement}})
	if err != nil {
		return fail(err)
	}
	if err := os.WriteFile(overlay, data, 0600); err != nil {
		return fail(err)
	}
	mutantArgs := append([]string{"test", "-overlay=" + overlay}, args[1:]...)
	mutant := runGoTest(root, mutantArgs)
	if err := os.WriteFile(filepath.Join(output, fmt.Sprintf("%02d-mutant.jsonl", index+1)), mutant.Output, 0644); err != nil {
		return fail(err)
	}
	if err := validateOutcome(mutant, c, true); err != nil {
		return fail(fmt.Errorf("mutant: %w", err))
	}
	result.MutationDetected = true
	return result
}

func runGoTest(root string, args []string) processResult {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = root
	cmd.WaitDelay = 5 * time.Second
	// Prevent inherited display settings or an unrelated overlay from changing
	// the evidence. Keep the selected Go toolchain and module environment.
	for _, entry := range cmd.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if key != "FORCE_COLOR" && key != "GOFLAGS" {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	result := processResult{Output: output.Bytes()}
	if err == nil {
		return result
	}
	result.ExitCode = -1
	if ctx.Err() != nil {
		result.Err = ctx.Err()
	} else {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			result.ExitCode = exit.ExitCode()
		} else {
			result.Err = err
		}
	}
	return result
}

func validateOutcome(result processResult, c mutationCase, mutant bool) error {
	if result.Err != nil {
		return result.Err
	}
	wantExit := 0
	if mutant {
		wantExit = 1
	}
	if result.ExitCode != wantExit {
		return fmt.Errorf("exit %d, want %d", result.ExitCode, wantExit)
	}
	var passed, failed, anyFailure bool
	var diagnostic strings.Builder
	scanner := bufio.NewScanner(bytes.NewReader(result.Output))
	scanner.Buffer(make([]byte, 65536), 16<<20)
	for scanner.Scan() {
		var event struct{ Action, Test, Output string }
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}
		if event.Action == "build-fail" || strings.Contains(event.Output, "panic: test timed out") || strings.Contains(event.Output, "[build failed]") {
			return errors.New("build failure or timeout is not mutation detection")
		}
		if event.Action == "fail" {
			anyFailure = true
		}
		if event.Test == c.Test {
			passed = passed || event.Action == "pass"
			failed = failed || event.Action == "fail"
		}
		if event.Test == c.Test || strings.HasPrefix(event.Test, c.Test+"/") {
			diagnostic.WriteString(event.Output)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if !mutant {
		if !passed || anyFailure {
			return errors.New("named baseline test did not pass")
		}
	} else if !failed || passed || !strings.Contains(diagnostic.String(), c.Diagnostic) {
		return errors.New("named test did not fail with the intended assertion")
	}
	return nil
}
