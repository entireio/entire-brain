package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func reportFixture(t *testing.T, profile string) coverageReport {
	t.Helper()
	path := filepath.Join(t.TempDir(), "combined.out")
	if err := os.WriteFile(path, []byte(profile), 0600); err != nil {
		t.Fatal(err)
	}
	env := environment{GoVersion: "go version fixture", Platform: "windows/amd64", GoEnv: map[string]string{"GOFLAGS": ""},
		Scope: "./... plus instrumented binary contracts", TestsPassed: true, ShardCount: 1}
	r, err := summarizeProfile(path, env)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestReportTotalsAndCompatibleBaseline(t *testing.T) {
	r := reportFixture(t, "mode: set\nC:/src/a.go:1.1,2.2 3 1\nC:/src/a.go:3.1,4.2 1 0\np/b.go:1.1,2.2 0 0\n")
	if r.Totals != (coverageCount{4, 3, 75}) || r.Files["C:/src/a.go"].Percent != 75 || r.Files["p/b.go"].Percent != 100 || len(r.ProfileSHA256) != 64 {
		t.Fatalf("report: %+v", r)
	}
	baseline := reportFixture(t, "mode: set\np/a.go:1.1,2.2 2 1\np/b.go:1.1,2.2 2 0\n")
	delta, err := compareReports(r, baseline)
	if err != nil || delta != 25 {
		t.Fatalf("delta=%v err=%v", delta, err)
	}
	for _, change := range []struct {
		name   string
		mutate func(*coverageReport)
	}{
		{"failed", func(r *coverageReport) { r.Environment.TestsPassed = false }},
		{"toolchain", func(r *coverageReport) { r.Environment.GoVersion = "other" }},
		{"platform", func(r *coverageReport) { r.Environment.Platform = "linux/amd64" }},
		{"flags", func(r *coverageReport) { r.Environment.GoEnv = map[string]string{"GOFLAGS": "-tags different"} }},
		{"race", func(r *coverageReport) { r.Environment.Race = true }},
		{"binary only", func(r *coverageReport) { r.Environment.BinaryOnly = true }},
		{"group", func(r *coverageReport) { r.Environment.ShardIndex = 1 }},
		{"missing config", func(r *coverageReport) { r.Environment.GoEnv = nil }},
		{"missing files", func(r *coverageReport) { r.Files = nil }},
		{"bad totals", func(r *coverageReport) { r.Totals.Percent = 101 }},
		{"old schema", func(r *coverageReport) { r.SchemaVersion = 0 }},
	} {
		t.Run(change.name, func(t *testing.T) {
			bad := baseline
			change.mutate(&bad)
			if _, err := compareReports(r, bad); err == nil {
				t.Fatal("accepted incompatible baseline")
			}
		})
	}
}

func TestReportsRetainRegressionEvidence(t *testing.T) {
	baseline := reportFixture(t, "mode: set\np/a.go:1.1,2.2 1 1\np/b.go:1.1,2.2 1 0\n")
	baselinePath := filepath.Join(t.TempDir(), "summary.json")
	if err := writeJSON(baselinePath, baseline); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, profile string
		fail          bool
	}{
		{"same", "mode: set\np/a.go:1.1,2.2 1 1\np/b.go:1.1,2.2 1 0\n", false},
		{"decrease", "mode: set\np/a.go:1.1,2.2 1 0\np/b.go:1.1,2.2 1 0\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := t.TempDir()
			if err := writeJSON(filepath.Join(out, "environment.json"), baseline.Environment); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(out, "combined.out"), []byte(tc.profile), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(out, "tests.jsonl"), []byte("{\"Action\":\"pass\",\"Package\":\"p\"}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			err := finishReports(out, baselinePath, nil)
			if (err != nil) != tc.fail {
				t.Fatalf("error=%v", err)
			}
			data, err := os.ReadFile(filepath.Join(out, "summary.json"))
			var report coverageReport
			if err != nil || json.Unmarshal(data, &report) != nil || report.Delta == nil {
				t.Fatalf("missing comparison evidence: %s %v", data, err)
			}
		})
	}
}

func TestTestSummaryCountsRootsAndIncompleteRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tests.jsonl")
	data := `{"Action":"start","Package":"p"}
{"Action":"run","Package":"p","Test":"TestPass"}
{"Action":"pass","Package":"p","Test":"TestPass/child"}
{"Action":"pass","Package":"p","Test":"TestPass"}
{"Action":"skip","Package":"p","Test":"TestSkip"}
{"Action":"fail","Package":"p","Test":"TestFail"}
{"Action":"run","Package":"p","Test":"TestInterrupted"}
{"Action":"fail","Package":"p"}
compiler diagnostic
`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := summarizeTests(path)
	if err != nil || r.Tests != (outcomeCounts{1, 1, 1, 1}) || r.Packages.Failed != 1 || r.Diagnostics != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	if err := os.WriteFile(path, []byte(data+"{\"Action\":\"pass\",\"Package\":\"p\",\"Test\":\"TestPass\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := summarizeTests(path); err == nil {
		t.Fatal("accepted duplicate result")
	}
}

func TestSummaryRejectsMalformedProfiles(t *testing.T) {
	for _, contents := range []string{"mode: set\n", "mode: count\na:1.1,2.2 1 1\n", "mode: set\na 1 1\n", "mode: set\na:1.1,2.2 -1 1\n", "mode: set\na:1.1,2.2 1 2\n", "mode: set\na:1.1,2.2 1 1\na:1.1,2.2 1 1\n"} {
		path := filepath.Join(t.TempDir(), "profile")
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := summarizeProfile(path, environment{}); err == nil {
			t.Fatalf("accepted %q", contents)
		}
	}
}

func TestMergedTestLogCountsCLIPackageOnce(t *testing.T) {
	logs := map[int]string{}
	for i, name := range []string{"TestA", "TestB"} {
		logs[i] = filepath.Join(t.TempDir(), "tests.jsonl")
		data := `{"Action":"start","Package":"p/internal/cli"}` + "\n" +
			`{"Action":"pass","Package":"p/internal/cli","Test":"` + name + `"}` + "\n" +
			`{"Action":"pass","Package":"p/internal/cli"}` + "\n"
		if err := os.WriteFile(logs[i], []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(t.TempDir(), "tests.jsonl")
	if err := combineTestLogs(logs, out); err != nil {
		t.Fatal(err)
	}
	r, err := summarizeTests(out)
	if err != nil || r.Tests.Passed != 2 || r.Packages.Passed != 1 {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestCollectEnvironmentRecordsActualGoConfiguration(t *testing.T) {
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "-tags=coverage_fixture")
	e, err := collectEnvironment(t.TempDir(), options{cliShards: 1})
	if err != nil || !strings.HasPrefix(e.GoVersion, "go version ") || e.GoEnv["GOFLAGS"] != "-tags=coverage_fixture" || e.Platform == "" || e.VCSAvailable {
		t.Fatalf("%+v %v", e, err)
	}
}
