package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/entireio/entire-brain/internal/cli/internal/semstore"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

func TestLegacyBrainLayoutDetection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source *semanticSourceManifest
		path   string
		legacy bool
	}{
		{name: "fresh"},
		{name: "current", source: &semanticSourceManifest{StoreOwner: "graph"}},
		{name: "old manifest", source: &semanticSourceManifest{}, legacy: true},
		{name: "other owner", source: &semanticSourceManifest{StoreOwner: "brain"}, legacy: true},
		{name: "orphan generations", path: "semantic/generations", legacy: true},
		{name: "orphan snapshots", path: "semantic/snapshots", legacy: true},
		{name: "interrupted cleanup", source: &semanticSourceManifest{StoreOwner: "graph"}, path: "semantic/generations", legacy: true},
		{name: "current traces", path: "semantic/traces"},
		{name: "unrelated embeddings", path: "facts/main/embeddings"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			brain := filepath.Join(t.TempDir(), "brain")
			if tc.path != "" {
				if err := os.MkdirAll(filepath.Join(brain, tc.path), 0700); err != nil {
					t.Fatal(err)
				}
			}
			manifest := &exportManifest{Sources: &brainSources{Semantic: tc.source}}
			err := checkLegacyBrainLayout(brain, manifest)
			if errors.Is(err, errLegacyBrainLayout) != tc.legacy || (!tc.legacy && err != nil) {
				t.Fatalf("legacy=%v, err=%v", tc.legacy, err)
			}
			if tc.legacy {
				for _, want := range []string{"0.2.0", "refresh index", "preserve runtime traces", "back up", brain} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("missing recovery instruction %q: %v", want, err)
					}
				}
			}
			if tc.path == "" {
				if _, err := os.Stat(brain); !os.IsNotExist(err) {
					t.Fatalf("check created a brain directory: %v", err)
				}
			}
		})
	}
}

func TestLegacyBrainLayoutRejectsSymlinks(t *testing.T) {
	for _, rel := range []string{"semantic", "semantic/generations", "semantic/snapshots"} {
		t.Run(rel, func(t *testing.T) {
			brain, outside := t.TempDir(), t.TempDir()
			link := filepath.Join(brain, rel)
			if err := os.MkdirAll(filepath.Dir(link), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, link); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			if err := checkLegacyBrainLayout(brain, nil); err == nil {
				t.Fatal("accepted a symlink in the old layout")
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 0 {
				t.Fatalf("changed symlink target: %v, %v", entries, err)
			}
		})
	}
}

// The old SQLite bytes are deliberately unreadable. Detection must not try to
// open them or turn a failed migration into a successful, destructive refresh.
func seedLegacyBrainLayout(t *testing.T, brain string) map[string][]byte {
	t.Helper()
	rel := "semantic/generations/legacy/semantic.sqlite"
	if err := os.MkdirAll(filepath.Dir(filepath.Join(brain, rel)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brain, rel), []byte("unreadable old store with authored traces"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeBrainSemanticSource(brain, "gh/example/repo", &semanticSourceManifest{StorePath: rel, GenerationPath: "semantic/generations/legacy"}); err != nil {
		t.Fatal(err)
	}
	if err := writeGraphTraces(brain, []semstore.TraceObservation{{FromID: "preserved"}}); err != nil {
		t.Fatal(err)
	}
	before := map[string][]byte{}
	for _, path := range []string{bmanifest.FileName, rel, semstore.TraceFile} {
		data, err := os.ReadFile(filepath.Join(brain, path))
		if err != nil {
			t.Fatal(err)
		}
		before[path] = data
	}
	return before
}

func assertLegacyBrainUnchanged(t *testing.T, brain string, before map[string][]byte) {
	t.Helper()
	for path, want := range before {
		got, err := os.ReadFile(filepath.Join(brain, path))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("changed %s: %v", path, err)
		}
	}
}

func TestLegacyBrainOperationsRefuseWithoutChangingData(t *testing.T) {
	for _, operation := range []string{"index", "index without Graph", "code", "traces", "refresh", "force refresh", "best effort refresh", "force explicit output", "bundle export"} {
		t.Run(operation, func(t *testing.T) {
			opts, repo, brain, _ := newGraphStoreFixture(t)
			before := seedLegacyBrainLayout(t, brain)
			cmd := &cobra.Command{}
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			var err error
			switch operation {
			case "index", "index without Graph":
				err = runSemanticIndex(context.Background(), cmd, opts, semanticIndexOptions{skipGraph: operation == "index without Graph"}, repo)
			case "code":
				_, err = loadGraphManifest(context.Background(), opts, repo, brain)
			case "traces":
				path := filepath.Join(t.TempDir(), "trace.jsonl")
				if err := os.WriteFile(path, []byte("{\"from\":\"render\",\"to\":\"normalize\",\"type\":\"CALLS\"}\n"), 0600); err != nil {
					t.Fatal(err)
				}
				err = ingestGraphTraces(cmd, opts, semanticTraceIngestOptions{}, path)
			case "bundle export":
				archive := filepath.Join(t.TempDir(), "brain.tar")
				err = runSemanticBundleExport(context.Background(), cmd, opts, archive, false)
				if _, statErr := os.Stat(archive); !os.IsNotExist(statErr) {
					t.Errorf("created bundle: %v", statErr)
				}
			default:
				refresh := defaultRefreshCommandOptions()
				refresh.seed.agent = "none"
				refresh.force = strings.HasPrefix(operation, "force")
				refresh.bestEffort = operation == "best effort refresh"
				if operation == "force explicit output" {
					cmd.Flags().String("output", "", "")
					if err := cmd.Flags().Set("output", brain); err != nil {
						t.Fatal(err)
					}
					refresh.outputDir = brain
				}
				err = runRefresh(context.Background(), cmd, opts, refresh)
			}
			if !errors.Is(err, errLegacyBrainLayout) {
				t.Fatalf("expected old-layout error, got %v\n%s", err, out.String())
			}
			assertLegacyBrainUnchanged(t, brain, before)
			if fakeRunnerCalled(opts.Runner.(*fakeCommandRunner), "entire", "graph", "index", "--repo", repo, "--head", "--profile", "full", "--format", "json") {
				t.Fatal("ran Graph indexing on an old brain")
			}
		})
	}
}

func TestLegacyBrainStatusAndDoctorShowRecovery(t *testing.T) {
	opts, repo, brain, _ := newGraphStoreFixture(t)
	before := seedLegacyBrainLayout(t, brain)
	for _, lightweight := range []bool{true, false} {
		report, err := buildBrainStatusReportWithAvailability(context.Background(), opts, repo, lightweight)
		if err != nil {
			t.Fatal(err)
		}
		if report.Sources.Semantic {
			t.Fatal("reported old layout as a usable code index")
		}
		if !strings.Contains(strings.Join(report.Warnings, "\n"), "0.2.0") {
			t.Fatalf("missing recovery message: %+v", report.Warnings)
		}
	}
	for _, args := range [][]string{{"status", "--json"}, {"status"}} {
		out, err := execute(t, NewRootCommand(opts), args...)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "0.2.0") || !strings.Contains(out, "refresh index") {
			t.Fatalf("status omitted recovery instructions: %s", out)
		}
	}
	out, err := execute(t, NewRootCommand(opts), "status", "--json", "--fail-on", "unsafe")
	if !errors.Is(err, errSemanticAuditGate) || !strings.Contains(out, "0.2.0") {
		t.Fatalf("status did not fail with recovery instructions: %v\n%s", err, out)
	}
	for _, args := range [][]string{{"doctor", "--json"}, {"doctor"}} {
		out, err := execute(t, NewRootCommand(opts), args...)
		if err == nil {
			t.Fatalf("doctor did not fail for old layout: %s", out)
		}
		if !strings.Contains(out, "0.2.0") || !strings.Contains(out, "refresh index") {
			t.Fatalf("doctor omitted recovery instructions: %s", out)
		}
		if len(args) > 1 {
			var report doctorReport
			if err := json.Unmarshal([]byte(out), &report); err != nil {
				t.Fatal(err)
			}
			if !errors.Is(doctorGateFailure(report, doctorFailOnError), errDoctorGate) {
				t.Fatal("old layout was not a brain error")
			}
		}
	}
	assertLegacyBrainUnchanged(t, brain, before)
}

func TestLegacyBrainBundleImportProtectsOldDestination(t *testing.T) {
	opts, _, brain, _ := newGraphStoreFixture(t)
	before := seedLegacyBrainLayout(t, brain)
	archive := filepath.Join(t.TempDir(), "current.tar")
	writeTestBundle(t, archive, map[string]string{
		bmanifest.FileName: `{"schema_version":4,"repo_key":"gh/example/repo","sources":{}}`,
		semstore.TraceFile: `[{"from_id":"replacement"}]`,
	})
	err := runSemanticBundleImport(context.Background(), &cobra.Command{}, opts, archive, bundleSHA256(t, archive), true)
	if !errors.Is(err, errLegacyBrainLayout) {
		t.Fatalf("expected old-layout error: %v", err)
	}
	assertLegacyBrainUnchanged(t, brain, before)
}

func TestLegacyBundleImportRefusesBeforeChangingDestination(t *testing.T) {
	for _, withManifest := range []bool{true, false} {
		t.Run(map[bool]string{true: "old manifest", false: "orphan store"}[withManifest], func(t *testing.T) {
			opts, _, brain, _ := newGraphStoreFixture(t)
			observations := []semstore.TraceObservation{{FromID: "keep me"}}
			if err := writeGraphTraces(brain, observations); err != nil {
				t.Fatal(err)
			}
			manifest := `{"schema_version":4,"repo_key":"gh/example/repo","sources":{}}`
			if withManifest {
				manifest = `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"store_path":"semantic/generations/old/semantic.sqlite"}}}`
			}
			archive := filepath.Join(t.TempDir(), "old.tar")
			writeTestBundle(t, archive, map[string]string{bmanifest.FileName: manifest, "semantic/generations/old/semantic.sqlite": "authored legacy traces"})
			err := runSemanticBundleImport(context.Background(), &cobra.Command{}, opts, archive, bundleSHA256(t, archive), true)
			if !errors.Is(err, errLegacyBrainLayout) || !strings.Contains(err.Error(), "export it again") {
				t.Fatalf("missing bundle recovery error: %v", err)
			}
			rows, err := semstore.ReadTraces(brain)
			if err != nil || len(rows) != 1 || rows[0] != observations[0] {
				t.Fatalf("changed destination traces: %+v, %v", rows, err)
			}
			if _, err := os.Stat(filepath.Join(brain, bmanifest.FileName)); !os.IsNotExist(err) {
				t.Fatalf("published a manifest: %v", err)
			}
		})
	}
}
