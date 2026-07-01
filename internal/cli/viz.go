package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	entirebrain "github.com/ashtom/entire-brain"
)

// vizDefaultLimit caps how many symbols load into the graph. Large brains hold
// many thousands of symbols; the cap keeps the force simulation smooth and a
// "truncated" flag tells the UI to surface it. 0 = no cap.
const vizDefaultLimit = 4000

// vizGraphViewCap bounds how many symbols actually render in the semantic view.
// We load a wider pool (vizDefaultLimit), compute degree, then keep the most-
// connected symbols so the default graph is a legible, connected constellation
// instead of a hairball of arbitrary, mostly-isolated nodes. Drilling in via the
// inspector's "Expand neighbors" pulls in the rest on demand. 0 = render all.
const vizGraphViewCap = 1400

type vizFlags struct {
	port   int
	branch string
	noOpen bool
	limit  int
}

// newVizCommand builds `entire brain viz` — a local, no-egress web interface
// that renders the brain's semantic graph. It mirrors newDashCommand's shape
// (dash.go) but serves a browser UI instead of a terminal TUI.
func newVizCommand(opts Options) *cobra.Command {
	var flags vizFlags
	cmd := &cobra.Command{
		Use:   "viz [path]",
		Short: "Explore the brain as a visual graph in your browser (local, offline)",
		Long: `viz starts a local, no-egress web interface that renders the brain's
semantic graph — code entities and their CALLS/IMPORTS relations — as an
explorable, force-directed graph with search and a symbol inspector.

It binds to 127.0.0.1 only, serves a self-contained embedded UI (no CDN), and
reads the brain from disk read-only: no agent calls, no outbound network.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runViz(cmd.Context(), cmd, opts, flags, agentSurfaceTarget(opts, args))
		},
	}
	cmd.Flags().IntVar(&flags.port, "port", 0, "Port to bind on 127.0.0.1 (0 = auto-pick a free port)")
	cmd.Flags().StringVar(&flags.branch, "branch", "", "Branch whose brain to load (default: current branch)")
	cmd.Flags().BoolVar(&flags.noOpen, "no-open", false, "Do not open a browser automatically")
	cmd.Flags().IntVar(&flags.limit, "limit", vizDefaultLimit, "Max symbols loaded into the graph (0 = no cap)")
	return cmd
}

// vizServer holds the resolved brain location for the lifetime of one server.
// Like the dashboard, it captures a single brain (brainDir/branch/manifest) at
// startup; re-run `viz` to pick up a fresh `refresh`.
type vizServer struct {
	opts     Options
	target   string
	repoDir  string
	brainDir string
	branch   string
	manifest *exportManifest
	limit    int
	// repo coordinates parsed from the brain key (provider/owner/repo), used to
	// build real entire.io + source links. Empty provider = local repo (no links).
	provider string
	owner    string
	repo     string
}

func runViz(ctx context.Context, cmd *cobra.Command, opts Options, flags vizFlags, target string) error {
	repoDir, brainDir, branch, err := resolveFactsTarget(ctx, opts, target, flags.branch)
	if err != nil {
		return err
	}

	// Best-effort: an unbuilt brain still serves — the UI shows an empty state
	// ("run `entire brain refresh`") rather than failing the command.
	var manifest *exportManifest
	if status, serr := buildBrainStatusReport(ctx, opts, target); serr == nil {
		manifest = status.Manifest
	}

	srv := &vizServer{opts: opts, target: target, repoDir: repoDir, brainDir: brainDir, branch: branch, manifest: manifest, limit: flags.limit}
	srv.provider, srv.owner, srv.repo = parseRepoFromBrainDir(brainDir)

	// Loopback only — never 0.0.0.0. The interface is a personal, read-only view
	// of local data and must not be reachable off-host.
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(flags.port))
	if err != nil {
		return fmt.Errorf("bind viz server: %w", err)
	}
	url := "http://" + ln.Addr().String()
	fmt.Fprintf(cmd.OutOrStdout(), "entire brain viz — %s\n", url)
	fmt.Fprintln(cmd.OutOrStdout(), "Read-only, loopback-only, no network. Press Ctrl-C to stop.")
	if !flags.noOpen {
		openBrowser(url)
	}

	httpSrv := &http.Server{Handler: vizSecurityHeaders(srv.mux()), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()
	if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("viz server: %w", err)
	}
	return nil
}

func (s *vizServer) mux() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("/api/summary", s.handleSummary)
	m.HandleFunc("/api/graph", s.handleGraph)
	m.HandleFunc("/api/node", s.handleNode)
	m.HandleFunc("/api/search", s.handleSearch)
	m.HandleFunc("/api/facts", s.handleFacts)
	m.HandleFunc("/api/sessions", s.handleSessions)
	m.HandleFunc("/api/history", s.handleHistory)
	m.HandleFunc("/api/docs", s.handleDocs)
	if sub, err := fs.Sub(entirebrain.WebUI, "webui/dist"); err == nil {
		m.Handle("/", http.FileServer(http.FS(sub)))
	}
	return m
}

// vizSecurityHeaders makes the no-egress guarantee machine-checkable: the CSP
// forbids any off-origin fetch (script/style/img/font/connect all 'self'), so
// even a compromised asset can't call home. Structural, not just a comment.
func vizSecurityHeaders(next http.Handler) http.Handler {
	const csp = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; font-src 'self'; base-uri 'none'; frame-ancestors 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// ---- JSON view models (clean shapes; never leak raw semanticRecord) ----

type vizNode struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	QualifiedName string `json:"qualified_name,omitempty"`
	Kind          string `json:"kind"`
	File          string `json:"file,omitempty"`
	Line          int    `json:"line,omitempty"`
	Language      string `json:"language,omitempty"`
	Signature     string `json:"signature,omitempty"`
	ContainerID   string `json:"container_id,omitempty"`
}

type vizEdge struct {
	From       string  `json:"from"`
	To         string  `json:"to"`
	Type       string  `json:"type"`
	Confidence float64 `json:"confidence,omitempty"`
	Scope      string  `json:"scope,omitempty"`
	Resolution string  `json:"resolution,omitempty"`
}

type vizGraphResp struct {
	Nodes     []vizNode `json:"nodes"`
	Edges     []vizEdge `json:"edges"`
	Total     int       `json:"total,omitempty"`
	Truncated bool      `json:"truncated,omitempty"`
	Warnings  []string  `json:"warnings,omitempty"`
}

type vizNodeResp struct {
	Symbol    vizNode   `json:"symbol"`
	Neighbors []vizNode `json:"neighbors"`
	Relations []vizEdge `json:"relations"`
	Snippet   string    `json:"snippet,omitempty"`
	Link      string    `json:"link,omitempty"`
	LinkLabel string    `json:"link_label,omitempty"`
	Warnings  []string  `json:"warnings,omitempty"`
}

type vizCounts struct {
	Symbols   int `json:"symbols"`
	Relations int `json:"relations"`
	Files     int `json:"files"`
	History   int `json:"history"`
	Facts     int `json:"facts"`
	Sessions  int `json:"sessions"`
	Docs      int `json:"docs"`
}

type vizSummaryResp struct {
	Repo        string    `json:"repo"`
	Branch      string    `json:"branch"`
	GeneratedAt string    `json:"generated_at,omitempty"`
	Counts      vizCounts `json:"counts"`
	Warnings    []string  `json:"warnings,omitempty"`
}

type vizHit struct {
	Source string  `json:"source"`
	ID     string  `json:"id"`
	Title  string  `json:"title,omitempty"`
	Text   string  `json:"text,omitempty"`
	Kind   string  `json:"kind,omitempty"`
	Path   string  `json:"path,omitempty"`
	Line   int     `json:"line,omitempty"`
	Score  float64 `json:"score,omitempty"`
}

type vizSearchResp struct {
	Hits []vizHit `json:"hits"`
}

func nodeJSON(r semanticRecord) vizNode {
	return vizNode{
		ID: r.ID, Name: r.Name, QualifiedName: r.QualifiedName, Kind: r.Kind,
		File: r.FilePath, Line: r.StartLine, Language: r.Language,
		Signature: r.Signature, ContainerID: r.ContainerID,
	}
}

func edgeJSON(r semanticRecord) vizEdge {
	return vizEdge{From: r.FromID, To: r.ToID, Type: r.Type, Confidence: r.Confidence, Scope: r.RelationScope, Resolution: r.Resolution}
}

// ---- handlers ----

func (s *vizServer) handleSummary(w http.ResponseWriter, r *http.Request) {
	resp := vizSummaryResp{Repo: s.repoDir, Branch: s.branch}
	if status, err := buildBrainStatusReport(r.Context(), s.opts, s.target); err == nil {
		if status.Repo.Root != "" {
			resp.Repo = status.Repo.Root
		}
		resp.GeneratedAt = status.Brain.GeneratedAt
		resp.Warnings = status.Warnings
		if m := status.Manifest; m != nil && m.Sources != nil {
			if sem := m.Sources.Semantic; sem != nil {
				resp.Counts.Symbols = sem.Symbols
				resp.Counts.Relations = sem.Relations
				resp.Counts.Files = sem.Files
			}
			if h := m.Sources.History; h != nil {
				resp.Counts.History = h.Records
			}
			if f := m.Sources.Facts; f != nil {
				resp.Counts.Facts = f.Facts
			}
			if ss := m.Sources.Sessions; ss != nil {
				resp.Counts.Sessions = len(ss.Sessions)
			}
		}
	}
	if docs, err := loadDocIndex(s.brainDir); err == nil {
		resp.Counts.Docs = len(docs.Records)
	}
	writeJSONHTTP(w, http.StatusOK, resp)
}

// sessionsFromManifest returns the export sessions captured in the manifest.
func (s *vizServer) sessionsFromManifest() []exportSession {
	if s.manifest == nil || s.manifest.Sources == nil || s.manifest.Sources.Sessions == nil {
		return nil
	}
	return s.manifest.Sources.Sessions.Sessions
}

func vizQueryLimit(r *http.Request, def int) int {
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return def
}

// ---- feature graph endpoints (each brain feature rendered as a graph) ----

// vizGNode is a node in a feature graph. Group drives its color/shape in the UI;
// Link is a real, clickable URL (an entire.io session page, a source file, ...).
type vizGNode struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind,omitempty"`
	Group     string `json:"group"`
	Color     string `json:"color,omitempty"`
	Text      string `json:"text,omitempty"`
	Meta      string `json:"meta,omitempty"`
	Link      string `json:"link,omitempty"`
	LinkLabel string `json:"link_label,omitempty"`
	File      string `json:"file,omitempty"`
	Line      int    `json:"line,omitempty"`
}

type vizGEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Type string `json:"type,omitempty"`
}

type vizFeatureGraph struct {
	Nodes     []vizGNode `json:"nodes"`
	Edges     []vizGEdge `json:"edges"`
	Total     int        `json:"total"`
	Truncated bool       `json:"truncated,omitempty"`
	Warnings  []string   `json:"warnings,omitempty"`
}

const (
	colorFact    = "#fbbf24"
	colorSession = "#818cf8"
	colorHistory = "#f25333"
	colorDoc     = "#34d399"
)

// linkProviders are the git forges whose brain keys map to real web URLs.
var linkProviders = map[string]bool{"gh": true, "gl": true, "bb": true}

// parseRepoFromBrainDir recovers provider/owner/repo from the brain key that is
// the tail of brainDir (e.g. .../repos/gh/acme/app). Empty provider = a local
// repo with no forge, hence no web links.
func parseRepoFromBrainDir(brainDir string) (provider, owner, repo string) {
	parts := strings.Split(strings.Trim(filepath.ToSlash(brainDir), "/"), "/")
	if len(parts) < 3 {
		return "", "", ""
	}
	p, o, r := parts[len(parts)-3], parts[len(parts)-2], parts[len(parts)-1]
	if !linkProviders[p] || o == "" || r == "" {
		return "", "", ""
	}
	return p, o, r
}

func vizWebBase() string {
	if v := strings.TrimSpace(os.Getenv("ENTIRE_WEB_BASE_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "https://entire.io"
}

// sessionLink is the real entire.io page for a session.
func (s *vizServer) sessionLink(id string) string {
	id = strings.TrimSpace(id)
	if id == "" || s.provider == "" {
		return ""
	}
	return fmt.Sprintf("%s/%s/%s/%s/session/%s", vizWebBase(), s.provider, s.owner, s.repo, url.PathEscape(id))
}

// sourceLink is the forge page for a source file (GitHub blob for gh repos).
func (s *vizServer) sourceLink(file string, line int) string {
	file = strings.TrimSpace(file)
	if file == "" || s.provider != "gh" {
		return ""
	}
	branch := s.branch
	if branch == "" {
		branch = "HEAD"
	}
	u := fmt.Sprintf("https://github.com/%s/%s/blob/%s/%s", s.owner, s.repo, branch, file)
	if line > 0 {
		u += fmt.Sprintf("#L%d", line)
	}
	return u
}

func vizShortLabel(text string, words int) string {
	f := strings.Fields(strings.ReplaceAll(text, "\n", " "))
	if len(f) > words {
		return strings.Join(f[:words], " ") + "..."
	}
	return strings.Join(f, " ")
}

func vizJoinMeta(parts ...string) string {
	out := parts[:0]
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " · ")
}

func vizEdgeKey(a, b string) string {
	if a > b {
		return b + "|" + a
	}
	return a + "|" + b
}

// linkBySharedKey connects nodes that share a key (a touched file, a locus token,
// a term). capPerKey bounds the fan-out so a widely-shared key can't explode the
// edge count.
func linkBySharedKey(keyToIDs map[string][]string, typ string, capPerKey int) []vizGEdge {
	var edges []vizGEdge
	seen := map[string]bool{}
	for _, ids := range keyToIDs {
		if len(ids) < 2 {
			continue
		}
		if capPerKey > 0 && len(ids) > capPerKey {
			ids = ids[:capPerKey]
		}
		for i := 0; i < len(ids); i++ {
			for j := i + 1; j < len(ids); j++ {
				k := vizEdgeKey(ids[i], ids[j])
				if seen[k] {
					continue
				}
				seen[k] = true
				edges = append(edges, vizGEdge{From: ids[i], To: ids[j], Type: typ})
			}
		}
	}
	return edges
}

// factBranches lists the branches whose facts to show — the current branch first,
// then every branch the manifest records. Facts are branch-scoped on disk, but
// the Facts view is a whole-brain view, so it aggregates across branches.
func (s *vizServer) factBranches() []string {
	out := []string{}
	seen := map[string]bool{}
	add := func(b string) {
		b = strings.TrimSpace(b)
		if b != "" && !seen[b] {
			seen[b] = true
			out = append(out, b)
		}
	}
	add(s.branch)
	if s.manifest != nil && s.manifest.Sources != nil && s.manifest.Sources.Facts != nil {
		for _, b := range s.manifest.Sources.Facts.Branches {
			add(b)
		}
	}
	if len(out) == 0 {
		out = append(out, s.branch)
	}
	return out
}

func (s *vizServer) handleFacts(w http.ResponseWriter, r *http.Request) {
	limit := vizQueryLimit(r, 500)
	seen := map[string]bool{}
	all := []factRecord{}
	var warnings []string
	for _, br := range s.factBranches() {
		fs, err := loadFacts(s.brainDir, br)
		if err != nil {
			warnings = append(warnings, "facts["+br+"]: "+err.Error())
			continue
		}
		for _, f := range fs {
			if f.ID != "" && seen[f.ID] {
				continue
			}
			seen[f.ID] = true
			all = append(all, f)
		}
	}
	total := len(all)
	trunc := limit > 0 && total > limit
	if trunc {
		all = all[:limit]
	}
	idset := map[string]bool{}
	nodes := make([]vizGNode, 0, len(all))
	locusMap := map[string][]string{}
	for _, f := range all {
		idset[f.ID] = true
		link, label := "", ""
		for _, a := range f.Provenance {
			if a.SessionID != "" {
				if l := s.sessionLink(a.SessionID); l != "" {
					link, label = l, "Open source session in Entire"
				}
				break
			}
		}
		conf := ""
		if f.Confidence != "" {
			conf = "conf " + f.Confidence
		}
		nodes = append(nodes, vizGNode{ID: f.ID, Name: vizShortLabel(f.Text, 6), Kind: f.Kind, Group: "fact", Color: colorFact, Text: f.Text, Meta: vizJoinMeta(f.Kind, f.Status, f.Origin, conf), Link: link, LinkLabel: label})
		for _, l := range f.Locus {
			key := strings.ToLower(strings.TrimSpace(l))
			if key != "" {
				locusMap[key] = append(locusMap[key], f.ID)
			}
		}
	}
	edges := []vizGEdge{}
	relSeen := map[string]bool{}
	for _, f := range all {
		for _, rid := range f.RelatedIDs {
			if !idset[rid] {
				continue
			}
			k := vizEdgeKey(f.ID, rid)
			if relSeen[k] {
				continue
			}
			relSeen[k] = true
			edges = append(edges, vizGEdge{From: f.ID, To: rid, Type: "related"})
		}
	}
	edges = append(edges, linkBySharedKey(locusMap, "shared locus", 6)...)
	writeJSONHTTP(w, http.StatusOK, vizFeatureGraph{Nodes: nodes, Edges: edges, Total: total, Truncated: trunc, Warnings: warnings})
}

func (s *vizServer) handleSessions(w http.ResponseWriter, r *http.Request) {
	limit := vizQueryLimit(r, 500)
	sessions := s.sessionsFromManifest()
	total := len(sessions)
	trunc := limit > 0 && total > limit
	if trunc {
		sessions = sessions[:limit]
	}
	nodes := make([]vizGNode, 0, len(sessions))
	fileMap := map[string][]string{}
	for _, se := range sessions {
		created := ""
		if !se.CreatedAt.IsZero() {
			created = se.CreatedAt.UTC().Format("2006-01-02 15:04Z")
		}
		files := ""
		if n := len(se.FilesTouched); n > 0 {
			files = fmt.Sprintf("%d files", n)
		}
		cps := ""
		if se.CheckpointsCount > 0 {
			cps = fmt.Sprintf("%d checkpoints", se.CheckpointsCount)
		}
		name := vizJoinMeta(se.Agent, se.Model)
		if name == "" {
			name = "session " + vizShortID(se.SessionID)
		}
		text := ""
		if se.Summary != nil {
			text = firstNonEmpty(se.Summary.Intent, se.Summary.Outcome)
		}
		link := s.sessionLink(se.SessionID)
		label := ""
		if link != "" {
			label = "Open in Entire"
		}
		nodes = append(nodes, vizGNode{ID: se.SessionID, Name: name, Kind: se.Kind, Group: "session", Color: colorSession, Text: text, Meta: vizJoinMeta(created, files, cps), Link: link, LinkLabel: label})
		for _, fp := range se.FilesTouched {
			if fp = strings.TrimSpace(fp); fp != "" {
				fileMap[fp] = append(fileMap[fp], se.SessionID)
			}
		}
	}
	edges := linkBySharedKey(fileMap, "shared file", 8)
	writeJSONHTTP(w, http.StatusOK, vizFeatureGraph{Nodes: nodes, Edges: edges, Total: total, Truncated: trunc})
}

func vizHistoryColor(kind string) string {
	switch k := strings.ToLower(kind); {
	case strings.Contains(k, "decision"):
		return colorSession
	case strings.Contains(k, "learning"):
		return "#22d3ee"
	default:
		return colorHistory
	}
}

func (s *vizServer) handleHistory(w http.ResponseWriter, r *http.Request) {
	limit := vizQueryLimit(r, 300)
	if s.manifest == nil || s.manifest.Sources == nil || s.manifest.Sources.History == nil {
		writeJSONHTTP(w, http.StatusOK, vizFeatureGraph{Nodes: []vizGNode{}, Edges: []vizGEdge{}, Warnings: []string{"no history yet — run `entire brain refresh`"}})
		return
	}
	idx, err := loadBrainHistoryIndex(s.brainDir, s.manifest.Sources.History)
	if err != nil {
		writeJSONHTTP(w, http.StatusOK, vizFeatureGraph{Nodes: []vizGNode{}, Edges: []vizGEdge{}, Warnings: []string{"history unavailable: " + err.Error()}})
		return
	}
	recs := idx.Records
	total := len(recs)
	trunc := limit > 0 && total > limit
	if trunc {
		recs = recs[:limit]
	}
	nodes := make([]vizGNode, 0, len(recs))
	termMap := map[string][]string{}
	for _, h := range recs {
		nodes = append(nodes, vizGNode{ID: h.ID, Name: vizShortLabel(h.Summary, 7), Kind: h.Kind, Group: "history", Color: vizHistoryColor(h.Kind), Text: h.Summary, Meta: vizJoinMeta(h.Kind, h.Branch)})
		for _, t := range h.Terms {
			key := strings.ToLower(strings.TrimSpace(t))
			if len(key) > 2 {
				termMap[key] = append(termMap[key], h.ID)
			}
		}
	}
	edges := linkBySharedKey(termMap, "shared topic", 5)
	writeJSONHTTP(w, http.StatusOK, vizFeatureGraph{Nodes: nodes, Edges: edges, Total: total, Truncated: trunc})
}

func (s *vizServer) handleDocs(w http.ResponseWriter, r *http.Request) {
	limit := vizQueryLimit(r, 400)
	idx, err := loadDocIndex(s.brainDir)
	if err != nil {
		writeJSONHTTP(w, http.StatusOK, vizFeatureGraph{Nodes: []vizGNode{}, Edges: []vizGEdge{}, Warnings: []string{"docs unavailable: " + err.Error()}})
		return
	}
	recs := idx.Records
	total := len(recs)
	trunc := limit > 0 && total > limit
	if trunc {
		recs = recs[:limit]
	}
	nodes := make([]vizGNode, 0, len(recs))
	pathMap := map[string][]string{}
	for _, d := range recs {
		name := firstNonEmpty(d.Heading, filepath.Base(d.Path))
		text := d.Text
		if len(text) > 500 {
			text = text[:500]
		}
		link := s.sourceLink(d.Path, d.Line)
		label := ""
		if link != "" {
			label = "View source"
		}
		nodes = append(nodes, vizGNode{ID: d.ID, Name: name, Group: "doc", Color: colorDoc, Text: text, Meta: d.Path, File: d.Path, Line: d.Line, Link: link, LinkLabel: label})
		if d.Path != "" {
			pathMap[d.Path] = append(pathMap[d.Path], d.ID)
		}
	}
	edges := linkBySharedKey(pathMap, "same file", 8)
	writeJSONHTTP(w, http.StatusOK, vizFeatureGraph{Nodes: nodes, Edges: edges, Total: total, Truncated: trunc})
}

func vizShortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func (s *vizServer) semanticSource() *semanticSourceManifest {
	if s.manifest == nil || s.manifest.Sources == nil {
		return nil
	}
	return s.manifest.Sources.Semantic
}

func (s *vizServer) handleGraph(w http.ResponseWriter, r *http.Request) {
	limit := s.limit
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	resp := vizGraphResp{Nodes: []vizNode{}, Edges: []vizEdge{}}
	sem := s.semanticSource()
	if sem == nil {
		resp.Warnings = append(resp.Warnings, "no semantic graph yet — run `entire brain refresh`")
		writeJSONHTTP(w, http.StatusOK, resp)
		return
	}
	syms, err := loadSemanticSymbols(s.brainDir, sem, limit)
	if err != nil {
		resp.Warnings = append(resp.Warnings, "semantic symbols: "+err.Error())
		writeJSONHTTP(w, http.StatusOK, resp)
		return
	}
	if len(syms) == 0 {
		resp.Warnings = append(resp.Warnings, "the semantic snapshot has no symbols yet — run `entire brain refresh`")
		writeJSONHTTP(w, http.StatusOK, resp)
		return
	}
	resp.Total = sem.Symbols

	// Load relations across the symbol pool, then keep the most-connected
	// symbols so the default view is a legible, connected graph rather than a
	// hairball of thousands of arbitrary (often isolated) nodes. loadSemanticSymbols
	// returns symbols in storage order, so an unranked cap would drop hubs and
	// keep leaves; ranking by degree first fixes that.
	poolSet := make(map[string]bool, len(syms))
	for i := range syms {
		poolSet[syms[i].ID] = true
	}
	relLimit := limit * 4
	if relLimit <= 0 {
		relLimit = 0
	}
	var rels []semanticRecord
	if r, rerr := s.relationsForSymbols(sem, syms, relLimit); rerr != nil {
		resp.Warnings = append(resp.Warnings, "relations: "+rerr.Error())
	} else {
		rels = r
	}
	deg := make(map[string]int, len(syms))
	for i := range rels {
		e := rels[i]
		if poolSet[e.FromID] && poolSet[e.ToID] {
			deg[e.FromID]++
			deg[e.ToID]++
		}
	}
	kept := syms
	if vizGraphViewCap > 0 && len(syms) > vizGraphViewCap {
		ranked := make([]semanticRecord, len(syms))
		copy(ranked, syms)
		sort.SliceStable(ranked, func(i, j int) bool { return deg[ranked[i].ID] > deg[ranked[j].ID] })
		kept = ranked[:vizGraphViewCap]
	}
	keptSet := make(map[string]bool, len(kept))
	for i := range kept {
		resp.Nodes = append(resp.Nodes, nodeJSON(kept[i]))
		keptSet[kept[i].ID] = true
	}
	for i := range rels {
		e := rels[i]
		if keptSet[e.FromID] && keptSet[e.ToID] {
			resp.Edges = append(resp.Edges, edgeJSON(e))
		}
	}
	resp.Truncated = sem.Symbols > len(kept)
	writeJSONHTTP(w, http.StatusOK, resp)
}

func (s *vizServer) handleNode(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	resp := vizNodeResp{Neighbors: []vizNode{}, Relations: []vizEdge{}}
	if id == "" {
		writeJSONHTTP(w, http.StatusBadRequest, vizNodeResp{Warnings: []string{"missing id"}})
		return
	}
	sem := s.semanticSource()
	if sem == nil {
		resp.Warnings = append(resp.Warnings, "no semantic graph")
		writeJSONHTTP(w, http.StatusOK, resp)
		return
	}
	node, neighbors, rels, err := s.nodeDetail(sem, id)
	if err != nil {
		resp.Warnings = append(resp.Warnings, err.Error())
		writeJSONHTTP(w, http.StatusOK, resp)
		return
	}
	if node == nil {
		writeJSONHTTP(w, http.StatusNotFound, vizNodeResp{Neighbors: []vizNode{}, Relations: []vizEdge{}, Warnings: []string{"symbol not found: " + id}})
		return
	}
	resp.Symbol = nodeJSON(*node)
	resp.Snippet = node.Blob
	if l := s.sourceLink(node.FilePath, node.StartLine); l != "" {
		resp.Link = l
		resp.LinkLabel = "View source"
	}
	for i := range neighbors {
		resp.Neighbors = append(resp.Neighbors, nodeJSON(neighbors[i]))
	}
	for i := range rels {
		resp.Relations = append(resp.Relations, edgeJSON(rels[i]))
	}
	writeJSONHTTP(w, http.StatusOK, resp)
}

func (s *vizServer) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	limit := 25
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	resp := vizSearchResp{Hits: []vizHit{}}
	if q == "" {
		writeJSONHTTP(w, http.StatusOK, resp)
		return
	}
	// Symbol matches first so ⌘K / rail search can jump straight to a graph node.
	if sem := s.semanticSource(); sem != nil {
		if syms, _, _, err := semanticContextFacts(s.brainDir, sem, q, limit, 0); err == nil {
			for _, sy := range syms {
				resp.Hits = append(resp.Hits, vizHit{Source: "symbol", ID: sy.ID, Title: sy.Name, Text: sy.QualifiedName, Kind: sy.Kind, Path: sy.FilePath, Line: sy.StartLine})
			}
		}
	}
	// Memory hits (facts / history / docs), same code path as `entire brain search`.
	if u, err := retrieveUnified(s.brainDir, s.branch, q, limit, modeLexical); err == nil {
		for _, h := range u {
			resp.Hits = append(resp.Hits, vizHit{Source: h.Source, ID: h.ID, Title: h.Heading, Text: h.Text, Path: h.Path, Line: h.Line, Score: h.Score})
		}
	}
	writeJSONHTTP(w, http.StatusOK, resp)
}

// relationsForSymbols returns the relations among the given symbols, mirroring
// the SQLite-vs-snapshot branch semanticContextFacts uses (semantic.go).
func (s *vizServer) relationsForSymbols(sem *semanticSourceManifest, syms []semanticRecord, limit int) ([]semanticRecord, error) {
	if sem.StorePath != "" {
		storePath, err := validateSemanticDeclaredStore(s.brainDir, sem)
		if err != nil {
			return nil, err
		}
		return findSemanticRelationsForSymbolsInSQLite(storePath, syms, limit)
	}
	full, err := s.snapshotFullPath(sem)
	if err != nil {
		return nil, err
	}
	return findSemanticRelationsForSymbols(full, syms, limit)
}

// nodeDetail resolves one symbol by exact id plus its relations and neighbors,
// mirroring semanticContextFacts but seeded by id rather than a search query.
func (s *vizServer) nodeDetail(sem *semanticSourceManifest, id string) (*semanticRecord, []semanticRecord, []semanticRecord, error) {
	const limit = 80
	if sem.StorePath != "" {
		storePath, err := validateSemanticDeclaredStore(s.brainDir, sem)
		if err != nil {
			return nil, nil, nil, err
		}
		byID, err := loadSemanticSymbolsByIDsSQLite(storePath, []string{id})
		if err != nil {
			return nil, nil, nil, err
		}
		node, ok := byID[id]
		if !ok {
			return nil, nil, nil, nil
		}
		rels, err := findSemanticRelationsForSymbolsInSQLite(storePath, []semanticRecord{node}, limit*4)
		if err != nil {
			return nil, nil, nil, err
		}
		neighbors, err := s.resolveNeighbors(node, rels, func(ids []string) (map[string]semanticRecord, error) {
			return loadSemanticSymbolsByIDsSQLite(storePath, ids)
		}, limit)
		if err != nil {
			return nil, nil, nil, err
		}
		return &node, neighbors, rels, nil
	}
	full, err := s.snapshotFullPath(sem)
	if err != nil {
		return nil, nil, nil, err
	}
	byID, err := loadSemanticSymbolsByIDsSnapshot(full, []string{id})
	if err != nil {
		return nil, nil, nil, err
	}
	node, ok := byID[id]
	if !ok {
		return nil, nil, nil, nil
	}
	rels, err := findSemanticRelationsForSymbols(full, []semanticRecord{node}, limit*4)
	if err != nil {
		return nil, nil, nil, err
	}
	neighbors, err := s.resolveNeighbors(node, rels, func(ids []string) (map[string]semanticRecord, error) {
		return loadSemanticSymbolsByIDsSnapshot(full, ids)
	}, limit)
	if err != nil {
		return nil, nil, nil, err
	}
	return &node, neighbors, rels, nil
}

func (s *vizServer) resolveNeighbors(node semanticRecord, rels []semanticRecord, load func([]string) (map[string]semanticRecord, error), limit int) ([]semanticRecord, error) {
	ids := neighborCandidateIDs([]semanticRecord{node}, rels)
	if len(ids) == 0 {
		return nil, nil
	}
	byID, err := load(ids)
	if err != nil {
		return nil, err
	}
	return resolveContextNeighbors([]semanticRecord{node}, rels, byID, limit), nil
}

func (s *vizServer) snapshotFullPath(sem *semanticSourceManifest) (string, error) {
	snap, err := validateSemanticSnapshotPath(sem.SnapshotPath)
	if err != nil {
		return "", err
	}
	if err := rejectSymlinkPathComponents(s.brainDir, snap); err != nil {
		return "", err
	}
	return filepath.Join(s.brainDir, snap), nil
}

func writeJSONHTTP(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// openBrowser best-effort opens url in the default browser. Failure never fails
// the command — the URL is already printed for manual navigation.
func openBrowser(url string) {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", url)
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		c = exec.Command("xdg-open", url)
	}
	_ = c.Start()
}
