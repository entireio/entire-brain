package cli

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

type hostedFactsBinding struct {
	RepoID  string `json:"repo_id"`
	BaseURL string `json:"base_url"`
}

func hostedFactsBindingPath(branch string) string {
	return filepath.ToSlash(filepath.Join(factsBranchRelDir(branch), "hosted-target.json"))
}

func writeHostedFactsBinding(brainDir, branch, repoID, baseURL string) error {
	binding := hostedFactsBinding{RepoID: repoID, BaseURL: strings.TrimRight(baseURL, "/")}
	data, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	return withBrainWriteLock(brainDir, func() error {
		return writeBrainRelativeFileAtomic(brainDir, hostedFactsBindingPath(branch), append(data, '\n'), 0600)
	})
}

func checkHostedFactsBinding(brainDir, branch, repoID, baseURL string) error {
	data, present, err := readMemoryStateFile(brainDir, hostedFactsBindingPath(branch), "hosted fact binding", defaultMaxReadBytes)
	if err != nil {
		return err
	}
	var binding hostedFactsBinding
	if present {
		if err := json.Unmarshal(data, &binding); err != nil {
			return fmt.Errorf("invalid hosted fact binding: %w", err)
		}
	}
	if !present || binding.RepoID != repoID || binding.BaseURL != strings.TrimRight(baseURL, "/") {
		return fmt.Errorf("local facts are not bound to this hosted repository and API; run 'facts sync' with the intended target before mirroring settlements")
	}
	return nil
}
