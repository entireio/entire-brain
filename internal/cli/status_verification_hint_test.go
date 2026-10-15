package cli

import (
	"strings"
	"testing"
)

// Fact-anchor verification moved behind --details because it walks git history
// per anchor commit (24s to 2.4s on a 961-fact brain). Moving it is right;
// letting the line vanish without a word is not. A reader who saw
// "verification: ..." yesterday and sees nothing today has been told that
// verification passed, which is the exact silent-degradation class this change
// set exists to remove.
func TestStatusSaysWhereVerificationWentWhenItIsNotRun(t *testing.T) {
	t.Parallel()

	report := statusFixtureReport(t, 44)
	report.Facts = &brainStatusFacts{Facts: 12, Verification: nil}

	got := renderStatus(t, report, true)
	if !strings.Contains(got, "verification:") {
		t.Fatalf("status with facts and no verification must still name verification; got:\n%s", got)
	}
	if !strings.Contains(got, "--details") {
		t.Fatalf("the line must point at the flag that runs it; got:\n%s", got)
	}
}

// With no facts there is nothing to verify, so the hint would be noise. A
// notice everyone learns to ignore is worse than no notice.
func TestStatusStaysQuietAboutVerificationWithNoFacts(t *testing.T) {
	t.Parallel()

	report := statusFixtureReport(t, 44)
	report.Facts = &brainStatusFacts{Facts: 0}

	if got := renderStatus(t, report, true); strings.Contains(got, "verification:") {
		t.Fatalf("a brain with no facts must not advertise verification; got:\n%s", got)
	}
}
