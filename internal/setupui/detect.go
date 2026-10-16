package setupui

import (
	"strings"

	"github.com/entireio/entire-brain/internal/tui"
)

// detect.go renders phase 1. Its job is to make phase 4's spend prompt an
// INFORMED decision: a reader who is about to be asked "distill now?" should
// already know which agent would run, which model it would run, and whether the
// graph provider can even answer afterwards.
//
// The detection itself is the caller's. This file takes a filled struct and
// never shells out, which is what makes the block assertable without a repo,
// an agent CLI or a network.

// Remedy defaults. Every absent capability prints one of these unless the
// caller supplies a better one. The rule they enforce: a line that says
// something is missing and does not say what to run about it is a line the
// reader cannot act on, and this block has no room for those.
const (
	// RemedyAgent is what to do about no agent CLI on PATH.
	RemedyAgent = "install one, then re-run: entire brain setup"
	// RemedyProvider is what to do about no graph provider.
	RemedyProvider = "entire graph version"
	// RemedySemantic is what to do about a missing semantic index.
	RemedySemantic = "entire brain refresh"
	// RemedyEntities is what to do about a missing entity index.
	RemedyEntities = "entire brain refresh"
	// RemedyHosted is what to do about an unbound hosted brain.
	//
	// It is NOT "setup will bind it for you". `entire repo view` and
	// `entire repo whereami` do not exist, so setup cannot resolve a repo to a
	// hosted brain on its own (COR-2029). Printing an automatic binding that
	// cannot happen is worse than printing none.
	RemedyHosted = "entire brain connect"
)

// Capability is one yes/no fact with a remedy for the no.
type Capability struct {
	// OK is whether the capability is present.
	OK bool
	// Detail is an optional qualifier shown when present ("13.2k symbols").
	Detail string
	// Remedy is the command to run when absent. Empty takes the default.
	Remedy string
}

// RepoFacts is what was detected about the repository.
type RepoFacts struct {
	Name    string
	Path    string
	Commits int
	Dirty   bool
	// Detached reports a detached HEAD, which changes what "clean tree" means
	// to a reader deciding whether setup is looking at what they think it is.
	Detached bool
}

// AgentFacts is the coding agent that a distill would run. Name empty means
// none was found.
type AgentFacts struct {
	Name   string
	Model  string
	Effort string
	Remedy string
}

// ProviderFacts is the code-graph provider. Name empty means none was found.
type ProviderFacts struct {
	Name     string
	Version  string
	Semantic Capability
	Entities Capability
	Remedy   string
}

// HostedFacts is the hosted-brain binding.
type HostedFacts struct {
	Bound bool
	// Target names the hosted brain when bound.
	Target string
	// Note qualifies an unbound state. Empty takes the local-only default,
	// which is the verified truth today.
	Note   string
	Remedy string
}

// Detected is everything phase 1 reports.
type Detected struct {
	Repo     RepoFacts
	Agent    AgentFacts
	Provider ProviderFacts
	Hosted   HostedFacts
}

// detailKeyWidth aligns the second column of the Detect block.
const detailKeyWidth = 8

func detailRow(key, value string) string {
	if pad := detailKeyWidth - len(key); pad > 0 {
		key += strings.Repeat(" ", pad)
	}
	return key + " " + value
}

// Summary is the Detect phase's own line: the repository, in one clause each.
func (d Detected) Summary(r *Renderer) string {
	bullet := " " + r.Bullet() + " "
	name := d.Repo.Name
	if name == "" {
		name = "(unnamed)"
	}
	parts := []string{"repo " + name, plural(d.Repo.Commits, "commit")}
	switch {
	case d.Repo.Detached:
		parts = append(parts, "detached HEAD")
	case d.Repo.Dirty:
		parts = append(parts, "uncommitted changes")
	default:
		parts = append(parts, "clean tree")
	}
	return strings.Join(parts, bullet)
}

// Details are the continuation rows: agent, provider, hosted. Each states a
// real capability, and each absent one names a runnable remedy.
func (d Detected) Details(r *Renderer) []string {
	return []string{d.agentRow(r), d.providerRow(r), d.hostedRow(r)}
}

func (d Detected) agentRow(r *Renderer) string {
	if d.Agent.Name == "" {
		return detailRow("agent", "none on PATH "+r.Dash()+" "+remedy(d.Agent.Remedy, RemedyAgent))
	}
	qualifiers := make([]string, 0, 2)
	if d.Agent.Model != "" {
		qualifiers = append(qualifiers, d.Agent.Model)
	}
	if d.Agent.Effort != "" {
		qualifiers = append(qualifiers, "effort "+d.Agent.Effort)
	}
	value := d.Agent.Name
	if len(qualifiers) > 0 {
		value += " (" + strings.Join(qualifiers, ", ") + ")"
	}
	return detailRow("agent", value)
}

func (d Detected) providerRow(r *Renderer) string {
	if d.Provider.Name == "" {
		return detailRow("provider", "none "+r.Dash()+" "+remedy(d.Provider.Remedy, RemedyProvider))
	}
	value := d.Provider.Name
	if d.Provider.Version != "" {
		value += " " + d.Provider.Version
	}
	value += "  " + capabilityText(r, "semantic", d.Provider.Semantic, RemedySemantic)
	value += "  " + capabilityText(r, "entities", d.Provider.Entities, RemedyEntities)
	return detailRow("provider", value)
}

func (d Detected) hostedRow(r *Renderer) string {
	if d.Hosted.Bound {
		target := d.Hosted.Target
		if target == "" {
			target = "bound"
		}
		return detailRow("hosted", "bound "+r.Dash()+" "+target)
	}
	note := d.Hosted.Note
	if note == "" {
		// Verified live: there is no command that resolves this repo to a
		// hosted brain, so setup stays local-only and says which ticket owns
		// the gap rather than implying it will fix itself.
		note = "local-only (COR-2029)"
	}
	return detailRow("hosted", "not bound "+r.Dash()+" "+note+"; bind: "+remedy(d.Hosted.Remedy, RemedyHosted))
}

// capabilityText renders "semantic ✓" or "semantic ✗ (entire brain refresh)".
// The glyph carries the meaning; the colour only makes it quicker to find, so
// the line survives NO_COLOR unchanged.
func capabilityText(r *Renderer, label string, c Capability, fallback string) string {
	if c.OK {
		out := label + " " + r.MarkGlyph(tui.MarkDone)
		if c.Detail != "" {
			out += " (" + c.Detail + ")"
		}
		return out
	}
	return label + " " + r.MarkGlyph(tui.MarkFailed) + " (" + remedy(c.Remedy, fallback) + ")"
}

func remedy(given, fallback string) string {
	if strings.TrimSpace(given) != "" {
		return given
	}
	return fallback
}

// Remedies lists every remedy this block would print for an absent capability.
// It exists so "no line a reader cannot act on" is checkable rather than
// reviewable.
func (d Detected) Remedies() []string {
	out := []string{}
	if d.Agent.Name == "" {
		out = append(out, remedy(d.Agent.Remedy, RemedyAgent))
	}
	if d.Provider.Name == "" {
		out = append(out, remedy(d.Provider.Remedy, RemedyProvider))
	} else {
		if !d.Provider.Semantic.OK {
			out = append(out, remedy(d.Provider.Semantic.Remedy, RemedySemantic))
		}
		if !d.Provider.Entities.OK {
			out = append(out, remedy(d.Provider.Entities.Remedy, RemedyEntities))
		}
	}
	if !d.Hosted.Bound {
		out = append(out, remedy(d.Hosted.Remedy, RemedyHosted))
	}
	return out
}

// Report settles the Detect phase: one phase line plus the capability rows.
func (d Detected) Report(phase *PhaseRun, r *Renderer) {
	phase.OK(d.Summary(r), d.Details(r)...)
}

func plural(n int, noun string) string {
	out := itoa(n) + " " + noun
	if n != 1 {
		out += "s"
	}
	return out
}
