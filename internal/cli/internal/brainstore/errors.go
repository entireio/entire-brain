package brainstore

import "strings"

// PrivacyBusyCode is the stable code for contention on the privacy side-effect lock.
const PrivacyBusyCode = "memory_privacy_busy"

// PrivacyDirtyCode reports that excluded-session cleanup has not verified
// clean, so privacy-gated reads and response writes must fail closed.
const PrivacyDirtyCode = "memory_privacy_dirty"

// Stable storage failure codes shared by artifact readers and their callers.
const (
	StateCorruptCode = "memory_state_corrupt"
	StateUnsafeCode  = "memory_state_unsafe"
)

// UnsupportedVersionCode reports persisted state written at a schema version
// newer than this build supports; the bytes are read-only, not corrupt.
const UnsupportedVersionCode = "memory_unsupported_version"

// DirtyWorktreeCode prefixes the seed and semantic refusals to index
// uncommitted content. It is shared so the reporter that attaches the
// remedy (newSetupComponent) matches a code rather than either message's
// prose, which would let a reword drop the hint silently.
const DirtyWorktreeCode = "dirty_worktree"

// Stable memory-maintenance error codes. An empty result never stands in for
// any of these states.
const (
	LockBusyCode          = "memory_lock_busy"
	SourceStaleCode       = "memory_source_stale"
	CancelledCode         = "memory_cancelled"
	CancelTooLateCode     = "memory_cancel_too_late"
	MigrationRequiredCode = "memory_migration_required"
	QueryTooBroadCode     = "memory_query_too_broad"
	InputTooLargeCode     = "memory_input_too_large"
	PrivacyExcludedCode   = "memory_privacy_excluded"

	IdentityAmbiguousCode = "memory_identity_ambiguous"
	ProviderUnavailCode   = "memory_provider_unavailable"
	SessionInventoryCode  = "memory_session_inventory_degraded"
	// WorkerDegradedCode reports that the last worker pass completed its
	// projection lanes but could not export new sessions. It is a health issue,
	// not a crash: the Brain keeps answering from what it already has, while
	// nothing captured since that failure can reach it.
	WorkerDegradedCode = "memory_worker_degraded"
)

// MemoryErrorCode classifies an error by the first stable code its message
// carries, defaulting to StateCorruptCode.
func MemoryErrorCode(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	for _, code := range []string{
		SessionInventoryCode,
		StateUnsafeCode,
		UnsupportedVersionCode,
		MigrationRequiredCode,
		SourceStaleCode,
		CancelTooLateCode,
		CancelledCode,
		LockBusyCode,
		QueryTooBroadCode,
		InputTooLargeCode,
		PrivacyExcludedCode,
		PrivacyDirtyCode,
		PrivacyBusyCode,
		IdentityAmbiguousCode,
		ProviderUnavailCode,
		StateCorruptCode,
	} {
		if strings.Contains(message, code) {
			return code
		}
	}
	return StateCorruptCode
}
