package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/agentsetup"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

// Only aggregate shape information comes from the cohort. Every value below is
// synthetic. Session metadata is deliberately repeated through the production
// session aliases, as it is in retained manifests.
func TestBrainManifestObservedShapesRoundTrip(t *testing.T) {
	// The observed cases divide every dimension by `scale` in the default run:
	// the shapes, aliasing, padding math, and round-trip checks are identical,
	// only the soak at recorded cohort sizes is lost. The above-read-cap
	// relation stays proven by the third case, which lowers the cap instead.
	// The nightly runs with ENTIRE_BRAIN_OBSERVED_SCALE=1 for the true sizes.
	scale := 8
	if observedScale() {
		scale = 1
	}
	for _, tc := range []struct {
		name                                string
		branches, sessions, warnings, bytes int
		readCap                             int64 // when non-zero, lower defaultMaxReadBytes to this
	}{
		{"session-heavy", 870 / scale, 5305 / scale, 111 / scale, 18569433 / scale, 0},
		{"warning-heavy", 389 / scale, 2633 / scale, 6238 / scale, 17220291 / scale, 0},
		// Guards against a manifest reader ever routed through the capped
		// default read path; only the fixture-above-cap relation matters.
		{"larger-than-default-read-limit", 20, 200, 20, 5 << 20, 4 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.readCap > 0 {
				if int64(tc.bytes) <= tc.readCap {
					t.Fatalf("fixture of %d bytes does not exceed the lowered read cap %d", tc.bytes, tc.readCap)
				}
				prev := base.DefaultMaxReadBytes
				base.DefaultMaxReadBytes = tc.readCap
				t.Cleanup(func() { base.DefaultMaxReadBytes = prev })
			}
			repo := t.TempDir()
			key, err := localRepoStorageKey(repo)
			if err != nil {
				t.Fatal(err)
			}
			dataDir := t.TempDir()
			dir := filepath.Join(dataDir, "repos", filepath.FromSlash(key))
			source := &sessionSourceManifest{}
			for i := 0; i < tc.branches; i++ {
				source.Branches = append(source.Branches, exportBranch{Branch: fmt.Sprintf("branch-%d", i), Directory: fmt.Sprintf("sessions/branch-%d", i), SessionCount: 1})
			}
			for i := 0; i < tc.sessions; i++ {
				source.Sessions = append(source.Sessions, exportSession{SessionID: fmt.Sprintf("session-%d", i), Branch: "main", LatestCheckpoint: fmt.Sprintf("%012x", i), TranscriptPath: fmt.Sprintf("sessions/main/session-%d.jsonl", i), Summary: &checkpointSummary{Intent: strings.Repeat("synthetic intent ", 30), Outcome: "synthetic outcome"}, FilesTouched: []string{"internal/example.go"}})
			}
			for i := 0; i < tc.warnings; i++ {
				source.Warnings = append(source.Warnings, fmt.Sprintf("synthetic checkpoint coverage warning %d", i))
			}
			manifest := exportManifest{SchemaVersion: bmanifest.SchemaVersion, RepoKey: key, Sources: &brainSources{Sessions: source, Seed: &seedSourceManifest{}, Semantic: &semanticSourceManifest{}, History: &historySourceManifest{}, Docs: &docSourceManifest{}}}
			if tc.name != "warning-heavy" {
				manifest.Sources.Patterns = &patternSourceManifest{}
			}
			applySessionSourceAliases(&manifest)
			// Distribute synthetic metadata across session summaries; only a small
			// remainder goes in a known scalar to match exact observed byte sizes.
			manifest.BrainVersion = "x"
			bmanifest.Normalize(&manifest)
			data, err := json.MarshalIndent(manifest, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			padding := tc.bytes - len(data) - 1
			if padding < 0 {
				t.Fatalf("fixture already exceeds target by %d", -padding)
			}
			perSession := padding / (2 * tc.sessions)
			for i := range source.Sessions {
				source.Sessions[i].Summary.Outcome += strings.Repeat("x", perSession)
			}
			manifest.BrainVersion += strings.Repeat("x", padding%(2*tc.sessions))
			if err := writeBrainManifestAndReadme(dir, manifest); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(filepath.Join(dir, bmanifest.FileName))
			if err != nil || info.Size() != int64(tc.bytes) {
				t.Fatalf("size: %v, %v", info, err)
			}
			for _, reader := range []func(string) (*exportManifest, error){bmanifest.Load, bmanifest.LoadForReplace} {
				got, err := reader(dir)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, &manifest) {
					t.Fatal("manifest records changed during round trip")
				}
			}
			got, health, issue := inspectBrainManifestHealth(dir)
			if issue != nil || health.State != "current" || len(got.Sessions) != tc.sessions {
				t.Fatalf("health failed: %+v", health)
			}
			// A stored manifest must not activate Brain agent guidance.
			setupOpts := agentsetup.Options{DataDir: dataDir, StateDir: t.TempDir(), ConfigDir: t.TempDir(), ListPlugins: func() (string, error) {
				return "Managed plugin directory: /fixture\n\n  brain v1.0.0 → /fixture/entire-brain\n  graph v1.0.0 → /fixture/entire-graph\n", nil
			}}
			render := func() (string, error) { return agentsetup.Preview(repo, "graph", setupOpts) }
			guide, err := render()
			if err != nil || notGraphOnly(guide) {
				t.Fatalf("manifest affected agent activation: %v", err)
			}
			if err := agentsetup.Install(repo, render, io.Discard); err != nil {
				t.Fatal(err)
			}
			// Exercise replacement's strict read of the existing oversized manifest.
			if err := writeBrainManifestAndReadme(dir, *got); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(dir, bmanifest.FileName))
			if err != nil {
				t.Fatal(err)
			}
			got.GeneratedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			if err := writeBrainManifestAndReadme(dir, *got); err == nil {
				t.Fatal("invalid time must fail encoding")
			}
			after, err := os.ReadFile(filepath.Join(dir, bmanifest.FileName))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("failed operation changed previous manifest")
			}
			if _, err := bmanifest.Load(dir); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBrainManifestDecoderCompatibility(t *testing.T) {
	for _, tc := range []struct{ name, body, code string }{
		{"v1", `{"schema_version":1}`, ""},
		{"v2", `{"schema_version":2}`, ""},
		{"v3", `{"schema_version":3}`, ""},
		{"unknown", `{"schema_version":3,"future":{"nested":true}}`, ""},
		{"empty", ``, brainstore.StateCorruptCode},
		{"malformed", `{"schema_version":3, broken}`, brainstore.StateCorruptCode},
		{"truncated", `{"schema_version":3,"sessions":[`, brainstore.StateCorruptCode},
		{"trailing-json", `{"schema_version":3} {}`, brainstore.StateCorruptCode},
		{"trailing-garbage", `{"schema_version":3} garbage`, brainstore.StateCorruptCode},
		{"missing-version", `{}`, brainstore.UnsupportedVersionCode},
		{"v4", `{"schema_version":4}`, ""},
		// Derived, not a literal: a hardcoded "newer" value stops meaning
		// "newer than supported" the moment bmanifest.SchemaVersion catches
		// up to it, and silently starts testing the wrong thing instead of
		// failing.
		{"newer", fmt.Sprintf(`{"schema_version":%d,"future":true}`, bmanifest.SchemaVersion+1), brainstore.UnsupportedVersionCode},
		{"older", `{"schema_version":-1}`, brainstore.UnsupportedVersionCode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeTestManifest(t, tc.body)
			_, err := bmanifest.Load(dir)
			_, health, issue := inspectBrainManifestHealth(dir)
			if tc.code == "" {
				if err != nil || issue != nil || health.State != "current" {
					t.Fatalf("read=%v health=%+v", err, health)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tc.code) || issue == nil || health.ErrorCode != tc.code {
					t.Fatalf("read=%v health=%+v", err, health)
				}
				if err := writeBrainManifestAndReadme(dir, exportManifest{}); err == nil {
					t.Fatal("writer must refuse invalid existing state")
				}
				after, _ := os.ReadFile(filepath.Join(dir, bmanifest.FileName))
				if string(after) != tc.body {
					t.Fatal("failed rewrite changed original")
				}
			}
		})
	}
}

func TestBrainManifestReaderKeepsFileProtections(t *testing.T) {
	for _, kind := range []string{"directory", "symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, bmanifest.FileName)
			target := filepath.Join(t.TempDir(), "target.json")
			if err := os.WriteFile(target, []byte(`{"schema_version":3}`), 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "directory":
				err = os.Mkdir(path, 0700)
			case "symlink":
				err = os.Symlink(target, path)
			case "hardlink":
				err = os.Link(target, path)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := bmanifest.Load(dir); err == nil || !strings.Contains(err.Error(), brainstore.StateUnsafeCode) {
				t.Fatalf("unsafe read: %v", err)
			}
			_, health, _ := inspectBrainManifestHealth(dir)
			if health.State != "unsafe" {
				t.Fatalf("health: %+v", health)
			}
		})
	}
}

func TestBrainManifestLargeUnknownFieldsRemainReadOnly(t *testing.T) {
	// The unknown-field payload must exceed maxManifestBytes so the bounded
	// reader below still refuses it; lowering the bound keeps that property
	// without megabytes of race-instrumented JSON per decode pass.
	prev := base.MaxManifestBytes
	base.MaxManifestBytes = 64 << 10
	t.Cleanup(func() { base.MaxManifestBytes = prev })
	dir := writeTestManifest(t, `{"schema_version":3,"sessions":[],"future":"`+strings.Repeat("x", 128<<10)+`"}`)
	_, dropped, err := bmanifest.Read(dir, true)
	if err != nil || !dropped {
		t.Fatalf("tolerant read: dropped=%v err=%v", dropped, err)
	}
	path := filepath.Join(dir, bmanifest.FileName)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeBrainManifestAndReadme(dir, exportManifest{}); err == nil {
		t.Fatal("writer erased unknown fields")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("unknown fields were changed")
	}
	if _, err := base.SafeReadFile(path, base.MaxManifestBytes); err == nil {
		t.Fatal("unrelated bounded reader lost its limit")
	}
	// Explicit forced-refresh cleanup shares the dedicated decoder, while still
	// using the checked remover. This only removes synthetic test state.
	removed, err := discardManifestThisBuildCannotRewrite(dir)
	if err != nil || !removed {
		t.Fatalf("checked cleanup: removed=%v err=%v", removed, err)
	}
}
