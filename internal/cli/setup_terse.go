package cli

import (
	"strings"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/setupui"
)

// setup_terse.go maps the setup report onto the short form.
//
// It is the only place that decides which short-form state a run landed in,
// so the states stay mutually exclusive and testable without a brain.

// terseCapabilityLabels turns internal component ids into words a person
// recognises. A component missing from this map is deliberately NOT reported
// in the short form: an id like "entities" tells a reader nothing they can act
// on, and `doctor` already lists every component by name.
var terseCapabilityLabels = map[string]string{
	brainComponentSemantic: "code context",
	brainComponentHistory:  "history search",
	brainComponentDocs:     "docs search",
	// Session export earns a label for one reason: its commonest failure is
	// not a failure. A repository where no agent session has finished yet
	// reports an incomplete scope and no readable IDs, which is exactly what
	// every repository looks like before its first session. Left unlabelled it
	// collapsed into "1 other component did not build -- doctor names them",
	// so the reader ran doctor to be told nothing was wrong. That round trip
	// is the referral loop this PR removes, and it has to be closed in the
	// SHORT form, because the short form is what they see.
	brainComponentSessions: "session history",
}

// terseCapabilityRemedies maps a missing capability to the one command that
// restores it, when a single command does.
var terseCapabilityRemedies = map[string]string{
	brainComponentSemantic: "run `entire plugin install graph`",
	// Not a command: there is nothing to run. The honest remedy for "no
	// captured sessions yet" is to work, which is why this one is a sentence.
	brainComponentSessions: "no captured sessions yet — they accrue as you work in this repo",
}

func terseReportFor(report setupReport, timings *setupTimings, queryable bool, opts setupCommandOptions, brainCmd string) setupui.TerseReport {
	out := setupui.TerseReport{
		Queryable: queryable,
		Sessions:  report.Facts.Sessions,
		Distilled: report.Facts.Distilled,
		Queued:    report.Facts.Queued,
		BrainCmd:  base.SetupBrainCommand(brainCmd),
	}
	if timings != nil {
		out.Elapsed = timings.total()
	}

	_, skipped, failed := partitionSetupComponents(report.Instant.Components)
	// Skipped components are capability gaps too: an installed Graph without
	// the store contract leaves code inspection unavailable just as surely as
	// a failed build would, and the short form is where that gets discovered.
	// Only labelled ones report, same as failures; a skip is never counted
	// into UnnamedFailures because it is not one.
	namedFailures := 0
	for _, component := range append(append([]setupComponent{}, skipped...), failed...) {
		if label, named := terseCapabilityLabels[component.Name]; named {
			out.Unavailable = append(out.Unavailable, label)
			if component.failed() {
				namedFailures++
			}
			if out.Remedy == "" {
				if hint := strings.TrimSpace(component.Hint); component.State == "skipped" && hint != "" {
					out.Remedy = hint
				} else if remedy, ok := terseCapabilityRemedies[component.Name]; ok {
					out.Remedy = remedy
				}
			}
		}
	}
	// A component with no reader-facing label is still a component that did
	// not build. Leaving Unavailable empty for it was not just a missing
	// line: UpToDate is computed from len(Unavailable), so a build that lost
	// `entities` collapsed to the one-line "Brain up to date" and said
	// nothing at all. The count is kept separately so the degraded build is
	// never mistaken for a clean one, and the names stay out of the short
	// form -- an id like "entities" is not something a reader can act on,
	// which is why it has no label in the first place.
	out.UnnamedFailures = len(failed) - namedFailures

	// Same read as the verbose Next block, same rule: not already bound, not
	// explicitly opted out. The short form is the DEFAULT, so this is where
	// the opt-in actually gets discovered.
	out.OfferConnect = setupOffersConnect(report.BrainPath)
	out.HostedError = report.HostedConnectError

	out.Distill = terseDistillOutcome(report)
	if out.Distill == setupui.DistillFailed {
		out.DistillReason = setupui.FlattenText(report.Backfill.Detail)
	}
	out.Watcher, out.WatcherReason = terseWatcherOutcome(report, opts)

	// A re-run that changed nothing collapses to one line. "Nothing to do"
	// means: usable, nothing left to distill, watcher up. Anything else still
	// owes the reader an explanation.
	out.UpToDate = queryable &&
		len(out.Unavailable) == 0 &&
		out.UnnamedFailures == 0 &&
		report.Facts.Sessions > 0 &&
		report.Facts.Pending() == 0 &&
		out.Watcher == setupui.WatcherAlreadyRunning
	return out
}

// terseDistillOutcome reads the FACTS PHASE, not the fact counts.
//
// It used to branch on report.Facts.Running and report.Facts.Agent. Neither is
// ever set: factsBackfillStatusForBrain fills only Sessions and Distilled from
// the manifest and the distill cache. So "started" and "declined" were
// unreachable and every run with pending sessions reported "no agent CLI to
// distill them" -- telling people to install a tool they had just watched the
// same command use. The phase result is the only thing that knows what
// actually happened.
func terseDistillOutcome(report setupReport) setupui.DistillOutcome {
	switch report.Backfill.State {
	case "ok":
		return setupui.DistillStarted
	case "failed":
		// Attempted and broke. Reporting this as "no agent CLI" would send
		// the reader to install a tool they already have.
		return setupui.DistillFailed
	}

	// Skipped. The typed consent reason says why; matching English against
	// Detail would break the first time a sentence is reworded.
	switch report.Backfill.consentWhy {
	case setupui.ConsentReasonNoSessions:
		return setupui.DistillNoSessions
	case setupui.ConsentReasonNoAgent:
		return setupui.DistillNoAgent
	case setupui.ConsentReasonNoBackfill:
		return setupui.DistillSkippedFlag
	case setupui.ConsentReasonDeclined, setupui.ConsentReasonNoTTY:
		return setupui.DistillDeclined

	// Skipped BEFORE consent was asked. These are not decisions and must not
	// be reported as one. The default below used to swallow all four and say
	// "Distillation skipped -- run `setup` again anytime", which contradicts
	// a backfill that is running and is unfollowable advice when the brain
	// is paused: the next setup skips for exactly the same reason.
	case setupui.ConsentReasonPaused:
		return setupui.DistillPaused
	case setupui.ConsentReasonAlreadyRunning:
		return setupui.DistillInFlight
	case setupui.ConsentReasonAlreadyDistilled:
		return setupui.DistillAlreadyDone
	case setupui.ConsentReasonNoExecutable:
		return setupui.DistillNoExecutable
	}

	// No reason recorded at all. Every skip in runSetupBackfill now carries
	// one, so this is reachable only from a report that never ran the phase
	// -- the empty-repo path, for one. Counts are the honest fallback there.
	switch {
	case report.Facts.Sessions == 0:
		return setupui.DistillNoSessions
	case report.Facts.Pending() == 0:
		return setupui.DistillAlreadyDone
	default:
		return setupui.DistillDeclined
	}
}

func terseWatcherOutcome(report setupReport, opts setupCommandOptions) (setupui.WatcherOutcome, string) {
	switch {
	case opts.noDaemon:
		return setupui.WatcherSkippedFlag, ""
	case report.Daemon.Installed && report.Daemon.Running:
		return setupui.WatcherAlreadyRunning, ""
	case report.Daemon.Installed:
		return setupui.WatcherInstalled, ""
	default:
		return setupui.WatcherUnavailable, setupui.FlattenText(report.Daemon.Detail)
	}
}
