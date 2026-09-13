package cli

import (
	"path"
	"strings"
	"testing"
	"time"
)

// privacyPathHostileIDs are the session ids that turn a filename-construction
// bug into an outage. The colon cases are the ones that mattered: this tool's
// own id vocabulary prints them, so an operator pasting an id it emitted was
// enough to make `privacy exclude` and `privacy purge` fail outright on
// Windows.
var privacyPathHostileIDs = []string{
	"session:totally-made-up-12345",
	"conversation-session:8b1c0027",
	"fact:8932cffc31492ddfd72dae32",
	"history:abc/def",
	`back\slash`,
	`star*and?question`,
	`quote"lt<gt>pipe|`,
	"trailing ",
	"trailing.",
	"with%percent",
	"CON", "con", "NUL", "Com1", "LPT9",
	"73b0b938-93e4-4771-901e-43227e8b904b",
}

// TestPrivacyTransactionPathIsAFilenameOnEveryPlatform pins the invariant the
// escaping exists to provide.
//
// It is written as a property of the path rather than as a Windows test on
// purpose: the bug only *fired* on Windows (ERROR_INVALID_PARAMETER, "The
// parameter is incorrect"), but the defect is that a filename was built with
// url.PathEscape, which is specified against URL path segments and therefore
// leaves a colon alone. Asserting the property makes the check run on every
// platform in CI instead of only on the two shards that caught it.
func TestPrivacyTransactionPathIsAFilenameOnEveryPlatform(t *testing.T) {
	for _, id := range privacyPathHostileIDs {
		rel := privacyTransactionRel(id)
		if !strings.HasPrefix(rel, historyDirName+"/privacy/") {
			t.Fatalf("privacyTransactionRel(%q) = %q, want it under %s/privacy/", id, rel, historyDirName)
		}
		name := path.Base(rel)
		// The characters no Windows filesystem accepts in a name. The colon is
		// the one url.PathEscape let through, because `a:b` is a legal URL path
		// segment and an NTFS alternate data stream.
		if idx := strings.IndexAny(name, `<>:"/\|?*`); idx >= 0 {
			t.Fatalf("privacyTransactionRel(%q) produced filename %q containing the illegal character %q", id, name, name[idx])
		}
		for _, r := range name {
			if r < 0x20 || r == 0x7f {
				t.Fatalf("privacyTransactionRel(%q) produced filename %q containing a control byte %q", id, name, r)
			}
		}
		// Windows silently strips a trailing dot or space, so a name that ends
		// in one does not round-trip to the file that was written.
		if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
			t.Fatalf("privacyTransactionRel(%q) produced filename %q ending in a dot or space", id, name)
		}
		stem := strings.TrimSuffix(name, ".json")
		if stem == name {
			t.Fatalf("privacyTransactionRel(%q) produced filename %q without the .json suffix", id, name)
		}
		// A reserved device name is reserved WITH an extension too: CON.json is
		// the console, not a file.
		if windowsReservedDeviceNames[strings.ToUpper(stem)] {
			t.Fatalf("privacyTransactionRel(%q) produced reserved device name %q", id, name)
		}
	}
}

// TestPrivacyTransactionPathIsInjective is the property that makes the escaping
// safe to change: two different sessions must never share a transaction record,
// or excluding one would overwrite the durable receipt of the other.
func TestPrivacyTransactionPathIsInjective(t *testing.T) {
	seen := map[string]string{}
	for _, id := range append(append([]string(nil), privacyPathHostileIDs...),
		"a:b", "a%3Ab", "a/b", "a%2Fb", "CON", "%43ON", "secret-sess", "clean-sess") {
		rel := privacyTransactionRel(id)
		if other, ok := seen[rel]; ok && other != id {
			t.Fatalf("session ids %q and %q both map to %q", other, id, rel)
		}
		seen[rel] = id
	}
}

// TestPrivacyTransactionPathIsUnchangedForCapturedSessionIDs pins the migration
// claim in escapePrivacyTransactionID: captured session ids are UUIDs, none of
// their characters were ever escaped, and the fix must not rename a single
// record that exists on a real brain.
func TestPrivacyTransactionPathIsUnchangedForCapturedSessionIDs(t *testing.T) {
	for _, id := range []string{
		"73b0b938-93e4-4771-901e-43227e8b904b",
		"ea0eb368-4dcc-4d88-ae73-6ef84e81dba9",
		"secret-sess",
		"clean-sess",
	} {
		want := historyDirName + "/privacy/" + id + ".json"
		if got := privacyTransactionRel(id); got != want {
			t.Fatalf("privacyTransactionRel(%q) = %q, want the pre-fix name %q (no existing record may be renamed)", id, got, want)
		}
	}
}

// TestPurgeEvictsDistillCacheForColonSessionID guards the trap that sits right
// next to the filename fix.
//
// Three places escape a session id for the distill cache --
// distillSessionCacheKey writes the key, purgeDistillCacheEntries matches it by
// suffix to evict, and verifySessionPrivacy matches it again to report
// leftovers -- and all three use url.PathEscape. That is CORRECT there: the
// result is a JSON map key, not a filename, so a colon is harmless. But the
// three only work because they agree, and the filename fix next door is exactly
// the kind of change someone extends into one of them "for consistency". Do
// that to one and purge silently stops evicting while verify starts failing.
//
// A colon id is the case that would diverge first, so this pins it end to end.
func TestPurgeEvictsDistillCacheForColonSessionID(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	now := time.Date(2026, 8, 8, 11, 0, 0, 0, time.UTC)
	const colonID = "session:colon-cache-id"

	cache := loadDistillCache(brainDir)
	if cache.Sessions == nil {
		cache.Sessions = map[string]string{}
	}
	cache.Sessions[distillSessionCacheKey("main", colonID)] = "sha256:aaa"
	cache.Sessions[distillSessionCacheKey("main", "kept-sess")] = "sha256:bbb"
	if err := saveDistillCache(brainDir, cache); err != nil {
		t.Fatal(err)
	}

	if err := withBrainWriteLock(brainDir, func() error {
		plan, planErr := buildSessionPurgePlan(brainDir, colonID)
		if planErr != nil {
			return planErr
		}
		return executeSessionCleanup(brainDir, colonID, plan, now, "", true)
	}); err != nil {
		t.Fatalf("exclude with a colon id: %v", err)
	}

	after := loadDistillCache(brainDir)
	if _, ok := after.Sessions[distillSessionCacheKey("main", colonID)]; ok {
		t.Fatalf("purge did not evict the distill-cache entry for %q: %+v", colonID, after.Sessions)
	}
	if _, ok := after.Sessions[distillSessionCacheKey("main", "kept-sess")]; !ok {
		t.Fatalf("purge evicted an unrelated distill-cache entry: %+v", after.Sessions)
	}
	report, err := verifySessionPrivacy(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Clean {
		t.Fatalf("verification must be clean after excluding a colon id: %+v", report.Findings)
	}
}
