package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"
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

// ---- feature list endpoints (the drill-in views behind the hub) ----

type vizFact struct {
	ID         string   `json:"id"`
	Kind       string   `json:"kind,omitempty"`
	Text       string   `json:"text"`
	Locus      []string `json:"locus,omitempty"`
	Paths      []string `json:"paths,omitempty"`
	Status     string   `json:"status,omitempty"`
	Origin     string   `json:"origin,omitempty"`
	Confidence string   `json:"confidence,omitempty"`
}

type vizListResp struct {
	Items     any      `json:"items"`
	Total     int      `json:"total"`
	Truncated bool     `json:"truncated,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
}

// factBranches lists the branches whose facts to show — the current branch first,
// then every branch the manifest records. Facts are branch-scoped on disk, but
// the hub's Facts view is a whole-brain view, so it aggregates across branches
// (otherwise the count and the list disagree when the checkout is on a branch
// that carries no facts).
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
	items := make([]vizFact, 0, len(all))
	for _, f := range all {
		items = append(items, vizFact{ID: f.ID, Kind: f.Kind, Text: f.Text, Locus: f.Locus, Paths: f.Paths, Status: f.Status, Origin: f.Origin, Confidence: f.Confidence})
	}
	writeJSONHTTP(w, http.StatusOK, vizListResp{Items: items, Total: total, Truncated: trunc, Warnings: warnings})
}

type vizSession struct {
	ID          string `json:"id"`
	Branch      string `json:"branch,omitempty"`
	Agent       string `json:"agent,omitempty"`
	Model       string `json:"model,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Created     string `json:"created,omitempty"`
	Files       int    `json:"files"`
	Checkpoints int    `json:"checkpoints"`
	Intent      string `json:"intent,omitempty"`
	Outcome     string `json:"outcome,omitempty"`
}

func (s *vizServer) handleSessions(w http.ResponseWriter, r *http.Request) {
	limit := vizQueryLimit(r, 500)
	sessions := s.sessionsFromManifest()
	total := len(sessions)
	trunc := limit > 0 && total > limit
	if trunc {
		sessions = sessions[:limit]
	}
	items := make([]vizSession, 0, len(sessions))
	for _, se := range sessions {
		item := vizSession{ID: se.SessionID, Branch: se.Branch, Agent: se.Agent, Model: se.Model, Kind: se.Kind, Files: len(se.FilesTouched), Checkpoints: se.CheckpointsCount}
		if !se.CreatedAt.IsZero() {
			item.Created = se.CreatedAt.UTC().Format("2006-01-02 15:04Z")
		}
		if se.Summary != nil {
			item.Intent = se.Summary.Intent
			item.Outcome = se.Summary.Outcome
		}
		items = append(items, item)
	}
	writeJSONHTTP(w, http.StatusOK, vizListResp{Items: items, Total: total, Truncated: trunc})
}

type vizHistory struct {
	ID      string `json:"id"`
	Kind    string `json:"kind,omitempty"`
	Branch  string `json:"branch,omitempty"`
	Summary string `json:"summary"`
}

func (s *vizServer) handleHistory(w http.ResponseWriter, r *http.Request) {
	limit := vizQueryLimit(r, 300)
	resp := vizListResp{Items: []vizHistory{}}
	if s.manifest == nil || s.manifest.Sources == nil || s.manifest.Sources.History == nil {
		resp.Warnings = []string{"no history yet — run `entire brain refresh`"}
		writeJSONHTTP(w, http.StatusOK, resp)
		return
	}
	idx, err := loadBrainHistoryIndex(s.brainDir, s.manifest.Sources.History)
	if err != nil {
		resp.Warnings = []string{"history unavailable: " + err.Error()}
		writeJSONHTTP(w, http.StatusOK, resp)
		return
	}
	recs := idx.Records
	total := len(recs)
	trunc := limit > 0 && total > limit
	if trunc {
		recs = recs[:limit]
	}
	items := make([]vizHistory, 0, len(recs))
	for _, h := range recs {
		items = append(items, vizHistory{ID: h.ID, Kind: h.Kind, Branch: h.Branch, Summary: h.Summary})
	}
	writeJSONHTTP(w, http.StatusOK, vizListResp{Items: items, Total: total, Truncated: trunc})
}

type vizDoc struct {
	ID      string `json:"id"`
	Path    string `json:"path"`
	Heading string `json:"heading,omitempty"`
	Text    string `json:"text,omitempty"`
}

func (s *vizServer) handleDocs(w http.ResponseWriter, r *http.Request) {
	limit := vizQueryLimit(r, 400)
	idx, err := loadDocIndex(s.brainDir)
	if err != nil {
		writeJSONHTTP(w, http.StatusOK, vizListResp{Items: []vizDoc{}, Warnings: []string{"docs unavailable: " + err.Error()}})
		return
	}
	recs := idx.Records
	total := len(recs)
	trunc := limit > 0 && total > limit
	if trunc {
		recs = recs[:limit]
	}
	items := make([]vizDoc, 0, len(recs))
	for _, d := range recs {
		text := d.Text
		if len(text) > 400 {
			text = text[:400]
		}
		items = append(items, vizDoc{ID: d.ID, Path: d.Path, Heading: d.Heading, Text: text})
	}
	writeJSONHTTP(w, http.StatusOK, vizListResp{Items: items, Total: total, Truncated: trunc})
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
	resp.Truncated = sem.Symbols > len(syms)

	idset := make(map[string]bool, len(syms))
	for i := range syms {
		resp.Nodes = append(resp.Nodes, nodeJSON(syms[i]))
		idset[syms[i].ID] = true
	}
	relLimit := limit * 4
	if relLimit <= 0 {
		relLimit = 0
	}
	if rels, rerr := s.relationsForSymbols(sem, syms, relLimit); rerr != nil {
		resp.Warnings = append(resp.Warnings, "relations: "+rerr.Error())
	} else {
		for i := range rels {
			e := rels[i]
			if idset[e.FromID] && idset[e.ToID] {
				resp.Edges = append(resp.Edges, edgeJSON(e))
			}
		}
	}
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
