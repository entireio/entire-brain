// Command coverage runs Go tests and CLI contracts with Go's native coverage tools.
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
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

type options struct {
	output              string
	race, binaryOnly    bool
	timeout             string
	cliShards, cliShard int
	mergeInput          string
	baseline            string
}

func main() {
	var opts options
	flag.StringVar(&opts.output, "output", "", "directory for coverage and test results")
	flag.BoolVar(&opts.race, "race", false, "enable the Go race detector")
	flag.BoolVar(&opts.binaryOnly, "binary-only", false, "run only CLI contracts")
	flag.StringVar(&opts.timeout, "test-timeout", "30m", "Go test timeout per package")
	flag.IntVar(&opts.cliShards, "cli-shards", 1, "number of independent CLI test groups")
	flag.IntVar(&opts.cliShard, "cli-shard", 0, "zero-based CLI test group")
	flag.StringVar(&opts.mergeInput, "merge-input", "", "merge profiles from this artifact directory without running tests")
	flag.StringVar(&opts.baseline, "baseline", "", "same-configuration summary.json; fail on a decrease greater than 0.01 percentage points")
	flag.Parse()
	root, err := os.Getwd()
	if err == nil {
		err = runCoverage(root, opts)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func command(root string, output io.Writer, args ...string) error {
	cmd := exec.Command("go", args...)
	cmd.Dir = root
	cmd.Stdout = output
	cmd.Stderr = output
	return cmd.Run()
}

func runCoverage(root string, opts options) (result error) {
	if opts.cliShards == 0 {
		opts.cliShards = 1
	}
	if opts.cliShards < 1 || opts.cliShard < 0 || opts.cliShard >= opts.cliShards {
		return errors.New("invalid CLI test group")
	}
	if opts.output == "" {
		return errors.New("-output is required")
	}
	out, err := filepath.Abs(opts.output)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(out, 0755); err != nil {
		return err
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return errors.New("output directory must be empty to prevent stale coverage")
	}
	defer func() { result = errors.Join(result, finishReports(out, opts.baseline, result)) }()
	if opts.mergeInput != "" {
		return mergeArtifacts(root, opts.mergeInput, out)
	}
	metadata, err := collectEnvironment(root, opts)
	if err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(out, "environment.json"), metadata); err != nil {
		return err
	}
	flags := []string{}
	if opts.race {
		flags = append(flags, "-race")
	}
	profiles := []string{}
	var testErr error
	if !opts.binaryOnly {
		log, err := os.Create(filepath.Join(out, "tests.jsonl"))
		if err != nil {
			return err
		}
		args := append([]string{"test"}, flags...)
		args = append(args, "-count=1", "-timeout", opts.timeout, "-covermode=atomic", "-coverpkg=./...", "-coverprofile="+filepath.Join(out, "tests.out"), "-json", "./...")
		fmt.Println("Running Go tests with coverage")
		if runtime.GOOS == "windows" || opts.cliShards > 1 {
			// Detached CLI workers can keep the test executable mapped after exit.
			// Retain that executable outside go test's temporary cleanup directory.
			testErr = splitTests(root, out, opts.timeout, flags, log, opts.cliShard, opts.cliShards)
			profiles = append(profiles, filepath.Join(out, "cli.out"))
		} else {
			testErr = command(root, log, args...)
		}
		if err = log.Close(); err != nil {
			return err
		}
		if testErr != nil {
			if err = printFailureLog(filepath.Join(out, "tests.jsonl"), os.Stderr); err != nil {
				return errors.Join(testErr, err)
			}
		}
		if opts.cliShard == 0 {
			profiles = append(profiles, filepath.Join(out, "tests.out"))
		}
	}
	build, err := os.MkdirTemp("", "brain-instrumented-")
	if err != nil {
		return errors.Join(testErr, err)
	}
	defer os.RemoveAll(build)
	binary := filepath.Join(build, "entire-brain")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	args := append([]string{"build"}, flags...)
	args = append(args, "-cover", "-covermode=atomic", "-coverpkg=./...", "-o", binary, "./cmd/entire-brain")
	if err = command(root, os.Stderr, args...); err != nil {
		return errors.Join(testErr, err)
	}
	counters := filepath.Join(build, "counters")
	if err = os.Mkdir(counters, 0755); err != nil {
		return errors.Join(testErr, err)
	}
	if err = binaryContracts(binary, out, counters); err != nil {
		return errors.Join(testErr, err)
	}
	binaryProfile := filepath.Join(out, "binary.out")
	if err = command(root, os.Stderr, "tool", "covdata", "textfmt", "-i="+counters, "-o="+binaryProfile); err != nil {
		return errors.Join(testErr, err)
	}
	profiles = append(profiles, binaryProfile)
	combined := filepath.Join(out, "combined.out")
	if err = mergeProfiles(profiles, combined); err != nil {
		return errors.Join(testErr, err)
	}
	if err = command(root, os.Stdout, "tool", "cover", "-func="+combined); err != nil {
		return errors.Join(testErr, err)
	}
	err = command(root, os.Stderr, "tool", "cover", "-html="+combined, "-o", filepath.Join(out, "coverage.html"))
	return errors.Join(testErr, err)
}

func binaryContracts(binary, out, counters string) error {
	dir, err := os.MkdirTemp("", "brain-cli-contract-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	env := []string{}
	isolated := map[string]bool{"HOME": true, "USERPROFILE": true, "APPDATA": true, "LOCALAPPDATA": true, "XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true, "XDG_STATE_HOME": true, "XDG_CACHE_HOME": true}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		key = strings.ToUpper(key)
		if isolated[key] || strings.HasPrefix(key, "ENTIRE_") || strings.HasPrefix(key, "GIT_") || strings.HasPrefix(key, "XDG_") || key == "GOCOVERDIR" {
			continue
		}
		env = append(env, entry)
	}
	for key := range isolated {
		env = append(env, key+"="+filepath.Join(dir, key))
	}
	env = append(env, "GOCOVERDIR="+counters)
	for _, tc := range []struct {
		name string
		args []string
		code int
	}{
		{"help", []string{"--help"}, 0}, {"version", []string{"version"}, 0}, {"capabilities", []string{"capabilities", "--json"}, 0}, {"invalid-command", []string{"not-a-brain-command"}, 1},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cmd := exec.CommandContext(ctx, binary, tc.args...)
		cmd.Dir = dir
		cmd.Env = env
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		cancel()
		code := 0
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				return err
			}
			code = exit.ExitCode()
		}
		if err = writeJSON(filepath.Join(out, "binary-"+tc.name+".json"), map[string]any{"args": tc.args, "exit_code": code, "stdout": stdout.String(), "stderr": stderr.String()}); err != nil {
			return err
		}
		if code != tc.code {
			return fmt.Errorf("%s: exit %d, want %d: %s", tc.name, code, tc.code, stderr.String())
		}
		switch tc.name {
		case "help":
			if !strings.Contains(stdout.String(), "Usage:") {
				return errors.New("missing CLI usage")
			}
		case "version":
			if strings.TrimSpace(stdout.String()) != "dev" {
				return errors.New("unexpected unstamped version")
			}
		case "invalid-command":
			if !strings.Contains(stderr.String(), "unknown command") {
				return errors.New("missing unknown-command diagnostic")
			}
		case "capabilities":
			var caps struct {
				Schema int `json:"schema_version"`
				Build  struct {
					CGO *bool `json:"brain_cgo"`
				} `json:"build"`
			}
			if err = json.Unmarshal(stdout.Bytes(), &caps); err != nil {
				return err
			}
			if caps.Schema != 1 || caps.Build.CGO == nil || *caps.Build.CGO {
				return errors.New("unexpected CLI capabilities")
			}
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("metadata commands created persistent state")
	}
	return nil
}

func printFailureLog(path string, out io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 65536), 16<<20)
	for scanner.Scan() {
		var event struct{ Action, Output string }
		if json.Unmarshal(scanner.Bytes(), &event) != nil || event.Action == "" {
			fmt.Fprintln(out, scanner.Text())
		} else if event.Output != "" {
			fmt.Fprint(out, event.Output)
		}
	}
	return scanner.Err()
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0644)
}

func mergeProfiles(paths []string, output string) error {
	type block struct {
		statements int
		hit        bool
	}
	blocks := map[string]block{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		if len(lines) < 2 || (lines[0] != "mode: atomic" && lines[0] != "mode: count" && lines[0] != "mode: set") {
			return fmt.Errorf("invalid or empty coverage profile: %s", path)
		}
		for _, line := range lines[1:] {
			fields := strings.Fields(line)
			if len(fields) != 3 {
				return fmt.Errorf("invalid coverage block: %q", line)
			}
			n, err := strconv.Atoi(fields[1])
			if err != nil || n < 0 {
				return fmt.Errorf("invalid statement count: %q", line)
			}
			count, err := strconv.ParseInt(fields[2], 10, 64)
			if err != nil || count < 0 {
				return fmt.Errorf("invalid hit count: %q", line)
			}
			old, exists := blocks[fields[0]]
			if exists && old.statements != n {
				return fmt.Errorf("incompatible coverage block: %s", fields[0])
			}
			blocks[fields[0]] = block{n, old.hit || count > 0}
		}
	}
	keys := make([]string, 0, len(blocks))
	for key := range blocks {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var text strings.Builder
	text.WriteString("mode: set\n")
	for _, key := range keys {
		b := blocks[key]
		hit := 0
		if b.hit {
			hit = 1
		}
		fmt.Fprintf(&text, "%s %d %d\n", key, b.statements, hit)
	}
	return os.WriteFile(output, []byte(text.String()), 0644)
}
