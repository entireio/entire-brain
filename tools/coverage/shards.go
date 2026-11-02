package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

func splitTests(root, out, timeout string, flags []string, log io.Writer, index, count int) error {
	var packages bytes.Buffer
	list := exec.Command("go", "list", "./...")
	list.Dir, list.Stdout, list.Stderr = root, &packages, log
	if err := list.Run(); err != nil {
		return err
	}
	var others []string
	var cli string
	for _, name := range strings.Fields(packages.String()) {
		if strings.HasSuffix(name, "/internal/cli") {
			cli = name
		} else {
			others = append(others, name)
		}
	}
	if cli == "" {
		return errors.New("CLI test package not found")
	}
	var otherErr error
	if index == 0 {
		args := append([]string{"test"}, flags...)
		args = append(args, "-count=1", "-timeout", timeout, "-covermode=atomic", "-coverpkg=./...", "-coverprofile="+filepath.Join(out, "tests.out"), "-json")
		otherErr = command(root, log, append(args, others...)...)
	}
	binary := filepath.Join(out, "cli.test.exe")
	args := append([]string{"test", "-c"}, flags...)
	args = append(args, "-covermode=atomic", "-coverpkg=./...", "-o", binary, cli)
	if err := command(root, log, args...); err != nil {
		return errors.Join(otherErr, err)
	}
	var inventory bytes.Buffer
	list = exec.Command(binary, "-test.list=.")
	list.Dir, list.Stdout, list.Stderr = filepath.Join(root, "internal", "cli"), &inventory, log
	if err := list.Run(); err != nil {
		return errors.Join(otherErr, err)
	}
	all, selected, err := selectRoots(inventory.String(), index, count)
	if err != nil {
		return errors.Join(otherErr, err)
	}
	if err := writeJSON(filepath.Join(out, "shard-plan.json"), map[string]any{"index": index, "count": count, "all": all, "selected": selected}); err != nil {
		return errors.Join(otherErr, err)
	}
	quoted := make([]string, len(selected))
	for i, name := range selected {
		quoted[i] = regexp.QuoteMeta(name)
	}
	run := "^(" + strings.Join(quoted, "|") + ")$"
	if len(run) > 28000 {
		return errors.Join(otherErr, errors.New("test group exceeds the Windows command-line budget; increase -cli-shards"))
	}
	var toolDir bytes.Buffer
	if err := command(root, &toolDir, "env", "GOTOOLDIR"); err != nil {
		return errors.Join(otherErr, err)
	}
	cliLog, err := os.Create(filepath.Join(out, "cli.jsonl"))
	if err != nil {
		return errors.Join(otherErr, err)
	}
	// Keep the binary in the artifact directory: detached Windows workers can
	// still have it mapped after the test process exits.
	cmd := exec.Command("go",
		"tool", "test2json", "-t", "-p", cli, binary,
		"-test.paniconexit0", "-test.v=test2json", "-test.count=1", "-test.shuffle=off",
		"-test.run="+run, "-test.timeout="+timeout, "-test.coverprofile="+filepath.Join(out, "cli.out"))
	cmd.Dir = filepath.Join(root, "internal", "cli")
	cmd.Env = append(cmd.Environ(), "PATH="+strings.TrimSpace(toolDir.String())+string(os.PathListSeparator)+os.Getenv("PATH"))
	combinedOutput := io.MultiWriter(log, cliLog)
	cmd.Stdout, cmd.Stderr = combinedOutput, combinedOutput
	err = cmd.Run()
	closeErr := cliLog.Close()
	return errors.Join(otherErr, err, closeErr, verifyRootOutcomes(filepath.Join(out, "cli.jsonl"), selected))
}

func selectRoots(output string, index, count int) ([]string, []string, error) {
	if count < 1 || index < 0 || index >= count {
		return nil, nil, errors.New("invalid CLI test group")
	}
	var all []string
	seen := map[string]bool{}
	validRoot := regexp.MustCompile(`^(Test|Fuzz|Example)[\pL\pN_]*$`)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Benchmark") || strings.HasPrefix(line, "coverage:") {
			continue
		}
		if !validRoot.MatchString(line) || seen[line] {
			return nil, nil, fmt.Errorf("invalid test inventory entry: %q", line)
		}
		seen[line] = true
		all = append(all, line)
	}
	sort.Strings(all)
	var selected []string
	for i, name := range all {
		if i%count == index {
			selected = append(selected, name)
		}
	}
	if len(selected) == 0 {
		return nil, nil, errors.New("empty CLI test group")
	}
	return all, selected, nil
}

func verifyRootOutcomes(path string, selected []string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	wanted := map[string]bool{}
	for _, name := range selected {
		wanted[name] = true
	}
	seen := map[string]bool{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 65536), 16<<20)
	for scanner.Scan() {
		var event struct{ Action, Test string }
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return fmt.Errorf("invalid test event: %w", err)
		}
		if event.Test == "" || strings.Contains(event.Test, "/") {
			continue
		}
		if event.Action != "pass" && event.Action != "skip" && event.Action != "fail" {
			continue
		}
		if !wanted[event.Test] || seen[event.Test] {
			return fmt.Errorf("unexpected or repeated test outcome: %s", event.Test)
		}
		seen[event.Test] = true
		if event.Action == "fail" {
			return fmt.Errorf("test failed: %s", event.Test)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if len(seen) != len(wanted) {
		return fmt.Errorf("incomplete CLI group: %d of %d test outcomes", len(seen), len(wanted))
	}
	return nil
}

func mergeArtifacts(root, input, out string) error {
	var profiles []string
	var common *environment
	logs := map[int]string{}
	plans := map[int]struct {
		Count         int
		All, Selected []string
	}{}
	err := filepath.WalkDir(input, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Name() == "shard-plan.json" {
			var plan struct {
				Index, Count  int
				All, Selected []string
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if err := json.Unmarshal(data, &plan); err != nil {
				return err
			}
			if _, exists := plans[plan.Index]; exists {
				return errors.New("duplicate test group")
			}
			dir := filepath.Dir(path)
			data, err = os.ReadFile(filepath.Join(dir, "environment.json"))
			if err != nil {
				return err
			}
			var env environment
			if err := json.Unmarshal(data, &env); err != nil {
				return err
			}
			if !env.TestsPassed || env.RunExitCode != 0 || env.ShardIndex != plan.Index || env.ShardCount != plan.Count {
				return errors.New("failed or mismatched test group environment")
			}
			env.ShardIndex = 0
			if common == nil {
				common = &env
			} else {
				if err := equivalentConfiguration(*common, env); err != nil {
					return err
				}
				if common.Commit != env.Commit || common.WorktreeStatus != env.WorktreeStatus || common.VCSAvailable != env.VCSAvailable {
					return errors.New("test groups use different source revisions")
				}
			}
			logs[plan.Index] = filepath.Join(dir, "tests.jsonl")
			all, selected, err := selectRoots(strings.Join(plan.All, "\n"), plan.Index, plan.Count)
			if err != nil || strings.Join(selected, "\n") != strings.Join(plan.Selected, "\n") {
				return errors.New("invalid test group plan")
			}
			plans[plan.Index] = struct {
				Count         int
				All, Selected []string
			}{plan.Count, all, selected}
			if err := verifyRootOutcomes(filepath.Join(filepath.Dir(path), "cli.jsonl"), selected); err != nil {
				return err
			}
			for _, name := range []string{"cli.out", "binary.out"} {
				profiles = append(profiles, filepath.Join(filepath.Dir(path), name))
			}
			if plan.Index == 0 {
				profiles = append(profiles, filepath.Join(filepath.Dir(path), "tests.out"))
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	first, ok := plans[0]
	if !ok || first.Count != len(plans) {
		return errors.New("missing test groups")
	}
	for i := 0; i < first.Count; i++ {
		plan, ok := plans[i]
		if !ok || plan.Count != first.Count || strings.Join(plan.All, "\n") != strings.Join(first.All, "\n") {
			return errors.New("inconsistent test group inventories")
		}
	}
	common.ShardCount, common.ShardIndex = 1, 0
	common.Scope = "./... plus instrumented binary contracts"
	if err := writeJSON(filepath.Join(out, "environment.json"), common); err != nil {
		return err
	}
	if err := combineTestLogs(logs, filepath.Join(out, "tests.jsonl")); err != nil {
		return err
	}
	combined := filepath.Join(out, "combined.out")
	if err := mergeProfiles(profiles, combined); err != nil {
		return err
	}
	return command(root, os.Stderr, "tool", "cover", "-html="+combined, "-o", filepath.Join(out, "coverage.html"))
}

func combineTestLogs(logs map[int]string, output string) error {
	f, err := os.Create(output)
	if err != nil {
		return err
	}
	defer f.Close()
	for i := 0; i < len(logs); i++ {
		if err := appendTestLog(f, logs[i], i); err != nil {
			return err
		}
	}
	return f.Close()
}

func appendTestLog(out io.Writer, path string, index int) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 65536), 16<<20)
	for scanner.Scan() {
		var event struct{ Action, Package, Test string }
		// Each group executes the CLI package. Count its package outcome once;
		// retain every root test outcome across all groups.
		if json.Unmarshal(scanner.Bytes(), &event) == nil && index != 0 && event.Test == "" && strings.HasSuffix(event.Package, "/internal/cli") {
			continue
		}
		if _, err := fmt.Fprintln(out, scanner.Text()); err != nil {
			return err
		}
	}
	return scanner.Err()
}
