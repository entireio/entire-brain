package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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

type theme struct {
	id, repoKey, scope, title, description, shape, memberKeys string
	support                                                   int
	fingerprint                                               string
	strength                                                  float64
	verdict                                                   string
}

// storeTheme inserts one agent-proposed theme with its verdict-derived status.
func storeTheme(db *sql.DB, th theme, ts string) error {
	var verdict any
	if th.verdict != "" {
		verdict = th.verdict
	}
	_, e := db.Exec(`INSERT OR REPLACE INTO themes
		(id, repo_key, scope, title, description, shape, member_keys, support, fingerprint, strength, status, verdict, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		th.id, th.repoKey, th.scope, th.title, th.description, th.shape, th.memberKeys, th.support,
		th.fingerprint, th.strength, themeStatusForVerdict(th.verdict), verdict, ts, ts)
	return e
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

// loadAcceptedThemeAsDeep maps an accepted theme into the deep-skill input so a
// theme skill is synthesized from the VERIFIED latent practice (its agent
// description), never a raw intent_sig.
func loadAcceptedThemeAsDeep(brainDir, themeID string) (deepSkillInput, bool) {
	db, err := openPatternCorpusDB(brainDir)
	if err != nil {
		return deepSkillInput{}, false
	}
	defer db.Close()
	var title, desc, shape, verdict string
	err = db.QueryRow(`SELECT title, COALESCE(description,''), COALESCE(shape,''), COALESCE(verdict,'')
		FROM themes WHERE id=?`, themeID).Scan(&title, &desc, &shape, &verdict)
	if err != nil || verdict != "accepted" {
		return deepSkillInput{}, false
	}
	return deepSkillInput{
		rec: deepDossierRecord{
			SchemaVersion: deepSchemaVersion,
			PatternID:     themeID,
			Title:         title,
			Trigger:       "latent practice (" + shape + "): " + desc,
		},
		verdict: dossierVerdict{Verdict: "accepted"},
	}, true
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

// --- theme proposal (agent-only, explicit, egress-gated) ---

const themeSampleCap = 50

const themeProposeSystemPrompt = `You are given a SAMPLE of read-only / conversational work episodes from a coding agent's session history — investigations, "how does X work" diagnoses, design discussions, planning. These are the latent practices the command/task channel misses.

Group episodes that share ONE coherent, recurring latent practice — by MEANING, not by wording. Episodes phrased differently can belong to the same practice; episodes that merely share a phrase but concern different topics must NOT be grouped. Leave unrelated episodes ungrouped. Curate membership: include only the episodes that genuinely belong.

Return EXACTLY one JSON object and nothing else (no prose, no code fences):
{
  "themes": [
    {
      "title": "<short title>",
      "description": "<what the recurring practice is>",
      "member_keys": ["<episode_key from the sample>", ...],
      "rationale": "<why these cohere>",
      "verdict": "accepted" | "needs_split" | "low_confidence"
    }
  ]
}

Use "accepted" only for a genuinely coherent recurring practice with enough members; use "needs_split"/"low_confidence" for weaker groupings. Do not invent episode_keys that are not in the sample.`

type proposedTheme struct {
	Title      string   `json:"title"`
	Desc       string   `json:"description"`
	MemberKeys []string `json:"member_keys"`
	Rationale  string   `json:"rationale"`
	Verdict    string   `json:"verdict"`
}

// verifyThemes is the agent-only theme proposal phase (reached via the explicit
// `patterns verify --themes`): it samples shaped episodes, asks an agent to
// propose coherent latent practices by MEANING (not intent_sig buckets), curates
// membership to the agent's choice, expands each seed deterministically, and
// stores the result. Egress-gated; cached by the sample fingerprint; never from
// refresh.
func verifyThemes(ctx context.Context, db *sql.DB, brainDir, repoDir, agent, model, effort string, run distillAgentRunner, now time.Time) (dossierVerifyStats, error) {
	var stats dossierVerifyStats
	if err := rejectAgentForNoEgress(agent); err != nil {
		return stats, err
	}

	// Sample shaped episodes (read-only/conversation), most recent first.
	sample, sampleKeys := sampleShapedEpisodes(db, brainDir)
	if len(sample) < themeMinMembers {
		_, _ = db.Exec(`DELETE FROM themes`) // nothing to propose from
		return stats, nil
	}
	// Fingerprint the actual evidence payload (episode key + shape + intent +
	// excerpt), so the cache invalidates when episode CONTENT changes, not only
	// when the set of keys changes.
	payload, _ := json.Marshal(map[string]any{"episodes": sample})
	sampleFP := proposalSampleFingerprint(payload)
	if corpusMeta(db, "themes_sample_fingerprint") == sampleFP && corpusScalar(db, `SELECT COUNT(*) FROM themes`) > 0 {
		stats.Cached = corpusScalar(db, `SELECT COUNT(*) FROM themes`)
		stats.Considered = stats.Cached
		return stats, nil // unchanged sample → reuse prior proposals
	}

	args, err := distillAgentCommandArgs(agent, nil, themeProposeSystemPrompt)
	if err != nil {
		return stats, err
	}
	args = injectAgentModel(args, agent, model)
	args = injectAgentEffort(args, agent, effort)

	out, err := run(ctx, repoDir, args, []byte(redactText(string(payload))), dossierVerifyTimeout)
	if err != nil {
		return stats, fmt.Errorf("theme proposal agent: %w", err)
	}
	proposed, err := parseProposedThemes(out)
	if err != nil {
		return stats, err
	}

	repoKey := corpusMeta(db, "repo_key")
	ts := now.UTC().Format(time.RFC3339)
	if _, err := db.Exec(`DELETE FROM themes`); err != nil { // agent re-proposes the full set
		return stats, err
	}
	valid := map[string]bool{}
	for _, k := range sampleKeys {
		valid[k] = true
	}
	for _, pt := range proposed {
		stats.Considered++
		members := uniqueStrings(filterValidKeys(pt.MemberKeys, valid))
		if len(members) < themeMinMembers {
			stats.Failed++
			continue
		}
		// Deterministic expansion AFTER the agent seed: pull in additional shaped
		// episodes whose intent/excerpt overlaps the proposed description.
		members = expandThemeMembers(db, members, pt.Title+" "+pt.Desc)
		verdict := strings.ToLower(strings.TrimSpace(pt.Verdict))
		if !allowedVerdicts[verdict] {
			// A missing/unknown verdict must never be treated as accepted — it would
			// surface an unverified theme. Default to the softest non-accepting verdict.
			verdict = "low_confidence"
		}
		memberJSON, _ := json.Marshal(members)
		shape := memberDominantShape(db, members)
		strength := math.Round(supportScoreV2(len(members))*1000) / 1000
		id := "theme:" + hexSHA("theme\x00"+repoKey+"\x00"+strings.Join(sortedCopy(members), "|"))
		if err := storeTheme(db, theme{
			id: id, repoKey: repoKey, scope: "repo",
			title:       redactText(truncateString(strings.TrimSpace(firstNonEmpty(pt.Title, "theme")), 120)),
			description: redactText(truncateString(strings.TrimSpace(firstNonEmpty(pt.Desc, pt.Rationale)), 280)),
			shape:       shape, memberKeys: string(memberJSON), support: len(members),
			fingerprint: themeFingerprint(members), strength: strength, verdict: verdict,
		}, ts); err != nil {
			return stats, err
		}
		if verdict == "accepted" {
			stats.Verified++
		}
	}
	_ = setCorpusMeta(db, map[string]string{"themes_sample_fingerprint": sampleFP})
	return stats, nil
}

func parseProposedThemes(out string) ([]proposedTheme, error) {
	raw := strings.TrimSpace(out)
	if i := strings.IndexByte(raw, '{'); i >= 0 {
		if j := strings.LastIndexByte(raw, '}'); j >= i {
			raw = raw[i : j+1]
		}
	}
	var wrap struct {
		Themes []proposedTheme `json:"themes"`
	}
	if err := json.Unmarshal([]byte(raw), &wrap); err != nil {
		return nil, fmt.Errorf("parse theme proposal: %w", err)
	}
	return wrap.Themes, nil
}

// sampleShapedEpisodes returns up to themeSampleCap recent read-only/conversation
// episodes as {episode_key, shape, intent, excerpt} (redacted), plus their keys.
func sampleShapedEpisodes(db *sql.DB, brainDir string) ([]map[string]string, []string) {
	rows, err := db.Query(`
		SELECT e.episode_key, s.shape, COALESCE(e.intent_raw,''), e.source_path, e.start_line, e.end_line
		FROM episodes e JOIN episode_shapes s ON s.episode_id=e.id
		WHERE s.shape IN ('read_only','conversation')
		ORDER BY e.created_at DESC LIMIT ?`, themeSampleCap)
	if err != nil {
		return nil, nil
	}
	defer rows.Close()
	var sample []map[string]string
	var keys []string
	for rows.Next() {
		var key, shape, intent, path string
		var start, end int
		if rows.Scan(&key, &shape, &intent, &path, &start, &end) != nil {
			continue
		}
		ex := ""
		lines := end - start + 1
		if lines <= 0 || lines > themeMemberExcerptLines {
			lines = themeMemberExcerptLines
		}
		if t := transcriptExcerpt(brainDir, episodeAnchor{Path: path, Line: start}, lines, themeMemberExcerptBytes); t != "" {
			ex = redactText(t)
		}
		sample = append(sample, map[string]string{
			"episode_key": key, "shape": shape,
			"intent": redactText(truncateString(intent, 160)), "excerpt": ex,
		})
		keys = append(keys, key)
	}
	return sample, keys
}

func filterValidKeys(keys []string, valid map[string]bool) []string {
	var out []string
	for _, k := range keys {
		if valid[k] {
			out = append(out, k)
		}
	}
	return out
}

// expandThemeMembers adds shaped episodes whose intent overlaps the seed text,
// beyond the agent's curated members (the deterministic expansion after a seed).
func expandThemeMembers(db *sql.DB, members []string, seedText string) []string {
	terms := map[string]bool{}
	for _, w := range brainBriefTaskWordPattern.FindAllString(strings.ToLower(seedText), -1) {
		if !brainBriefFileMatchTermStop(w) {
			terms[w] = true
		}
	}
	if len(terms) == 0 {
		return members
	}
	have := map[string]bool{}
	for _, m := range members {
		have[m] = true
	}
	rows, err := db.Query(`
		SELECT e.episode_key, COALESCE(e.intent_raw,'')
		FROM episodes e JOIN episode_shapes s ON s.episode_id=e.id
		WHERE s.shape IN ('read_only','conversation')`)
	if err != nil {
		return members
	}
	defer rows.Close()
	for rows.Next() {
		if len(members) >= themeMemberCap {
			break
		}
		var key, intent string
		if rows.Scan(&key, &intent) != nil || have[key] {
			continue
		}
		overlap := 0
		for _, w := range brainBriefTaskWordPattern.FindAllString(strings.ToLower(intent), -1) {
			if terms[w] {
				overlap++
			}
		}
		if overlap >= 2 { // needs real shared vocabulary, not one stopword-ish hit
			members = append(members, key)
			have[key] = true
		}
	}
	return members
}

func memberDominantShape(db *sql.DB, members []string) string {
	shapes := map[string]int{}
	for _, m := range members {
		var shape string
		if db.QueryRow(`SELECT s.shape FROM episode_shapes s JOIN episodes e ON e.id=s.episode_id WHERE e.episode_key=?`, m).Scan(&shape) == nil {
			shapes[shape]++
		}
	}
	return dominantShape(shapes)
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

const themeMemberExcerptLines = 24
const themeMemberExcerptBytes = 700
