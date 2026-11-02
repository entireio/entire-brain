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
	BrainDir            string
	Identity            string
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

// WithLockedRetrievalPrivacyPolicies is the mutation-side policy boundary.
// Tombstone mutations use the same per-Brain write lock. Locks are acquired in
// deterministic path order so a workspace operation spanning several Brains
// cannot deadlock with another workspace operation containing the same members.
// requireDerivedClean is the caller's derived-read gate, enforced for every
// policy that demands it while the locks are held.
//
// Read-response emission does NOT use this: it validates without locks (see
// ValidateRetrievalPrivacyPolicies). Only operations that mutate pattern/corpus
// state under the captured policies still take the locks.
func WithLockedRetrievalPrivacyPolicies(policies []RetrievalPrivacyPolicy, requireDerivedClean func(string) error, fn func() error) error {
	unlock, err := AcquireRetrievalPrivacyPolicies(policies, requireDerivedClean)
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}

// ValidateRetrievalPrivacyPolicies re-checks each policy identity and derived
// gate WITHOUT taking any lock. This is the read-response emission check: a
// response assembled under a policy that has since changed fails closed, but a
// tombstone landing after this check and before the output write is not
// blocked — it takes effect on the next command. That instant-suppression
// guarantee was deliberately retired: enforcing it meant acquiring the global
// brain write lock per response, so any long-running writer (promote, distill)
// failed every concurrent reader with brain_locked.
func ValidateRetrievalPrivacyPolicies(policies []RetrievalPrivacyPolicy, requireDerivedClean func(string) error) error {
	byBrain, derivedClean, brainDirs, err := normalizeRetrievalPrivacyPolicies(policies)
	if err != nil {
		return err
	}
	for _, brainDir := range brainDirs {
		if err := RequireRetrievalPrivacyPolicyIdentity(brainDir, byBrain[brainDir]); err != nil {
			return err
		}
		if derivedClean[brainDir] {
			if err := requireDerivedClean(brainDir); err != nil {
				return err
			}
		}
	}
	return nil
}

func normalizeRetrievalPrivacyPolicies(policies []RetrievalPrivacyPolicy) (map[string]string, map[string]bool, []string, error) {
	byBrain := make(map[string]string, len(policies))
	derivedClean := make(map[string]bool, len(policies))
	for _, policy := range policies {
		if policy.BrainDir == "" {
			continue
		}
		brainDir := filepath.Clean(policy.BrainDir)
		if prior, exists := byBrain[brainDir]; exists && prior != policy.Identity {
			return nil, nil, nil, fmt.Errorf("%s: response was assembled under conflicting privacy policies", PrivacyDirtyCode)
		}
		byBrain[brainDir] = policy.Identity
		derivedClean[brainDir] = derivedClean[brainDir] || policy.RequireDerivedClean
	}
	brainDirs := make([]string, 0, len(byBrain))
	for brainDir := range byBrain {
		brainDirs = append(brainDirs, brainDir)
	}
	sort.Strings(brainDirs)
	return byBrain, derivedClean, brainDirs, nil
}

func AcquireRetrievalPrivacyPolicies(policies []RetrievalPrivacyPolicy, requireDerivedClean func(string) error) (func(), error) {
	byBrain, derivedClean, brainDirs, err := normalizeRetrievalPrivacyPolicies(policies)
	if err != nil {
		return nil, err
	}
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
