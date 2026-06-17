package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Themes / latent practices (Pattern Consolidation v2, Priority 5).
//
// The command/task channel misses conversational and read-only work — investigations,
// design discussions, "how does X work" diagnoses. Themes capture those: clusters
// of read-only/conversation episodes that recur under one intent signature. The
// deterministic builder proposes theme CANDIDATES (token-free, during refresh);
// promotion to a surfaced theme requires the explicit, egress-gated theme verifier
// (`patterns verify --themes`). Verified-accepted themes appear in patterns/get and,
// task-gated, in brief/overview; rejected themes never surface.

const themeMinMembers = 3
const themeMemberCap = 40

// classifyEpisodeShape labels an episode by what it did. Deterministic.
func classifyEpisodeShape(nCmds, nTools int, hasWrite, hasRead bool) string {
	switch {
	case hasWrite:
		return "write"
	case nCmds > 0:
		return "shell"
	case hasRead || nTools > 0:
		return "read_only"
	default:
		return "conversation"
	}
}

// classifyEpisodeShapes (re)derives episode_shapes from the already-indexed
// episode signals — no reindex required. Deterministic and token-free.
func classifyEpisodeShapes(db *sql.DB, now time.Time) error {
	rows, err := db.Query(`
		SELECT e.id, e.n_cmds, e.n_tools,
		       MAX(CASE WHEN ef.action IN ('write','edit','add','delete') THEN 1 ELSE 0 END),
		       MAX(CASE WHEN ef.action='read' THEN 1 ELSE 0 END)
		FROM episodes e LEFT JOIN episode_files ef ON ef.episode_id=e.id
		GROUP BY e.id`)
	if err != nil {
		return err
	}
	type row struct {
		id                string
		nCmds, nTools     int
		hasWrite, hasRead sql.NullInt64
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.nCmds, &r.nTools, &r.hasWrite, &r.hasRead); err != nil {
			rows.Close()
			return err
		}
		all = append(all, r)
	}
	rows.Close()

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM episode_shapes`); err != nil {
		return err
	}
	for _, r := range all {
		shape := classifyEpisodeShape(r.nCmds, r.nTools, r.hasWrite.Int64 == 1, r.hasRead.Int64 == 1)
		if _, err := tx.Exec(`INSERT OR REPLACE INTO episode_shapes (episode_id, shape) VALUES (?,?)`, r.id, shape); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// buildThemes proposes theme candidates from recurring read-only/conversation
// episodes grouped by intent signature. Rebuilt each refresh; a cached agent
// verdict survives while the member set (fingerprint) is unchanged.
func buildThemes(db *sql.DB, repoKey string, now time.Time) error {
	rows, err := db.Query(`
		SELECT e.intent_sig, e.episode_key, COALESCE(e.intent_raw,''), s.shape
		FROM episodes e JOIN episode_shapes s ON s.episode_id=e.id
		WHERE e.intent_sig != '' AND s.shape IN ('read_only','conversation')
		ORDER BY e.intent_sig, e.created_at DESC`)
	if err != nil {
		return err
	}
	type group struct {
		keys   []string
		sample string
		shapes map[string]int
	}
	groups := map[string]*group{}
	for rows.Next() {
		var intentSig, epKey, intentRaw, shape string
		if err := rows.Scan(&intentSig, &epKey, &intentRaw, &shape); err != nil {
			rows.Close()
			return err
		}
		g := groups[intentSig]
		if g == nil {
			g = &group{shapes: map[string]int{}}
			groups[intentSig] = g
		}
		if len(g.keys) < themeMemberCap {
			g.keys = append(g.keys, epKey)
		}
		if g.sample == "" {
			g.sample = intentRaw
		}
		g.shapes[shape]++
	}
	rows.Close()

	ts := now.UTC().Format(time.RFC3339)
	keep := map[string]bool{}
	for intentSig, g := range groups {
		members := uniqueStrings(g.keys)
		if len(members) < themeMinMembers {
			continue
		}
		id := "theme:" + hexSHA("theme\x00"+repoKey+"\x00"+intentSig)
		keep[id] = true
		dominant := dominantShape(g.shapes)
		fingerprint := themeFingerprint(members)
		support := 0
		for _, n := range g.shapes {
			support += n
		}
		title := redactText(truncateString(strings.TrimSpace(firstNonEmpty(g.sample, intentSig)), 120))
		desc := redactText("Recurring " + dominant + " work: " + intentSig)
		memberJSON, _ := json.Marshal(members)
		strength := math.Round(supportScoreV2(support)*1000) / 1000

		if err := upsertTheme(db, theme{
			id: id, repoKey: repoKey, scope: "repo", title: title, description: desc,
			shape: dominant, memberKeys: string(memberJSON), support: support,
			fingerprint: fingerprint, strength: strength,
		}, ts); err != nil {
			return err
		}
	}
	return pruneThemes(db, keep)
}

type theme struct {
	id, repoKey, scope, title, description, shape, memberKeys string
	support                                                   int
	fingerprint                                               string
	strength                                                  float64
}

// upsertTheme inserts/updates a theme candidate, preserving a cached verdict
// while the fingerprint is unchanged and resetting to 'candidate' when it moves.
func upsertTheme(db *sql.DB, th theme, ts string) error {
	var existFP, existVerdict string
	err := db.QueryRow(`SELECT fingerprint, COALESCE(verdict,'') FROM themes WHERE id=?`, th.id).Scan(&existFP, &existVerdict)
	switch err {
	case sql.ErrNoRows:
		_, e := db.Exec(`INSERT INTO themes
			(id, repo_key, scope, title, description, shape, member_keys, support, fingerprint, strength, status, verdict, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,'candidate',NULL,?,?)`,
			th.id, th.repoKey, th.scope, th.title, th.description, th.shape, th.memberKeys, th.support, th.fingerprint, th.strength, ts, ts)
		return e
	case nil:
		status := "candidate"
		verdict := any(nil)
		if existVerdict != "" && existFP == th.fingerprint {
			status = themeStatusForVerdict(existVerdict) // keep promotion
			verdict = existVerdict
		}
		_, e := db.Exec(`UPDATE themes SET title=?, description=?, shape=?, member_keys=?, support=?, fingerprint=?, strength=?, status=?, verdict=?, updated_at=? WHERE id=?`,
			th.title, th.description, th.shape, th.memberKeys, th.support, th.fingerprint, th.strength, status, verdict, ts, th.id)
		return e
	default:
		return err
	}
}

func themeStatusForVerdict(verdict string) string {
	if verdict == "accepted" {
		return "active"
	}
	return "candidate"
}

func pruneThemes(db *sql.DB, keep map[string]bool) error {
	rows, err := db.Query(`SELECT id FROM themes`)
	if err != nil {
		return err
	}
	var drop []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil && !keep[id] {
			drop = append(drop, id)
		}
	}
	rows.Close()
	for _, id := range drop {
		if _, err := db.Exec(`DELETE FROM themes WHERE id=?`, id); err != nil {
			return err
		}
	}
	return nil
}

func themeFingerprint(members []string) string {
	sorted := append([]string(nil), members...)
	sort.Strings(sorted)
	return "sha256:" + hexSHA(strings.Join(sorted, "|"))
}

func dominantShape(shapes map[string]int) string {
	best, bestN := "read_only", -1
	for s, n := range shapes {
		if n > bestN || (n == bestN && s < best) {
			best, bestN = s, n
		}
	}
	return best
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// --- theme views (surfaces) ---

type themeView struct {
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Shape       string  `json:"shape"`
	Support     int     `json:"support"`
	Strength    float64 `json:"strength"`
	Status      string  `json:"status"`
	Verdict     string  `json:"verdict,omitempty"`
}

// loadThemeViews returns theme rows. When verifiedOnly, only accepted themes are
// returned (for authoritative surfaces like brief/overview); rejected themes are
// always suppressed. Graceful: nil when no corpus.
func loadThemeViews(brainDir string, verifiedOnly bool) []themeView {
	db, err := openPatternCorpusDB(brainDir)
	if err != nil {
		return nil
	}
	defer db.Close()
	return queryThemeViews(db, verifiedOnly)
}

func queryThemeViews(db *sql.DB, verifiedOnly bool) []themeView {
	q := `SELECT id, title, description, shape, support, strength, status, COALESCE(verdict,'') FROM themes WHERE COALESCE(verdict,'') != 'rejected'`
	if verifiedOnly {
		q += ` AND verdict='accepted'`
	}
	q += ` ORDER BY strength DESC`
	rows, err := db.Query(q)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []themeView
	for rows.Next() {
		var v themeView
		if rows.Scan(&v.ID, &v.Title, &v.Description, &v.Shape, &v.Support, &v.Strength, &v.Status, &v.Verdict) == nil {
			out = append(out, v)
		}
	}
	return out
}

// themePatternView projects a theme into the unified patternView listing.
func themePatternView(th themeView) patternView {
	state := th.Status
	if th.Verdict != "" {
		state = strings.TrimLeft(state+"/"+th.Verdict, "/")
	}
	return patternView{
		ID: th.ID, Type: "theme", Scope: "repo", Kind: th.Shape,
		Title: th.Title, Strength: th.Strength, StrengthLabel: strengthLabelV2(th.Strength),
		Support: th.Support, DossierStatus: state,
	}
}

// strongestThemes returns the top verified themes by strength, capped — for the
// overview. Empty when no corpus.
func strongestThemes(brainDir string, limit int) []themeView {
	if limit <= 0 {
		return nil
	}
	themes := loadThemeViews(brainDir, true)
	if len(themes) > limit {
		themes = themes[:limit]
	}
	return themes
}

// getCorpusTheme resolves one theme by id for `get`.
func getCorpusTheme(brainDir, themeID string) (unifiedResult, bool) {
	db, err := openPatternCorpusDB(brainDir)
	if err != nil {
		return unifiedResult{}, false
	}
	defer db.Close()
	var v themeView
	var members string
	err = db.QueryRow(`SELECT id, title, description, shape, support, strength, status, COALESCE(verdict,''), COALESCE(member_keys,'')
		FROM themes WHERE id=?`, themeID).Scan(&v.ID, &v.Title, &v.Description, &v.Shape, &v.Support, &v.Strength, &v.Status, &v.Verdict, &members)
	if err != nil {
		return unifiedResult{}, false
	}
	var b strings.Builder
	b.WriteString(v.Title + "\n")
	b.WriteString("theme (" + v.Shape + "): " + v.Description + "\n")
	state := v.Status
	if v.Verdict != "" {
		state += "/" + v.Verdict
	}
	b.WriteString("members: " + strconv.Itoa(v.Support) + " episode(s) (" + state + ")")
	return unifiedResult{Source: "theme", ID: v.ID, Text: redactText(b.String())}, true
}

// rankTaskRelevantThemes returns verified themes whose title/description share a
// term with the task, capped — no ambient noise for unrelated tasks.
func rankTaskRelevantThemes(themes []themeView, terms []string, limit int) []themeView {
	if limit <= 0 || len(terms) == 0 {
		return nil
	}
	termSet := map[string]bool{}
	for _, t := range terms {
		termSet[t] = true
	}
	type scored struct {
		v       themeView
		overlap int
	}
	var matched []scored
	for _, th := range themes {
		hay := strings.ToLower(th.Title + " " + th.Description)
		seen := map[string]bool{}
		overlap := 0
		for _, w := range brainBriefTaskWordPattern.FindAllString(hay, -1) {
			if termSet[w] && !seen[w] {
				seen[w] = true
				overlap++
			}
		}
		if overlap > 0 {
			matched = append(matched, scored{th, overlap})
		}
	}
	sort.SliceStable(matched, func(i, j int) bool {
		if matched[i].overlap != matched[j].overlap {
			return matched[i].overlap > matched[j].overlap
		}
		return matched[i].v.Strength > matched[j].v.Strength
	})
	out := make([]themeView, 0, limit)
	for _, m := range matched {
		if len(out) >= limit {
			break
		}
		out = append(out, m.v)
	}
	return out
}

// --- theme verifier (explicit, egress-gated, cached) ---

const themeVerifySystemPrompt = `You audit a THEME: a cluster of recurring read-only or conversational work episodes from a coding agent's history (investigations, "how does X work" diagnoses, design discussions) that the command/task channel does not capture.

You are given the theme as JSON: title, description, shape, and member count. Decide whether it is a coherent, recurring, repo-meaningful latent practice worth surfacing — not an incidental grab-bag.

Return EXACTLY one JSON object and nothing else (no prose, no code fences):
{
  "schema_version": 1,
  "verdict": "accepted" | "rejected" | "needs_split" | "low_confidence",
  "reason": "<one or two sentences>",
  "unsupported_claims": [],
  "conflated_subpatterns": [],
  "required_edits": [],
  "evidence_fingerprint": "<copy the theme fingerprint verbatim>"
}

Accept only a coherent recurring practice; reject incidental or generic clusters.`

// verifyThemes runs the theme verifier over candidate themes whose fingerprint
// has no cached verdict. Egress-gated; cached; never from refresh.
func verifyThemes(ctx context.Context, db *sql.DB, repoDir, agent, model, effort string, run distillAgentRunner, now time.Time) (dossierVerifyStats, error) {
	var stats dossierVerifyStats
	if err := rejectAgentForNoEgress(agent); err != nil {
		return stats, err
	}
	args, err := distillAgentCommandArgs(agent, nil, themeVerifySystemPrompt)
	if err != nil {
		return stats, err
	}
	args = injectAgentModel(args, agent, model)
	args = injectAgentEffort(args, agent, effort)

	rows, err := db.Query(`SELECT id, title, description, shape, support, fingerprint, COALESCE(verdict,'') FROM themes ORDER BY id`)
	if err != nil {
		return stats, err
	}
	type cand struct {
		id, title, desc, shape, fp, verdict string
		support                             int
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.title, &c.desc, &c.shape, &c.support, &c.fp, &c.verdict); err != nil {
			rows.Close()
			return stats, err
		}
		cands = append(cands, c)
	}
	rows.Close()

	ts := now.UTC().Format(time.RFC3339)
	for _, c := range cands {
		stats.Considered++
		if c.verdict != "" {
			stats.Cached++
			continue
		}
		payload, _ := json.Marshal(map[string]any{
			"title": c.title, "description": c.desc, "shape": c.shape,
			"members": c.support, "fingerprint": c.fp,
		})
		verdict, raw, err := runThemeVerifier(ctx, repoDir, args, redactText(string(payload)), c.fp, run)
		if err != nil {
			stats.Failed++
			continue
		}
		if _, err := db.Exec(`UPDATE themes SET verdict=?, status=?, updated_at=? WHERE id=?`,
			verdict.Verdict, themeStatusForVerdict(verdict.Verdict), ts, c.id); err != nil {
			return stats, err
		}
		_ = raw
		stats.Verified++
	}
	return stats, nil
}

func runThemeVerifier(ctx context.Context, repoDir string, args []string, themeJSON, fingerprint string, run distillAgentRunner) (dossierVerdict, string, error) {
	out, err := run(ctx, repoDir, args, []byte(redactText(themeJSON)), dossierVerifyTimeout)
	if err != nil {
		return dossierVerdict{}, "", err
	}
	v, raw, err := parseDossierVerdict(out)
	if err != nil {
		return dossierVerdict{}, "", err
	}
	v.EvidenceFingerprint = fingerprint
	return v, raw, nil
}
