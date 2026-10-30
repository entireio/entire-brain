package brainstore

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
)

// HostedRepoBinding is the repo-level hosted-brain connection state, written
// by `entire brain connect` and `entire brain disconnect`. Only non-secrets
// live here; tokens are minted per call (see mintHostedToken). The per-branch
// hosted-target.json (hostedFactsBinding) records which branches have synced.
//
// OptedOut records an explicit `disconnect`: no sync until
// `connect` rewrites the binding. The coordinates are kept through an opt-out
// so re-connecting works without placement resolution. Disconnect never purges
// memories already pulled into the local store.
//
// NoAccessUntil pauses sync after the server denied access to the bound repo
// (deleted, or this caller lost its grant — COR-2279): background triggers
// skip the repo until it passes. `entire brain connect` clears it by writing
// a fresh binding.
type HostedRepoBinding struct {
	RepoID        string    `json:"repo_id"`
	BaseURL       string    `json:"base_url"`
	OptedOut      bool      `json:"opted_out,omitempty"`
	NoAccessUntil time.Time `json:"no_access_until,omitzero"`
}

const HostedRepoBindingFile = "hosted.json"

func WriteHostedRepoBinding(brainDir string, binding HostedRepoBinding) error {
	return WithBrainWriteLock(brainDir, func() error {
		return writeHostedRepoBindingLocked(brainDir, binding)
	})
}

// writeHostedRepoBindingLocked is the write half for callers that already hold
// the brain write lock (it is not reentrant).
func writeHostedRepoBindingLocked(brainDir string, binding HostedRepoBinding) error {
	binding.BaseURL = strings.TrimRight(binding.BaseURL, "/")
	data, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	return WriteBrainRelativeFileAtomic(brainDir, HostedRepoBindingFile, append(data, '\n'), 0600)
}

// SetHostedOptOut records an explicit disconnect as one locked
// read-modify-write, so a concurrent writer cannot interleave between reading
// the coordinates and marking them opted out. A corrupt state file must not
// block the opt-out — the user's intent is unambiguous — so it is replaced by
// a bare one. Returns the binding as written.
func SetHostedOptOut(brainDir string) (HostedRepoBinding, error) {
	var binding HostedRepoBinding
	err := WithBrainWriteLock(brainDir, func() error {
		existing, _, readErr := ReadHostedRepoBinding(brainDir)
		if readErr != nil {
			existing = HostedRepoBinding{}
		}
		existing.OptedOut = true
		binding = existing
		return writeHostedRepoBindingLocked(brainDir, existing)
	})
	return binding, err
}

// MutateHostedRepoBinding applies update to the current binding as one locked
// read-modify-write, so a concurrent connect/disconnect cannot interleave
// between the caller's earlier read and this write. update returns the new
// binding and whether to write it. An absent or unreadable binding is left
// untouched — this never creates, repairs, or overwrites consent state it
// could not read (the file may record an opt-out).
func MutateHostedRepoBinding(brainDir string, update func(HostedRepoBinding) (HostedRepoBinding, bool)) error {
	return WithBrainWriteLock(brainDir, func() error {
		current, present, err := ReadHostedRepoBinding(brainDir)
		if err != nil || !present {
			return err
		}
		next, write := update(current)
		if !write {
			return nil
		}
		return writeHostedRepoBindingLocked(brainDir, next)
	})
}

func ReadHostedRepoBinding(brainDir string) (HostedRepoBinding, bool, error) {
	var binding HostedRepoBinding
	data, present, err := ReadMemoryStateFile(brainDir, HostedRepoBindingFile, "hosted repo binding", base.DefaultMaxReadBytes)
	if err != nil || !present {
		return binding, false, err
	}
	if err := json.Unmarshal(data, &binding); err != nil {
		return binding, false, fmt.Errorf("invalid hosted repo binding: %w", err)
	}
	binding.BaseURL = strings.TrimRight(binding.BaseURL, "/")
	return binding, true, nil
}
