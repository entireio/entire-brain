package brainstore

import (
	"fmt"
	"path/filepath"
	"sort"
)

// RetrievalPrivacyPolicy is the checked tombstone generation under which a
// response was assembled. Keeping the brain path beside the identity lets the
// same final writer cover single-repository, workspace, get, and brief output.
type RetrievalPrivacyPolicy struct {
	BrainDir string
	Identity string
	// IssueIdentity pins the adapter-owned issue-evidence identity a response
	// was assembled under. brainstore carries it beside the tombstone identity
	// and does not interpret it; the CLI adapter revalidates it under the lock.
	IssueIdentity       string
	RequireDerivedClean bool
}

// CaptureRetrievalPrivacyPolicy records the checked tombstone identity for one
// Brain. Loading the full guard, not just the tombstone file, keeps capture
// failing on the same invalid durable-transaction state every read would.
func CaptureRetrievalPrivacyPolicy(brainDir string) (RetrievalPrivacyPolicy, error) {
	guard, err := LoadSessionReadGuard(brainDir, nil)
	if err != nil {
		return RetrievalPrivacyPolicy{}, err
	}
	return RetrievalPrivacyPolicy{BrainDir: brainDir, Identity: guard.PolicyIdentity}, nil
}

// RequireRetrievalPrivacyPolicyIdentity fails when the Brain's checked
// tombstone identity no longer matches the identity a response was assembled
// under.
func RequireRetrievalPrivacyPolicyIdentity(brainDir, expected string) error {
	guard, err := LoadSessionReadGuard(brainDir, nil)
	if err != nil {
		return err
	}
	if guard.PolicyIdentity != expected {
		return fmt.Errorf("%s: retrieval privacy policy changed during response assembly; retry the request", PrivacyDirtyCode)
	}
	return nil
}

// WithLockedRetrievalPrivacyPolicies is the response linearization boundary.
// Tombstone mutations use the same per-Brain write lock. Locks are acquired in
// deterministic path order so a workspace response spanning several Brains
// cannot deadlock with another workspace response containing the same members.
// requireDerivedClean is the caller's derived-read gate, enforced for every
// policy that demands it while the locks are held.
func WithLockedRetrievalPrivacyPolicies(policies []RetrievalPrivacyPolicy, requireDerivedClean func(string) error, fn func() error) error {
	unlock, err := AcquireRetrievalPrivacyPolicies(policies, requireDerivedClean)
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}

func AcquireRetrievalPrivacyPolicies(policies []RetrievalPrivacyPolicy, requireDerivedClean func(string) error) (func(), error) {
	byBrain := make(map[string]string, len(policies))
	derivedClean := make(map[string]bool, len(policies))
	for _, policy := range policies {
		if policy.BrainDir == "" {
			continue
		}
		brainDir := filepath.Clean(policy.BrainDir)
		if prior, exists := byBrain[brainDir]; exists && prior != policy.Identity {
			return nil, fmt.Errorf("%s: response was assembled under conflicting privacy policies", PrivacyDirtyCode)
		}
		byBrain[brainDir] = policy.Identity
		derivedClean[brainDir] = derivedClean[brainDir] || policy.RequireDerivedClean
	}
	brainDirs := make([]string, 0, len(byBrain))
	for brainDir := range byBrain {
		brainDirs = append(brainDirs, brainDir)
	}
	sort.Strings(brainDirs)
	unlocks := make([]func(), 0, len(brainDirs))
	for _, brainDir := range brainDirs {
		unlock, err := AcquireBrainWriteLock(brainDir)
		if err != nil {
			for i := len(unlocks) - 1; i >= 0; i-- {
				unlocks[i]()
			}
			return nil, err
		}
		unlocks = append(unlocks, unlock)
	}
	release := func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}
	for _, brainDir := range brainDirs {
		if err := RequireRetrievalPrivacyPolicyIdentity(brainDir, byBrain[brainDir]); err != nil {
			release()
			return nil, err
		}
		if derivedClean[brainDir] {
			if err := requireDerivedClean(brainDir); err != nil {
				release()
				return nil, err
			}
		}
	}
	return release, nil
}
