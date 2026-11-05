package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
)

type environment struct {
	Commit         string            `json:"commit"`
	WorktreeStatus string            `json:"worktree_status"`
	VCSAvailable   bool              `json:"vcs_available"`
	GoVersion      string            `json:"go_version"`
	Platform       string            `json:"platform"`
	GoEnv          map[string]string `json:"go_env"`
	Race           bool              `json:"race"`
	BinaryOnly     bool              `json:"binary_only"`
	Scope          string            `json:"scope"`
	TestTimeout    string            `json:"test_timeout"`
	ShardCount     int               `json:"shard_count"`
	ShardIndex     int               `json:"shard_index"`
	TestsPassed    bool              `json:"tests_passed"`
	RunExitCode    int               `json:"run_exit_code"`
}

func collectEnvironment(root string, opts options) (environment, error) {
	capture := func(binary string, args ...string) ([]byte, error) {
		cmd := exec.Command(binary, args...)
		cmd.Dir = root
		return cmd.Output()
	}
	e := environment{Race: opts.race, BinaryOnly: opts.binaryOnly, TestTimeout: opts.timeout,
		ShardCount: opts.cliShards, ShardIndex: opts.cliShard, RunExitCode: -1}
	commit, err := capture("git", "rev-parse", "HEAD")
	if err == nil {
		e.Commit, e.VCSAvailable = strings.TrimSpace(string(commit)), true
		status, err := capture("git", "status", "--porcelain")
		if err != nil {
			return e, err
		}
		e.WorktreeStatus = string(status)
	}
	version, err := capture("go", "version")
	if err != nil {
		return e, err
	}
	e.GoVersion = strings.TrimSpace(string(version))
	data, err := capture("go", "env", "-json", "GOOS", "GOARCH", "CGO_ENABLED", "GOFLAGS", "GOEXPERIMENT", "GOAMD64", "GOARM64", "GOTOOLCHAIN")
	if err != nil {
		return e, err
	}
	if err := json.Unmarshal(data, &e.GoEnv); err != nil {
		return e, err
	}
	e.Platform = e.GoEnv["GOOS"] + "/" + e.GoEnv["GOARCH"]
	e.Scope = "./... plus instrumented binary contracts"
	if opts.binaryOnly {
		e.Scope = "instrumented binary contracts"
	}
	if opts.cliShards > 1 {
		e.Scope = "CLI group plus instrumented binary contracts"
	}
	return e, nil
}

type coverageCount struct {
	Statements int64   `json:"statements"`
	Covered    int64   `json:"covered"`
	Percent    float64 `json:"percent"`
}

func (c *coverageCount) percentage() {
	c.Percent = 100
	if c.Statements > 0 {
		c.Percent = 100 * float64(c.Covered) / float64(c.Statements)
	}
}

type coverageReport struct {
	SchemaVersion int                      `json:"schema_version"`
	Metric        string                   `json:"metric"`
	Environment   environment              `json:"environment"`
	Totals        coverageCount            `json:"totals"`
	Files         map[string]coverageCount `json:"files"`
	ProfileSHA256 string                   `json:"profile_sha256"`
	Delta         *float64                 `json:"delta_percentage_points,omitempty"`
}

func summarizeProfile(path string, env environment) (coverageReport, error) {
	r := coverageReport{SchemaVersion: 1, Metric: "Go statements", Environment: env, Files: map[string]coverageCount{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return r, err
	}
	hash := sha256.Sum256(data)
	r.ProfileSHA256 = hex.EncodeToString(hash[:])
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 2 || lines[0] != "mode: set" {
		return r, errors.New("expected a merged set coverage profile")
	}
	seen := map[string]bool{}
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) != 3 || seen[fields[0]] {
			return r, fmt.Errorf("invalid or duplicate coverage block: %q", line)
		}
		seen[fields[0]] = true
		colon := strings.LastIndex(fields[0], ":")
		if colon < 1 {
			return r, fmt.Errorf("invalid coverage location: %q", fields[0])
		}
		n, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || n < 0 {
			return r, fmt.Errorf("invalid statement count: %q", line)
		}
		if fields[2] != "0" && fields[2] != "1" {
			return r, fmt.Errorf("invalid set count: %q", line)
		}
		name := fields[0][:colon]
		c := r.Files[name]
		if n > math.MaxInt64-r.Totals.Statements {
			return r, errors.New("statement count overflow")
		}
		c.Statements += n
		r.Totals.Statements += n
		if fields[2] == "1" {
			c.Covered += n
			r.Totals.Covered += n
		}
		c.percentage()
		r.Files[name] = c
	}
	r.Totals.percentage()
	return r, nil
}

func compareReports(current, baseline coverageReport) (float64, error) {
	for _, r := range []coverageReport{current, baseline} {
		if r.SchemaVersion != 1 || r.Metric != "Go statements" || !r.Environment.TestsPassed || r.Environment.RunExitCode != 0 {
			return 0, errors.New("baseline comparison requires two successful coverage runs with schema version 1")
		}
		if r.Environment.GoVersion == "" || r.Environment.Platform == "" || len(r.Environment.GoEnv) == 0 || r.Environment.Scope == "" {
			return 0, errors.New("baseline comparison requires complete build configuration")
		}
		var totals coverageCount
		if len(r.Files) == 0 {
			return 0, errors.New("empty coverage summary")
		}
		for _, c := range r.Files {
			if c.Statements < 0 || c.Covered < 0 || c.Covered > c.Statements || c.Statements > math.MaxInt64-totals.Statements {
				return 0, errors.New("invalid coverage summary counts")
			}
			totals.Statements += c.Statements
			totals.Covered += c.Covered
		}
		totals.percentage()
		if totals != r.Totals {
			return 0, errors.New("inconsistent coverage summary totals")
		}
	}
	if err := equivalentConfiguration(current.Environment, baseline.Environment); err != nil {
		return 0, err
	}
	return current.Totals.Percent - baseline.Totals.Percent, nil
}

func equivalentConfiguration(a, b environment) error {
	if a.GoVersion != b.GoVersion || a.Platform != b.Platform || !reflect.DeepEqual(a.GoEnv, b.GoEnv) ||
		a.Race != b.Race || a.BinaryOnly != b.BinaryOnly || a.Scope != b.Scope ||
		a.ShardCount != b.ShardCount || a.ShardIndex != b.ShardIndex || a.TestTimeout != b.TestTimeout {
		return errors.New("baseline configuration differs: toolchain, platform, build flags, race, scope, timeout, or test group")
	}
	return nil
}

type outcomeCounts struct {
	Passed     int `json:"passed"`
	Skipped    int `json:"skipped"`
	Failed     int `json:"failed"`
	Incomplete int `json:"incomplete"`
}

type testSummary struct {
	Tests       outcomeCounts `json:"tests"`
	Packages    outcomeCounts `json:"packages"`
	Diagnostics int           `json:"diagnostic_lines"`
	RunPassed   bool          `json:"run_passed"`
	Error       string        `json:"error,omitempty"`
}

func summarizeTests(path string) (testSummary, error) {
	var result testSummary
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	} // Binary-only runs have no test stream.
	if err != nil {
		return result, err
	}
	defer f.Close()
	states := map[string]string{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 65536), 16<<20)
	for scanner.Scan() {
		var event struct{ Action, Package, Test string }
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			result.Diagnostics++
			continue
		}
		if event.Package == "" || strings.Contains(event.Test, "/") {
			continue
		}
		key := event.Package + "\x00" + event.Test
		switch event.Action {
		case "start", "run":
			if _, ok := states[key]; ok {
				return result, fmt.Errorf("repeated test start: %s", key)
			}
			states[key] = "incomplete"
		case "pass", "fail", "skip":
			if prev := states[key]; prev != "" && prev != "incomplete" {
				return result, fmt.Errorf("repeated test outcome: %s", key)
			}
			states[key] = event.Action
		}
	}
	for key, state := range states {
		counts := &result.Tests
		if strings.HasSuffix(key, "\x00") {
			counts = &result.Packages
		}
		switch state {
		case "pass":
			counts.Passed++
		case "fail":
			counts.Failed++
		case "skip":
			counts.Skipped++
		default:
			counts.Incomplete++
		}
	}
	return result, scanner.Err()
}

func finishReports(out, baseline string, runErr error) error {
	data, err := os.ReadFile(filepath.Join(out, "environment.json"))
	if err != nil {
		return err
	}
	var env environment
	if err := json.Unmarshal(data, &env); err != nil {
		return err
	}
	results, summaryErr := summarizeTests(filepath.Join(out, "tests.jsonl"))
	if !env.BinaryOnly && results.Packages.Passed+results.Packages.Skipped+results.Packages.Failed == 0 {
		summaryErr = errors.Join(summaryErr, errors.New("missing package outcomes for a test-suite run"))
	}
	if results.Tests.Failed+results.Packages.Failed+results.Tests.Incomplete+results.Packages.Incomplete > 0 {
		summaryErr = errors.Join(summaryErr, errors.New("test results contain failed or incomplete outcomes"))
	}
	runErr = errors.Join(runErr, summaryErr)
	// Compare against the test-run status first. Final report status must also
	// include profile and baseline errors, which are only known below.
	env.TestsPassed = runErr == nil
	env.RunExitCode = 0
	if runErr != nil {
		env.RunExitCode = 1
	}
	r, profileErr := summarizeProfile(filepath.Join(out, "combined.out"), env)
	var comparisonErr error
	if profileErr == nil && baseline != "" {
		data, err := os.ReadFile(baseline)
		var previous coverageReport
		if err == nil {
			err = json.Unmarshal(data, &previous)
		}
		if err == nil {
			var delta float64
			delta, err = compareReports(r, previous)
			if err == nil {
				r.Delta = &delta
				if delta < -0.01 {
					err = fmt.Errorf("statement coverage decreased by %.4f percentage points", -delta)
				}
			}
		}
		comparisonErr = err
	}
	runErr = errors.Join(runErr, profileErr, comparisonErr)
	env.TestsPassed = runErr == nil
	env.RunExitCode = 0
	if runErr != nil {
		env.RunExitCode = 1
		results.Error = runErr.Error()
	}
	results.RunPassed = env.TestsPassed
	r.Environment = env
	writeErr := errors.Join(
		writeJSON(filepath.Join(out, "test-results.json"), results),
		writeJSON(filepath.Join(out, "environment.json"), env),
	)
	if profileErr == nil {
		writeErr = errors.Join(writeErr, writeJSON(filepath.Join(out, "summary.json"), r))
	}
	return errors.Join(summaryErr, profileErr, comparisonErr, writeErr)
}
