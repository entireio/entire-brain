package tui

import "strings"

// Tab identifies a dashboard section. Home is the brain status/coverage view;
// the rest are entry browsers.
type Tab int

const (
	TabHome Tab = iota
	TabFacts
	TabSessions
	TabHistory
	TabSemantic
	TabSearch
)

// allTabs is the fixed left-to-right tab order.
var allTabs = []Tab{TabHome, TabFacts, TabSessions, TabHistory, TabSemantic, TabSearch}

// ParseTab resolves a tab name (case-insensitive), defaulting to Home for an
// empty or unknown name. The bool reports whether the name matched a known tab.
// A whitespace-only name is trimmed to empty and treated as the default Home
// selection (ok=true), so a blank --tab is not rejected as unknown.
func ParseTab(name string) (Tab, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "home":
		return TabHome, true
	case "facts":
		return TabFacts, true
	case "sessions":
		return TabSessions, true
	case "history":
		return TabHistory, true
	case "semantic":
		return TabSemantic, true
	case "search":
		return TabSearch, true
	default:
		return TabHome, false
	}
}

func (t Tab) String() string {
	switch t {
	case TabHome:
		return "Home"
	case TabFacts:
		return "Facts"
	case TabSessions:
		return "Sessions"
	case TabHistory:
		return "History"
	case TabSemantic:
		return "Semantic"
	case TabSearch:
		return "Search"
	default:
		return "?"
	}
}

// Snapshot is the read-only view of a built brain handed to the TUI. The cli
// layer assembles it from disk; the TUI never does file IO. All content is
// deterministic — derived from data the brain already stores, with no agent
// calls.
type Snapshot struct {
	Repo        string
	Branch      string
	GeneratedAt string
	Home        HomeView
	Facts       []FactView
	Sessions    []SessionView
	History     []HistoryView
	Semantic    []SemanticView
	// Search holds the results of the most recent in-dashboard search (the `s`
	// key). SearchQuery is the query that produced them, and SearchErr any error
	// from running it. They start empty and update live as the user searches.
	Search      []SearchResult
	SearchQuery string
	SearchErr   string
	// SearchEnabled reports whether in-dashboard search is wired (a non-nil
	// SearchFunc). It only affects the Search tab's empty-state prompt, so the
	// prompt doesn't tell the user to press `s` when `s` is a no-op.
	SearchEnabled bool
	// Notes records non-fatal context, e.g. capped lists ("facts: showing 500
	// of 1234"), so truncation is never silent.
	Notes []string
}

// SearchResult is one hit from an in-dashboard search across facts, history, and
// docs (the same retrieval `entire brain search` runs).
type SearchResult struct {
	Source   string // fact | history | doc
	ID       string
	Path     string // brain-relative (display)
	Heading  string
	Line     int
	Text     string
	Score    float64
	OpenPath string // absolute source path to open ("" if none)
	OpenLine int
}

// HomeView is the brain status/coverage summary rendered on the Home tab.
type HomeView struct {
	Sources    []SourceHealth
	Freshness  string // severity label ("" when unknown)
	Axes       []FreshnessAxis
	BlindSpots []string
	Warnings   []string
	Live       LiveState
}

type SourceHealth struct {
	Name    string // Seed, Sessions, Semantic, History, Facts
	Present bool
	Detail  string // e.g. "55 symbols · 8 files"
}

type FreshnessAxis struct {
	Name   string
	State  string
	Detail string
}

type LiveState struct {
	Branch  string
	Head    string
	Dirty   bool
	Summary string // "3 staged · 1 untracked" or "clean"
}

// FactView is one durable fact for the Facts tab.
type FactView struct {
	ID         string
	Kind       string
	Text       string
	Paths      []string
	Locus      []string
	Status     string
	Origin     string
	Confidence string
	Related    []string
	Provenance []Anchor
	Source     string // absolute transcript path to open ("" if none)
	SourceLine int
}

// Anchor cites where a fact was derived from.
type Anchor struct {
	SessionID  string
	Commit     string
	Transcript string // brain-relative (display)
	Line       int
	Verified   bool
}

// SessionView is one recorded session for the Sessions tab.
type SessionView struct {
	ID          string
	Branch      string
	Agent       string
	Model       string
	Kind        string
	Created     string
	Files       []string
	InputTok    int
	OutputTok   int
	Intent      string
	Outcome     string
	Checkpoints int
	Source      string // absolute transcript path to open ("" if none)
}

// SemanticView is one code symbol for the Semantic tab.
type SemanticView struct {
	Kind          string
	Name          string
	QualifiedName string
	FilePath      string // repo-relative (display)
	StartLine     int
	EndLine       int
	Signature     string
	Language      string
	Source        string // absolute file path to open ("" if none)
	SourceLine    int
}

// HistoryView is one history record (decision/learning/...) for the History tab.
type HistoryView struct {
	ID         string
	Kind       string
	Branch     string
	Summary    string
	Path       string // brain-relative (display)
	Line       int
	Source     string // absolute transcript path to open ("" if none)
	SourceLine int
}

// count returns the number of rows on a tab. The Home tab lists the brain's
// sources, so it counts those.
func (s Snapshot) count(tab Tab) int {
	switch tab {
	case TabHome:
		return len(s.Home.Sources)
	case TabFacts:
		return len(s.Facts)
	case TabSessions:
		return len(s.Sessions)
	case TabHistory:
		return len(s.History)
	case TabSemantic:
		return len(s.Semantic)
	case TabSearch:
		return len(s.Search)
	default:
		return 0
	}
}

// filterKey returns the lowercased text a row is matched against by the filter.
func (s Snapshot) filterKey(tab Tab, i int) string {
	switch tab {
	case TabHome:
		if i >= 0 && i < len(s.Home.Sources) {
			return strings.ToLower(s.Home.Sources[i].Name + " " + s.Home.Sources[i].Detail)
		}
		return ""
	case TabFacts:
		f := s.Facts[i]
		return strings.ToLower(f.Kind + " " + f.Text + " " + strings.Join(f.Paths, " ") + " " + strings.Join(f.Locus, " "))
	case TabSessions:
		v := s.Sessions[i]
		return strings.ToLower(v.Agent + " " + v.Model + " " + v.ID + " " + v.Intent + " " + v.Outcome)
	case TabHistory:
		h := s.History[i]
		return strings.ToLower(h.Kind + " " + h.Summary + " " + h.Branch)
	case TabSemantic:
		v := s.Semantic[i]
		return strings.ToLower(v.Kind + " " + v.Name + " " + v.QualifiedName + " " + v.FilePath + " " + v.Language)
	case TabSearch:
		r := s.Search[i]
		return strings.ToLower(r.Source + " " + r.Text + " " + r.Path + " " + r.Heading)
	default:
		return ""
	}
}

// openTarget returns the absolute source path and 1-based line to open for the
// entry at index i on a tab, or ("", 0) when there is nothing to open.
func (s Snapshot) openTarget(tab Tab, i int) (string, int) {
	switch tab {
	case TabFacts:
		if i >= 0 && i < len(s.Facts) {
			return s.Facts[i].Source, s.Facts[i].SourceLine
		}
	case TabSessions:
		if i >= 0 && i < len(s.Sessions) {
			return s.Sessions[i].Source, 0
		}
	case TabHistory:
		if i >= 0 && i < len(s.History) {
			return s.History[i].Source, s.History[i].SourceLine
		}
	case TabSemantic:
		if i >= 0 && i < len(s.Semantic) {
			return s.Semantic[i].Source, s.Semantic[i].SourceLine
		}
	case TabSearch:
		if i >= 0 && i < len(s.Search) {
			return s.Search[i].OpenPath, s.Search[i].OpenLine
		}
	}
	return "", 0
}
