package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// handoff.go is Phase 2 item 3 (agent-utility plan): the session handoff
// packet. An agent's context window ends and the next session starts cold,
// paying an expensive re-derivation tax ("what was in flight, what failed,
// what's blocked?"). The brain already holds the answer — session manifest +
// history index + facts — and the hand-written handover doc proved the
// artifact's shape. `brief --handoff` makes it a deterministic verb, tuned
// for resumption: STATE over knowledge ("PR #21 merged; the fused-arm
// question is open pending a Gemma run"), not conventions.

const (
	handoffDefaultSessions       = 5
	handoffDecisionsPerSession   = 3
	handoffValidationsPerSession = 2
	handoffRecentFacts           = 5
)

type handoffSession struct {
	SessionID   string    `json:"session_id"`
	Branch      string    `json:"branch,omitempty"`
	Agent       string    `json:"agent,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	Request     string    `json:"request,omitempty"`
	Decisions   []string  `json:"decisions,omitempty"`
	Validations []string  `json:"validations,omitempty"`
}

type handoffPacket struct {
	GeneratedAt     time.Time        `json:"generated_at"`
	Branch          string           `json:"branch"`
	Sessions        []handoffSession `json:"sessions"`
	Branches        []string         `json:"active_branches,omitempty"`
	RecentFacts     []factRecord     `json:"recent_facts,omitempty"`
	LastDistilledAt time.Time        `json:"last_distilled_at,omitzero"`
	// SessionsSinceDistill is the blind-spot signal: sessions captured after
	// the last distill whose insights are not yet in the fact store.
	SessionsSinceDistill int `json:"sessions_since_distill"`
	// Consolidations are the repo's strongest current/stale dossiers (v2) — the
	// patterns to keep in mind when resuming, with staleness visible.
	Consolidations []briefConsolidation `json:"consolidations,omitempty"`
	Warnings       []string             `json:"warnings,omitempty"`
}

// buildHandoffPacket synthesizes the packet deterministically — no agent, no
// embedder, warm path only. Sessions are attributed their history records via
// TranscriptPath == record.Path (the same provenance link history-eval-gen
// labels through).
func buildHandoffPacket(manifest *exportManifest, index historyIndex, facts []factRecord, branch string, sessionCount int, now time.Time, guard sessionReadGuard) handoffPacket {
	if sessionCount <= 0 {
		sessionCount = handoffDefaultSessions
	}
	p := handoffPacket{GeneratedAt: now, Branch: branch}

	type sessionRecords struct {
		request     string
		requestLine int
		decisions   []historyRecord
		validations []historyRecord
	}
	byPath := map[string]*sessionRecords{}
	get := func(path string) *sessionRecords {
		sr, ok := byPath[path]
		if !ok {
			sr = &sessionRecords{}
			byPath[path] = sr
		}
		return sr
	}
	for _, r := range index.Records {
		if guard.blocksRecord(r) {
			continue
		}
		switch r.Kind {
		case "request":
			// The session's opening request is the lowest-line request record
			// that is a real user ask — injected wrappers (environment context,
			// local-command caveats) are skipped with the same filter the facts
			// eval uses; later requests are mid-session follow-ups.
			if isWrapperRequest(r.Summary) {
				continue
			}
			sr := get(r.Path)
			if sr.request == "" || r.Line < sr.requestLine {
				sr.request, sr.requestLine = r.Summary, r.Line
			}
		case "decision":
			get(r.Path).decisions = append(get(r.Path).decisions, r)
		case "validation":
			get(r.Path).validations = append(get(r.Path).validations, r)
		}
	}

	sessions := guardExportSessions(guard, append([]exportSession(nil), manifest.Sources.Sessions.Sessions...))
	sort.SliceStable(sessions, func(i, j int) bool { return sessions[i].CreatedAt.After(sessions[j].CreatedAt) })
	branches := map[string]bool{}
	// A session continued across branches is exported once per branch with the
	// same id; the packet keeps only its most recent incarnation so the window
	// shows N distinct trajectories, not one session three times.
	seenSession := map[string]bool{}
	for _, s := range sessions {
		if len(p.Sessions) >= sessionCount {
			break
		}
		if seenSession[s.SessionID] {
			continue
		}
		seenSession[s.SessionID] = true
		hs := handoffSession{SessionID: shortSessionID(s.SessionID), Branch: s.Branch, Agent: s.Agent, CreatedAt: s.CreatedAt}
		if sr, ok := byPath[s.TranscriptPath]; ok {
			hs.Request = sr.request
			// Last decisions/validations carry the session's END state — where
			// it left off — which is what resumption needs; openers are usually
			// re-derivable from the request.
			hs.Decisions = lastSummaries(sr.decisions, handoffDecisionsPerSession)
			hs.Validations = lastSummaries(sr.validations, handoffValidationsPerSession)
		}
		branches[s.Branch] = true
		p.Sessions = append(p.Sessions, hs)
	}
	for b := range branches {
		if b != "" {
			p.Branches = append(p.Branches, b)
		}
	}
	sort.Strings(p.Branches)

	facts = guardFactRecords(guard, facts)
	active := make([]factRecord, 0, len(facts))
	for _, f := range facts {
		if f.Status == factStatusActive {
			active = append(active, f)
		}
	}
	sort.SliceStable(active, func(i, j int) bool { return active[i].UpdatedAt.After(active[j].UpdatedAt) })
	if len(active) > handoffRecentFacts {
		active = active[:handoffRecentFacts]
	}
	p.RecentFacts = active

	if last, _, ok := distillCoverage(manifest); ok {
		p.LastDistilledAt = last
		for _, session := range sessions {
			if session.CreatedAt.After(last) {
				p.SessionsSinceDistill++
			}
		}
	} else {
		p.SessionsSinceDistill = len(sessions)
		p.Warnings = append(p.Warnings, "no distilled facts yet; run `entire brain distill`")
	}
	return p
}

// lastSummaries returns the summaries of the last n usable records in index
// order (records were appended in scan order, which follows the transcript).
// Records whose summary is raw tool-call payload (e.g. `Bash {"command":…}`)
// are skipped — they are mechanics, and a resumption packet that quotes JSON
// blobs buries the trajectory it exists to surface.
func lastSummaries(recs []historyRecord, n int) []string {
	usable := make([]historyRecord, 0, len(recs))
	for _, r := range recs {
		if strings.Contains(r.Summary, `{"`) {
			continue
		}
		usable = append(usable, r)
	}
	if len(usable) > n {
		usable = usable[len(usable)-n:]
	}
	out := make([]string, 0, len(usable))
	for _, r := range usable {
		out = append(out, r.Summary)
	}
	return out
}

func runBrainHandoff(ctx context.Context, cmd *cobra.Command, opts Options, sessionCount int, jsonOut bool) error {
	_, brainDir, branch, err := resolveFactsTarget(ctx, opts, agentSurfaceTarget(opts, nil), "")
	if err != nil {
		return err
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		return err
	}
	if manifest.Sources == nil || manifest.Sources.Sessions == nil || len(manifest.Sources.Sessions.Sessions) == 0 {
		return fmt.Errorf("no exported sessions; run `entire brain refresh` first")
	}
	guard, err := loadSessionReadGuard(brainDir, manifest)
	if err != nil {
		return err
	}
	privacyPolicy := retrievalPrivacyPolicy{BrainDir: brainDir, Identity: guard.policyIdentity}
	privacyPolicy.RequireDerivedClean = true
	if err := requirePrivacyDerivedRead(brainDir); err != nil {
		return err
	}
	index, err := loadBrainHistoryIndex(brainDir, manifest.Sources.History)
	if err != nil {
		index = historyIndex{} // a handoff without history detail beats no handoff
	}
	facts, err := loadFacts(brainDir, branch)
	if err != nil {
		facts = nil
	}
	packet := buildHandoffPacket(manifest, index, facts, branch, sessionCount, opts.Now().UTC(), guard)
	packet.Consolidations, err = handoffConsolidationsChecked(brainDir, 5)
	if err != nil {
		return err
	}
	return bufferRetrievalCommandOutput(cmd, []retrievalPrivacyPolicy{privacyPolicy}, func() error {
		if jsonOut {
			return writeJSON(cmd, packet)
		}
		renderHandoff(cmd, packet)
		return nil
	})
}

func renderHandoff(cmd *cobra.Command, p handoffPacket) {
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "# Handoff — last %d session(s)", len(p.Sessions))
	if len(p.Branches) > 0 {
		fmt.Fprintf(w, " across %s", strings.Join(p.Branches, ", "))
	}
	fmt.Fprintln(w)
	if !p.LastDistilledAt.IsZero() {
		fmt.Fprintf(w, "facts last distilled %s; %d session(s) captured since (insights not yet distilled)\n",
			p.LastDistilledAt.Format("2006-01-02"), p.SessionsSinceDistill)
	}
	for _, warning := range p.Warnings {
		fmt.Fprintf(w, "warning: %s\n", warning)
	}
	for _, s := range p.Sessions {
		fmt.Fprintf(w, "\n## %s", s.SessionID)
		var meta []string
		if s.Branch != "" {
			meta = append(meta, s.Branch)
		}
		if s.Agent != "" {
			meta = append(meta, s.Agent)
		}
		meta = append(meta, s.CreatedAt.Format("2006-01-02 15:04"))
		fmt.Fprintf(w, " (%s)\n", strings.Join(meta, ", "))
		if s.Request != "" {
			fmt.Fprintf(w, "request: %s\n", truncateString(s.Request, 200))
		}
		for _, d := range s.Decisions {
			fmt.Fprintf(w, "  - %s\n", truncateString(d, 200))
		}
		for _, v := range s.Validations {
			fmt.Fprintf(w, "  ✓ %s\n", truncateString(v, 200))
		}
	}
	if len(p.RecentFacts) > 0 {
		fmt.Fprintf(w, "\n## Recently updated facts\n")
		for _, f := range p.RecentFacts {
			fmt.Fprintf(w, "- [%s] %s\n", factKindOrInferred(f), truncateString(f.Text, 200))
		}
	}
	if len(p.Consolidations) > 0 {
		fmt.Fprintf(w, "\n## Consolidated patterns\n")
		for _, c := range p.Consolidations {
			state := c.Status
			if c.Verdict != "" {
				state += "/" + c.Verdict
			}
			fmt.Fprintf(w, "- [%s %s] %s   id %s\n", c.Type, state, truncateString(c.Title, 120), c.PatternID)
		}
	}
}
