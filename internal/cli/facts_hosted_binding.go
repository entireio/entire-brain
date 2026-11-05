package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
)

type hostedFactsBinding struct {
	RepoID  string `json:"repo_id"`
	BaseURL string `json:"base_url"`
}

// hostedFactsBindingPath is the repo-level fact-sync marker. It recorded a
// per-branch sync before the store went flat; readHostedFactsBinding still
// falls back to the legacy per-branch locations so an upgraded brain keeps
// its binding without re-running `memories sync`.
func hostedFactsBindingPath() string {
	return filepath.ToSlash(filepath.Join(factstore.DirName, "hosted-target.json"))
}

func writeHostedFactsBinding(brainDir, repoID, baseURL string) error {
	return writeHostedFactsBindingContext(context.Background(), brainDir, repoID, baseURL)
}

func writeHostedFactsBindingContext(ctx context.Context, brainDir, repoID, baseURL string) error {
	binding := hostedFactsBinding{RepoID: repoID, BaseURL: strings.TrimRight(baseURL, "/")}
	data, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	return brainstore.WithBrainWriteLockContext(ctx, brainDir, func() error {
		return brainstore.WriteBrainRelativeFileAtomic(brainDir, hostedFactsBindingPath(), append(data, '\n'), 0600)
	})
}

// readHostedFactsBinding reads the fact-sync marker. The error return is
// reserved for a marker that EXISTS but cannot be trusted (unreadable,
// corrupt, or ambiguous); an absent marker is (zero, false, nil). When the
// flat marker is absent, EVERY legacy per-branch marker (live or parked by
// the store migration) is consulted: a unanimous target is adopted (and
// promoted to the flat marker, best-effort), but markers naming different
// targets are an ambiguity the flat store must not resolve silently — the
// repo-wide sync would ship every branch's memories to whichever marker
// happened to sort first.
func readHostedFactsBinding(brainDir string) (hostedFactsBinding, bool, error) {
	binding, present, err := readHostedFactsBindingAt(brainDir, hostedFactsBindingPath())
	if err != nil || present {
		return binding, present, err
	}
	var adopted *hostedFactsBinding
	for _, rel := range legacyHostedFactsBindingPaths(brainDir) {
		legacy, present, err := readHostedFactsBindingAt(brainDir, rel)
		if err != nil {
			return hostedFactsBinding{}, false, err
		}
		if !present {
			continue
		}
		if adopted == nil {
			copied := legacy
			adopted = &copied
			continue
		}
		if *adopted != legacy {
			return hostedFactsBinding{}, false, fmt.Errorf(
				"legacy hosted fact bindings disagree (%s at %s vs %s at %s); run 'memories sync' with the intended target to bind this repository",
				adopted.RepoID, adopted.BaseURL, legacy.RepoID, legacy.BaseURL)
		}
	}
	if adopted == nil {
		return hostedFactsBinding{}, false, nil
	}
	// Best-effort promotion: the next read finds the flat marker directly.
	_ = writeHostedFactsBinding(brainDir, adopted.RepoID, adopted.BaseURL)
	return *adopted, true, nil
}

func readHostedFactsBindingAt(brainDir, rel string) (hostedFactsBinding, bool, error) {
	var binding hostedFactsBinding
	data, present, err := brainstore.ReadMemoryStateFile(brainDir, rel, "hosted fact binding", base.DefaultMaxReadBytes)
	if err != nil || !present {
		return binding, false, err
	}
	if err := json.Unmarshal(data, &binding); err != nil {
		return binding, false, fmt.Errorf("invalid hosted fact binding: %w", err)
	}
	return binding, true, nil
}

// legacyHostedFactsBindingPaths enumerates per-branch marker locations from
// the pre-flat layout: live branch dirs (a brain not yet migrated) and dirs
// the migration parked under facts/.legacy-<ts>/. Sorted for determinism.
func legacyHostedFactsBindingPaths(brainDir string) []string {
	var rels []string
	appendDir := func(rel string) {
		entries, err := os.ReadDir(filepath.Join(brainDir, filepath.FromSlash(rel)))
		if err != nil {
			return
		}
		for _, entry := range entries {
			if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			marker := filepath.ToSlash(filepath.Join(rel, entry.Name(), "hosted-target.json"))
			if info, statErr := os.Lstat(filepath.Join(brainDir, filepath.FromSlash(marker))); statErr == nil && info.Mode().IsRegular() {
				rels = append(rels, marker)
			}
		}
	}
	appendDir(factstore.DirName)
	parkRoot := filepath.Join(brainDir, factstore.DirName)
	if entries, err := os.ReadDir(parkRoot); err == nil {
		for _, entry := range entries {
			if entry.IsDir() && strings.HasPrefix(entry.Name(), ".legacy-") {
				appendDir(filepath.ToSlash(filepath.Join(factstore.DirName, entry.Name())))
			}
		}
	}
	sort.Strings(rels)
	return rels
}

func checkHostedFactsBinding(brainDir, repoID, baseURL string) error {
	binding, present, err := readHostedFactsBinding(brainDir)
	if err != nil {
		return err
	}
	if !present || binding.RepoID != repoID || binding.BaseURL != strings.TrimRight(baseURL, "/") {
		return fmt.Errorf("local facts are not bound to this hosted repository and API; run 'memories sync' with the intended target first")
	}
	return nil
}
