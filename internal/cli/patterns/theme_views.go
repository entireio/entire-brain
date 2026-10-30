package patterns

import (
	"database/sql"
	"strings"
)

// --- theme views (surfaces) ---

type ThemeView struct {
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Shape       string  `json:"shape"`
	Support     int     `json:"support"`
	Strength    float64 `json:"strength"`
	Status      string  `json:"status"`
	Verdict     string  `json:"verdict,omitempty"`
}

// LoadThemeViews returns theme rows. When verifiedOnly, only accepted themes are
// returned (for authoritative surfaces like brief/overview); rejected themes are
// always suppressed. Graceful: nil when no corpus.
func LoadThemeViews(brainDir string, verifiedOnly bool) []ThemeView {
	themes, _ := LoadThemeViewsChecked(brainDir, verifiedOnly)
	return themes
}

func LoadThemeViewsChecked(brainDir string, verifiedOnly bool) ([]ThemeView, error) {
	db, present, err := OpenCorpusReadDBIfPresent(brainDir)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, nil
	}
	defer db.Close()
	return QueryThemeViewsChecked(db.DB, verifiedOnly)
}

func QueryThemeViews(db *sql.DB, verifiedOnly bool) []ThemeView {
	themes, _ := QueryThemeViewsChecked(db, verifiedOnly)
	return themes
}

func QueryThemeViewsChecked(db *sql.DB, verifiedOnly bool) ([]ThemeView, error) {
	// A corpus that predates the additive themes table has no themes, not a
	// broken read.
	if ok, err := CorpusHasTable(db, "themes"); err != nil || !ok {
		return nil, err
	}
	q := `SELECT id, title, description, shape, support, strength, status, COALESCE(verdict,'') FROM themes WHERE COALESCE(verdict,'') != 'rejected'`
	if verifiedOnly {
		q += ` AND verdict='accepted'`
	}
	q += ` ORDER BY strength DESC`
	rows, err := db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ThemeView
	for rows.Next() {
		var v ThemeView
		if err := rows.Scan(&v.ID, &v.Title, &v.Description, &v.Shape, &v.Support, &v.Strength, &v.Status, &v.Verdict); err != nil {
			return out, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ThemePatternView projects a theme into the unified PatternView listing.
func ThemePatternView(th ThemeView) PatternView {
	state := th.Status
	if th.Verdict != "" {
		state = strings.TrimLeft(state+"/"+th.Verdict, "/")
	}
	return PatternView{
		ID: th.ID, Type: "theme", Scope: "repo", Kind: th.Shape,
		Title: th.Title, Strength: th.Strength, StrengthLabel: StrengthLabelV2(th.Strength),
		Support: th.Support, DossierStatus: state,
	}
}
