package patterns

import (
	"database/sql"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
)

// PatternView is the unified listing shape for both pattern families, so
// procedures and practices sort and render through one path.
type PatternView struct {
	ID            string               `json:"id"`
	Type          string               `json:"type"`
	Scope         string               `json:"scope"`
	Kind          string               `json:"kind,omitempty"`
	Title         string               `json:"title"`
	Strength      float64              `json:"strength"`
	StrengthLabel string               `json:"strength_label"`
	Support       int                  `json:"support"`
	Repos         int                  `json:"repos,omitempty"`          // workspace scope
	RepoBreakdown []PatternRepoStat    `json:"repo_breakdown,omitempty"` // workspace scope
	Reinforcement *ReinforcementCounts `json:"reinforcement,omitempty"`
	Example       *EpisodeAnchor       `json:"example,omitempty"`
	SkillStatus   string               `json:"skill_status,omitempty"` // e.g. "active/update", "declined/reconsider"
	Note          string               `json:"note,omitempty"`         // human-readable recommendation
	// V2 corpus-backed fields (empty for legacy views).
	IntentSig     string `json:"intent_sig,omitempty"`
	Gram          string `json:"gram,omitempty"`
	DossierStatus string `json:"dossier_status,omitempty"` // current | stale (promotable patterns only)
	Verdict       string `json:"verdict,omitempty"`        // accepted | needs_split | low_confidence (rejected are suppressed)
	Workspace     string `json:"workspace,omitempty"`
}

// LoadPatternViews loads procedures + practices as unified views and an index by
// pattern id.
func LoadPatternViews(brainDir string) ([]PatternView, map[string]PatternView, error) {
	procedures, err := LoadBrainProcedures(brainDir)
	if err != nil {
		return nil, nil, err
	}
	practices, err := LoadBrainPractices(brainDir)
	if err != nil {
		return nil, nil, err
	}
	views := make([]PatternView, 0, len(procedures)+len(practices))
	for _, p := range procedures {
		views = append(views, ProcedureView(p))
	}
	for _, p := range practices {
		views = append(views, practiceView(p))
	}
	idx := make(map[string]PatternView, len(views))
	for _, v := range views {
		idx[v.ID] = v
	}
	return views, idx, nil
}

// StrongestPatterns loads the top-N patterns by strength for the overview.
func StrongestPatterns(brainDir string, limit int) []PatternView {
	views, _ := StrongestPatternsChecked(brainDir, limit)
	return views
}

func StrongestPatternsChecked(brainDir string, limit int) ([]PatternView, error) {
	views, _, err := LoadPatternViews(brainDir)
	if err != nil {
		return nil, err
	}
	if len(views) == 0 {
		return nil, nil
	}
	sort.SliceStable(views, func(i, j int) bool { return views[i].Strength > views[j].Strength })
	if limit > 0 && len(views) > limit {
		views = views[:limit]
	}
	return views, nil
}

func ProcedureView(p ProcedureRecord) PatternView {
	r := p.Reinforcement
	v := PatternView{
		ID: p.ID, Type: p.Type, Scope: p.Scope,
		Title:    strings.Join(p.Commands, " → "),
		Strength: p.Strength, StrengthLabel: p.StrengthLabel,
		Support: p.Support, Reinforcement: &r,
		Repos: p.Repos, RepoBreakdown: p.RepoBreakdown,
	}
	if len(p.Examples) > 0 {
		v.Example = &p.Examples[0]
	}
	return v
}

func practiceView(p PracticeRecord) PatternView {
	v := PatternView{
		ID: p.ID, Type: p.Type, Scope: p.Scope, Kind: p.Kind,
		Title:    p.Statement,
		Strength: p.Strength, StrengthLabel: p.StrengthLabel,
		Support: p.Support,
		Repos:   p.Repos, RepoBreakdown: p.RepoBreakdown,
	}
	if len(p.Examples) > 0 {
		v.Example = &p.Examples[0]
	}
	return v
}

// RenderPatternView prints a concise read-only pattern card. Patterns are
// diagnostic; the evidence-first approval card lives in `patterns skills form`.
func RenderPatternView(out io.Writer, v PatternView) {
	label := v.Type
	if v.Kind != "" {
		label += "/" + v.Kind
	}
	scope := v.Scope
	if v.Scope == "workspace" && v.Workspace != "" {
		scope = "workspace:" + v.Workspace
	}
	state := v.DossierStatus
	if v.Verdict != "" {
		state = strings.TrimLeft(state+"/"+v.Verdict, "/")
	}
	header := fmt.Sprintf("[%s] %s  (%s", strings.ToUpper(v.StrengthLabel), base.RedactText(v.Title), label)
	if scope != "" {
		header += ", " + scope
	}
	header += fmt.Sprintf(", strength %.2f", v.Strength)
	if state != "" {
		header += ", " + state
	}
	header += ")"
	fmt.Fprintln(out, header)
	if v.Reinforcement != nil {
		r := v.Reinforcement
		fmt.Fprintf(out, "    seen in %d episode(s); reinforcement %d↑ %d↓ %d·\n", v.Support, r.Success, r.Corrected, r.Neutral)
	} else {
		fmt.Fprintf(out, "    seen in %d session(s)\n", v.Support)
	}
	if v.Scope == "workspace" && v.Repos > 0 {
		repos := make([]string, 0, len(v.RepoBreakdown))
		for _, b := range v.RepoBreakdown {
			repos = append(repos, fmt.Sprintf("%s(%d)", b.RepoKey, b.Support))
		}
		fmt.Fprintf(out, "    across %d repo(s): %s\n", v.Repos, strings.Join(repos, ", "))
	}
	if v.Example != nil {
		fmt.Fprintf(out, "    e.g. %s:%d   id %s\n", base.RedactText(v.Example.Path), v.Example.Line, v.ID)
	} else {
		fmt.Fprintf(out, "    id %s\n", v.ID)
	}
	if v.Note != "" {
		fmt.Fprintf(out, "    ! %s\n", v.Note)
	}
}

// LoadCorpusPatternViewsChecked lists the V2 corpus `patterns` rows as unified
// PatternViews — the primary analytical source for the public `patterns`
// surface (replacing the legacy procedure/practice JSON views). Each view
// carries its dossier state (current/stale + verifier verdict) and top anchor.
// Rejected dossiers are suppressed. Graceful: nil when no corpus exists.
func LoadCorpusPatternViewsChecked(brainDir string) ([]PatternView, map[string]PatternView, bool, error) {
	db, present, err := OpenCorpusReadDBIfPresent(brainDir)
	if err != nil {
		return nil, nil, false, err
	}
	if !present {
		return nil, nil, false, nil
	}
	defer db.Close()

	anchors, err := topAnchorByPatternChecked(db.DB)
	if err != nil {
		return nil, nil, true, err
	}
	rows, err := db.Query(`
		SELECT p.id, p.type, p.scope, COALESCE(p.repo_key,''), COALESCE(p.workspace,''),
		       COALESCE(p.intent_sig,''), COALESCE(p.gram,''), COALESCE(p.meta_id,''),
		       p.title, p.strength, p.strength_label, p.support, p.n_repos,
		       p.outcome_success, p.outcome_corrected, p.outcome_neutral,
		       COALESCE(d.status,''), COALESCE(d.verdict,'')
		FROM patterns p
		LEFT JOIN dossiers d ON d.pattern_id = p.id
		ORDER BY p.strength DESC`)
	if err != nil {
		return nil, nil, true, err
	}
	defer rows.Close()
	var views []PatternView
	for rows.Next() {
		var (
			v                          PatternView
			repoKey, workspace, metaID string
			succ, corr, neutral        int
			dossierStatus, verdict     string
		)
		if err := rows.Scan(&v.ID, &v.Type, &v.Scope, &repoKey, &workspace,
			&v.IntentSig, &v.Gram, &metaID, &v.Title, &v.Strength, &v.StrengthLabel, &v.Support, &v.Repos,
			&succ, &corr, &neutral, &dossierStatus, &verdict); err != nil {
			return nil, nil, true, err
		}
		if verdict == "rejected" { // the agent verifier is the authority that removes
			continue
		}
		v.Kind = metaID
		v.Workspace = workspace
		v.Title = base.RedactText(v.Title)
		v.DossierStatus = dossierStatus
		v.Verdict = verdict
		rc := ReinforcementCounts{Success: succ, Corrected: corr, Neutral: neutral}
		v.Reinforcement = &rc
		if a, ok := anchors[v.ID]; ok {
			ca := a
			v.Example = &ca
		}
		views = append(views, v)
	}
	if rows.Err() != nil {
		return nil, nil, true, rows.Err()
	}
	// Attach the per-repo breakdown to workspace-scope patterns.
	breakdown, err := LoadWorkspaceRepoBreakdownChecked(db.DB)
	if err != nil {
		return nil, nil, true, err
	}
	for i := range views {
		if views[i].Scope == "workspace" {
			views[i].RepoBreakdown = breakdown[views[i].ID]
		}
	}
	idx := make(map[string]PatternView, len(views))
	for _, v := range views {
		idx[v.ID] = v
	}
	return views, idx, true, nil
}

// LoadCorpusPatternViews retains the historical best-effort helper for
// internal scoring/tests. Production response surfaces use the checked form.
func LoadCorpusPatternViews(brainDir string) ([]PatternView, map[string]PatternView, bool) {
	views, index, present, _ := LoadCorpusPatternViewsChecked(brainDir)
	return views, index, present
}

// topAnchorByPattern returns each pattern's rank-0 evidence anchor (redacted).
func topAnchorByPattern(db *sql.DB) map[string]EpisodeAnchor {
	anchors, _ := topAnchorByPatternChecked(db)
	return anchors
}

func topAnchorByPatternChecked(db *sql.DB) (map[string]EpisodeAnchor, error) {
	out := map[string]EpisodeAnchor{}
	rows, err := db.Query(`SELECT pattern_id, source_path, start_line FROM pattern_evidence WHERE rank=0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, path string
		var line int
		if err := rows.Scan(&id, &path, &line); err != nil {
			return nil, err
		}
		out[id] = EpisodeAnchor{Path: base.RedactText(path), Line: line}
	}
	return out, rows.Err()
}
