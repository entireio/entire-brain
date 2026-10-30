package cli

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
)

type hostedFactsBinding struct {
	RepoID  string `json:"repo_id"`
	BaseURL string `json:"base_url"`
}

func hostedFactsBindingPath(branch string) string {
	return filepath.ToSlash(filepath.Join(factstore.BranchRelDir(branch), "hosted-target.json"))
}

func writeHostedFactsBinding(brainDir, branch, repoID, baseURL string) error {
	binding := hostedFactsBinding{RepoID: repoID, BaseURL: strings.TrimRight(baseURL, "/")}
	data, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	return brainstore.WithBrainWriteLock(brainDir, func() error {
		return brainstore.WriteBrainRelativeFileAtomic(brainDir, hostedFactsBindingPath(branch), append(data, '\n'), 0600)
	})
}

// readHostedFactsBinding reads a branch's sync marker. The error return is
// reserved for a marker that EXISTS but cannot be trusted (unreadable or
// corrupt); an absent marker is (zero, false, nil).
func readHostedFactsBinding(brainDir, branch string) (hostedFactsBinding, bool, error) {
	var binding hostedFactsBinding
	data, present, err := brainstore.ReadMemoryStateFile(brainDir, hostedFactsBindingPath(branch), "hosted fact binding", base.DefaultMaxReadBytes)
	if err != nil || !present {
		return binding, false, err
	}
	if err := json.Unmarshal(data, &binding); err != nil {
		return binding, false, fmt.Errorf("invalid hosted fact binding: %w", err)
	}
	return binding, true, nil
}

func checkHostedFactsBinding(brainDir, branch, repoID, baseURL string) error {
	binding, present, err := readHostedFactsBinding(brainDir, branch)
	if err != nil {
		return err
	}
	if !present || binding.RepoID != repoID || binding.BaseURL != strings.TrimRight(baseURL, "/") {
		return fmt.Errorf("local facts are not bound to this hosted repository and API; run 'memories sync' with the intended target first")
	}
	return nil
}
