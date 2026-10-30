package viz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/cmdutil"
	"github.com/entireio/entire-brain/internal/cli/internal/docstore"
	"github.com/entireio/entire-brain/internal/cli/internal/historystore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"github.com/entireio/entire-brain/internal/cli/internal/retrieval"
	"github.com/entireio/entire-brain/internal/cli/internal/semstore"
	"io"
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
	"sync"
	"time"

	entirebrain "github.com/entireio/entire-brain"
	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	"github.com/spf13/cobra"
)

// vizGraphMaxView is a safety ceiling on rendered nodes — NOT a feature cap. It's
// set well above any real brain's largest feature (this repo's history is ~172k)
// so the node-count slider reaches every feature's true total; it exists only to
// stop a pathological ?limit=99999999 request from trying to allocate the world.
const vizGraphMaxView = 300000

// vizGraphMaxEdges is the absolute ceiling on rendered edges. The per-request cap
// (see HandleGraph) scales with the node count so a full graph shows its real
// structure instead of an artificially sparse thread; this just bounds the worst
// case for payload + per-frame draw cost.
const vizGraphMaxEdges = 90000

// vizSearchMaxHits bounds ?limit on /api/search. Hits render in the rail and
// command palette, so a few hundred per source is already past useful.
const vizSearchMaxHits = 500

// vizGraphViewCap bounds how many symbols actually render in the semantic view.
// We load a wider pool (see HandleGraph), compute degree, then keep the most-
// connected symbols so the default graph is a legible, connected constellation
// instead of a hairball of arbitrary, mostly-isolated nodes. Drilling in via the
// inspector's "Expand neighbors" pulls in the rest on demand. 0 = render
// everything up to the vizGraphMaxView safety ceiling.
const vizGraphViewCap = 1400

type Flags struct {
	Port   int
	Branch string
	NoOpen bool
}

// New builds `entire brain viz` — a local, no-egress web interface
// that renders the brain's semantic graph. It mirrors dash.New's shape
// (command/dash/dash.go) but serves a browser UI instead of a terminal TUI.
func New(cfg Config) *cobra.Command {
	var flags Flags
	cmd := &cobra.Command{
		Use:   "viz [path]",
		Short: "Explore the brain as a graph in your browser",
		Long: `viz starts a local, no-egress web interface that renders the brain's
code graph — code entities and their CALLS/IMPORTS relations — as an
explorable, force-directed graph with search and a symbol inspector.

It binds to 127.0.0.1 only, serves a self-contained embedded UI (no CDN), and
reads the brain from disk read-only: no agent calls, no outbound network.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return Run(cmd.Context(), cmd, cfg, flags, cfg.Deps.AgentSurfaceTarget(cfg.Options, args))
		},
	}
	cmd.Flags().IntVar(&flags.Port, "port", 0, "Port to bind on 127.0.0.1 (0 = auto-pick a free port)")
	cmd.Flags().StringVar(&flags.Branch, "branch", "", "Branch whose brain to load (default: current branch)")
	cmd.Flags().BoolVar(&flags.NoOpen, "no-open", false, "Do not open a browser automatically")
	return cmd
}

// Server holds the resolved brain location for the lifetime of one server.
// Like the dashboard, it captures a single brain (brainDir/branch/manifest) at
// startup; re-run `viz` to pick up a fresh `refresh`.
type Server struct {
	RepoDir       string
	BrainDir      string
	Branch        string
	Manifest      *bmanifest.Manifest
	Deps          Deps
	guardMu       sync.Mutex
	Guard         brainstore.SessionReadGuard
	GuardObserved bool
	// repo coordinates parsed from the brain key (provider/owner/repo), used to
	// build real entire.io + source links. Empty provider = local repo (no links).
	Provider string
	Owner    string
	Repo     string
	// symPool caches the largest symbol prefix loaded so far. The brain is
	// captured once at startup and never changes for the server's lifetime, and
	// semstore.LoadSymbols returns a stable storage-order prefix, so any smaller
	// pool is a slice of a larger one — slider moves after the first load never
	// re-parse the store. symPoolFull means the cache holds the entire store.
	symMu       sync.Mutex
	symPool     []semstore.Record
	symPoolFull bool
}

// vizListenAddr is the bind address — loopback ONLY, never 0.0.0.0 or ::. Factored
// out so the no-egress invariant (unreachable off-host) is unit-testable.
func vizListenAddr(port int) string { return "127.0.0.1:" + strconv.Itoa(port) }

func Run(ctx context.Context, cmd *cobra.Command, cfg Config, flags Flags, target string) error {
	repoDir, brainDir, branch, err := cfg.Deps.ResolveFactsTarget(ctx, cfg.Options, target, flags.Branch)
	if err != nil {
		return err
	}

	// Best-effort: an unbuilt brain still serves — the UI shows an empty state
	// ("run `entire brain refresh`") rather than failing the command.
	var manifest *bmanifest.Manifest
	if m, serr := cfg.Deps.BrainManifest(ctx, cfg.Options, target); serr == nil {
		manifest = m
	}
	guard, err := bmanifest.LoadSessionReadGuard(brainDir, manifest)
	if err != nil {
		return err
	}

	srv := &Server{
		RepoDir: repoDir, BrainDir: brainDir, Branch: branch, Manifest: manifest, Deps: cfg.Deps,
		Guard: guard, GuardObserved: !guard.Empty(),
	}
	// The manifest's RepoKey is the canonical key (it survives nested owner
	// groups and any store-layout change); the brainDir tail is the fallback.
	if manifest != nil && manifest.RepoKey != "" {
		srv.Provider, srv.Owner, srv.Repo = parseRepoFromKey(manifest.RepoKey)
	} else {
		srv.Provider, srv.Owner, srv.Repo = parseRepoFromBrainDir(brainDir)
	}

	// Build the handler before binding so a broken embed fails the command
	// outright instead of printing a URL that serves a blank UI.
	handler, err := srv.Mux()
	if err != nil {
		return err
	}

	// Loopback only — never 0.0.0.0. The interface is a personal, read-only view
	// of local data and must not be reachable off-host.
	ln, err := net.Listen("tcp", vizListenAddr(flags.Port))
	if err != nil {
		return fmt.Errorf("bind viz server: %w", err)
	}
	url := "http://" + ln.Addr().String()
	fmt.Fprintf(cmd.OutOrStdout(), "entire brain viz — %s\n", url)
	fmt.Fprintln(cmd.OutOrStdout(), "Read-only, loopback-only, no network. Press Ctrl-C to stop.")
	if !flags.NoOpen {
		openBrowser(url)
	}

	// Full timeout set, not just headers: even loopback-only, another local
	// process could hold connections open and pin goroutines. WriteTimeout is
	// generous because it spans handler time too, and a maxed node-count slider
	// legitimately produces a large response.
	httpSrv := &http.Server{
		Handler:           SecurityHeaders(handler),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}
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

func (s *Server) Mux() (http.Handler, error) {
	m := http.NewServeMux()
	m.HandleFunc("/api/summary", s.HandleSummary)
	m.HandleFunc("/api/graph", s.HandleGraph)
	m.HandleFunc("/api/node", s.HandleNode)
	m.HandleFunc("/api/search", s.HandleSearch)
	m.HandleFunc("/api/facts", s.HandleFacts)
	m.HandleFunc("/api/sessions", s.HandleSessions)
	m.HandleFunc("/api/session/replay", s.HandleSessionReplay)
	m.HandleFunc("/api/history", s.HandleHistory)
	m.HandleFunc("/api/docs", s.HandleDocs)
	// A missing embed is a build error — fail the command loudly rather than
	// serving an API with a blank, unexplained 404 UI at /.
	sub, err := fs.Sub(entirebrain.WebUI, "webui/dist")
	if err != nil {
		return nil, fmt.Errorf("embedded web UI unavailable (build error): %w", err)
	}
	m.Handle("/", http.FileServer(http.FS(sub)))
	return s.privacyResponseMiddleware(m), nil
}

// bufferedVizResponse keeps API status, headers, and body private until the
// complete response can be revalidated against the exact privacy policy under
// which the handler assembled it. Static UI assets contain no brain data and
// bypass this buffer.
type bufferedVizResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *bufferedVizResponse) Header() http.Header { return w.header }

func (w *bufferedVizResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *bufferedVizResponse) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(p)
}

func copyHTTPHeaders(dst, src http.Header) {
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
}

// privacyResponseMiddleware is the HTTP equivalent of the CLI's buffered final
// writer. A tombstone cannot linearize between the last policy check and the
// first response byte because both use the same per-Brain write lock.
func (s *Server) privacyResponseMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		policy, err := brainstore.CaptureRetrievalPrivacyPolicy(s.BrainDir)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		buffered := &bufferedVizResponse{header: make(http.Header)}
		next.ServeHTTP(buffered, r)
		status := buffered.status
		if status == 0 {
			status = http.StatusOK
		}
		err = s.Deps.CommitRetrievalResponse([]brainstore.RetrievalPrivacyPolicy{policy}, func() error {
			copyHTTPHeaders(w.Header(), buffered.header)
			w.WriteHeader(status)
			if buffered.body.Len() == 0 {
				return nil
			}
			n, writeErr := w.Write(buffered.body.Bytes())
			if writeErr == nil && n != buffered.body.Len() {
				return io.ErrShortWrite
			}
			return writeErr
		})
		if err != nil {
			// The real response has not been committed when validation fails, so
			// return a typed, content-free failure instead of stale API bytes.
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
		}
	})
}

// SecurityHeaders makes the no-egress guarantee machine-checkable: the CSP
// forbids any off-origin fetch (script/style/img/font/connect all 'self'), so
// even a compromised asset can't call home. Structural, not just a comment.
func SecurityHeaders(next http.Handler) http.Handler {
	const csp = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; font-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'; object-src 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// ---- JSON view models (clean shapes; never leak raw semstore.Record) ----

type Node struct {
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

type Edge struct {
	From       string  `json:"from"`
	To         string  `json:"to"`
	Type       string  `json:"type"`
	Confidence float64 `json:"confidence,omitempty"`
	Scope      string  `json:"scope,omitempty"`
	Resolution string  `json:"resolution,omitempty"`
}

type GraphResp struct {
	Nodes     []Node   `json:"nodes"`
	Edges     []Edge   `json:"edges"`
	Total     int      `json:"total,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
}

type NodeResp struct {
	Symbol    Node     `json:"symbol"`
	Neighbors []Node   `json:"neighbors"`
	Relations []Edge   `json:"relations"`
	Snippet   string   `json:"snippet,omitempty"`
	Link      string   `json:"link,omitempty"`
	LinkLabel string   `json:"link_label,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
}

type Counts struct {
	Symbols   int `json:"symbols"`
	Relations int `json:"relations"`
	Files     int `json:"files"`
	History   int `json:"history"`
	Facts     int `json:"facts"`
	Sessions  int `json:"sessions"`
	Docs      int `json:"docs"`
}

type SummaryResp struct {
	Repo        string   `json:"repo"`
	Branch      string   `json:"branch"`
	GeneratedAt string   `json:"generated_at,omitempty"`
	Counts      Counts   `json:"counts"`
	Warnings    []string `json:"warnings,omitempty"`
}

type Hit struct {
	Source string  `json:"source"`
	ID     string  `json:"id"`
	Title  string  `json:"title,omitempty"`
	Text   string  `json:"text,omitempty"`
	Kind   string  `json:"kind,omitempty"`
	Path   string  `json:"path,omitempty"`
	Line   int     `json:"line,omitempty"`
	Score  float64 `json:"score,omitempty"`
}

type SearchResp struct {
	Hits []Hit `json:"hits"`
}

func nodeJSON(r semstore.Record) Node {
	return Node{
		ID: r.ID, Name: r.Name, QualifiedName: r.QualifiedName, Kind: r.Kind,
		File: r.FilePath, Line: r.StartLine, Language: r.Language,
		Signature: r.Signature, ContainerID: r.ContainerID,
	}
}

func edgeJSON(r semstore.Record) Edge {
	return Edge{From: r.FromID, To: r.ToID, Type: r.Type, Confidence: r.Confidence, Scope: r.RelationScope, Resolution: r.Resolution}
}

// ---- handlers ----

// repoDisplayName is a stable, path-free repo identifier for the UI: the forge
// coordinates when known, else the repo directory's basename. Never the
// absolute local path — the frontend splits on '/' (wrong on Windows) and the
// filesystem layout is nobody's business.
func (s *Server) repoDisplayName() string {
	if s.Provider != "" {
		return s.Provider + "/" + s.Owner + "/" + s.Repo
	}
	if s.RepoDir == "" {
		return ""
	}
	return filepath.Base(strings.TrimRight(s.RepoDir, `/\`))
}

func (s *Server) HandleSummary(w http.ResponseWriter, r *http.Request) {
	guard, ok := s.requestSessionGuard(w)
	if !ok {
		return
	}
	resp := SummaryResp{Repo: s.repoDisplayName(), Branch: s.Branch}
	// Counts come straight from the manifest captured at startup. Rebuilding the
	// full status report here costs several seconds on large brains and would
	// block the very first paint of the hub — so we don't. docstore.Load is cheap.
	if m := s.Manifest; m != nil && m.Sources != nil {
		if sem := m.Sources.Semantic; sem != nil {
			resp.Counts.Symbols = sem.Symbols
			resp.Counts.Relations = sem.Relations
			resp.Counts.Files = sem.Files
			if !sem.GeneratedAt.IsZero() {
				resp.GeneratedAt = sem.GeneratedAt.UTC().Format(time.RFC3339)
			}
		}
		if h := m.Sources.History; h != nil {
			resp.Counts.History = h.Records
		}
		if f := m.Sources.Facts; f != nil {
			resp.Counts.Facts = f.Facts
		}
		if ss := m.Sources.Sessions; ss != nil {
			resp.Counts.Sessions = len(s.sessionsFromManifest(guard))
		}
	}
	if docs, err := docstore.Load(s.BrainDir); err == nil {
		resp.Counts.Docs = len(docs.Records)
	}
	writeJSONHTTP(w, http.StatusOK, resp)
}

// sessionsFromManifest returns the export sessions captured in the manifest.
func (s *Server) sessionsFromManifest(guard brainstore.SessionReadGuard) []bmanifest.Session {
	if s.Manifest == nil || s.Manifest.Sources == nil || s.Manifest.Sources.Sessions == nil {
		return nil
	}
	return bmanifest.GuardSessions(guard, s.Manifest.Sources.Sessions.Sessions)
}

// requestSessionGuard reloads privacy state at every HTTP request boundary.
// The visualization intentionally pins its manifest for a stable graph, but a
// privacy exclusion must take effect without restarting that long-lived
// process. Present invalid state returns a typed 503 and no derived content.
// If the policy file unexpectedly disappears, retain the startup guard as a
// conservative denial until an explicit current (possibly empty) policy is
// written or the server restarts.
func (s *Server) requestSessionGuard(w http.ResponseWriter) (brainstore.SessionReadGuard, bool) {
	// Serialize refreshes so one request cannot replace a newer valid policy
	// snapshot with an older one while another request is reading it.
	s.guardMu.Lock()
	defer s.guardMu.Unlock()

	stones, state, err := brainstore.LoadSessionTombstonesChecked(s.BrainDir)
	if err != nil {
		writeJSONHTTP(w, http.StatusServiceUnavailable, map[string]any{
			"error": cmdutil.CommandErrorCode(err), "message": "session privacy state is unavailable",
		})
		return brainstore.SessionReadGuard{}, false
	}
	if state.State == brainstore.SessionTombstoneAbsent && s.GuardObserved {
		return s.Guard, true
	}
	if state.State == brainstore.SessionTombstoneCurrent {
		s.GuardObserved = true
		if len(stones.Excluded) == 0 {
			s.Guard = brainstore.SessionReadGuard{}
		} else {
			s.Guard = brainstore.SessionReadGuard{IDs: stones.Excluded, Paths: bmanifest.ExcludedTranscriptPaths(s.Manifest, stones)}
		}
		return s.Guard, true
	}
	return s.Guard, true
}

// vizQueryLimit parses ?limit with the shared safety policy: invalid input
// keeps the default, and both the 0 "no cap" sentinel and anything above the
// ceiling clamp to vizGraphMaxView — no endpoint serves an unbounded graph.
func vizQueryLimit(r *http.Request, def int) int {
	limit := def
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			limit = n
		}
	}
	if limit <= 0 || limit > vizGraphMaxView {
		limit = vizGraphMaxView
	}
	return limit
}

// ---- feature graph endpoints (each brain feature rendered as a graph) ----

// GNode is a node in a feature graph. Group drives its color/shape in the UI;
// Link is a real, clickable URL (an entire.io session page, a source file, ...).
type GNode struct {
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

type GEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Type string `json:"type,omitempty"`
}

type FeatureGraph struct {
	Nodes     []GNode  `json:"nodes"`
	Edges     []GEdge  `json:"edges"`
	Total     int      `json:"total"`
	Truncated bool     `json:"truncated,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
}

const (
	colorFact    = "#fbbf24"
	ColorSession = "#818cf8"
	ColorHistory = "#f25333"
	colorDoc     = "#34d399"
)

// vizLinkSlugs are the forge slugs whose brain keys map to real web URLs —
// derived once at init from the canonical slug table (base.KnownRepoDomainSlugs,
// env.go) so a newly supported forge gets links here automatically instead of
// silently losing them to a stale hardcoded subset.
var vizLinkSlugs = func() map[string]bool {
	out := make(map[string]bool, len(base.KnownRepoDomainSlugs))
	for _, slug := range base.KnownRepoDomainSlugs {
		out[slug] = true
	}
	return out
}()

// parseRepoFromKey recovers provider/owner/repo from a canonical repo key
// (e.g. "gh/acme/app", or "gl/group/sub/app" for nested groups — everything
// between the slug and the final segment is the owner path). Empty provider =
// a local repo with no forge, hence no web links.
func parseRepoFromKey(key string) (provider, owner, repo string) {
	parts := strings.Split(strings.Trim(strings.TrimSpace(key), "/"), "/")
	if len(parts) < 3 || !vizLinkSlugs[parts[0]] {
		return "", "", ""
	}
	o := strings.Join(parts[1:len(parts)-1], "/")
	r := parts[len(parts)-1]
	if o == "" || r == "" {
		return "", "", ""
	}
	return parts[0], o, r
}

// parseRepoFromBrainDir is the fallback when no manifest records the repo key:
// the key is the tail of brainDir (e.g. .../repos/gh/acme/app). Only the plain
// slug/owner/repo shape is recoverable from a path tail; keys with nested
// owner groups need the manifest's RepoKey.
func parseRepoFromBrainDir(brainDir string) (provider, owner, repo string) {
	parts := strings.Split(strings.Trim(filepath.ToSlash(brainDir), "/"), "/")
	if len(parts) < 3 {
		return "", "", ""
	}
	return parseRepoFromKey(strings.Join(parts[len(parts)-3:], "/"))
}

func vizWebBase() string {
	if v := strings.TrimSpace(os.Getenv("ENTIRE_WEB_BASE_URL")); v != "" {
		// Only an absolute http(s) base may override: the value flows into every
		// generated link, so a javascript:/data: scheme here would be link
		// injection into the UI.
		if u, err := url.Parse(v); err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
			return strings.TrimRight(v, "/")
		}
	}
	return "https://entire.io"
}

// sessionLink is the real entire.io page for a session.
func (s *Server) sessionLink(id string) string {
	id = strings.TrimSpace(id)
	if id == "" || s.Provider == "" {
		return ""
	}
	return fmt.Sprintf("%s/%s/%s/%s/session/%s", vizWebBase(), s.Provider, s.Owner, s.Repo, url.PathEscape(id))
}

// vizEscapePath percent-encodes each segment of a slash-separated path so
// branch names and file paths containing '#', '?', spaces, etc. survive URL
// interpolation, while the segment-separating slashes stay literal.
func vizEscapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// sourceLink is the forge page for a source file (GitHub blob for gh repos).
func (s *Server) sourceLink(file string, line int) string {
	// Store paths are not normalized on write, so on Windows they can carry
	// backslashes — which vizEscapePath would percent-encode into a broken URL.
	file = strings.TrimSpace(filepath.ToSlash(file))
	if file == "" || s.Provider != "gh" {
		return ""
	}
	branch := s.Branch
	if branch == "" {
		branch = "HEAD"
	}
	u := fmt.Sprintf("https://github.com/%s/%s/blob/%s/%s", s.Owner, s.Repo, vizEscapePath(branch), vizEscapePath(file))
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
// dedupEdges drops duplicate undirected edges (keeping the first), so combining
// several link passes (e.g. shared-topic + same-file) never double-counts a pair.
func dedupEdges(edges []GEdge) []GEdge {
	seen := make(map[string]bool, len(edges))
	out := edges[:0]
	for _, e := range edges {
		k := vizEdgeKey(e.From, e.To)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, e)
	}
	return out
}

func linkBySharedKey(keyToIDs map[string][]string, typ string, capPerKey int) []GEdge {
	var edges []GEdge
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
				edges = append(edges, GEdge{From: ids[i], To: ids[j], Type: typ})
			}
		}
	}
	return edges
}

// factBranches lists the branches whose facts to show — the current branch first,
// then every branch the manifest records. Facts are branch-scoped on disk, but
// the Facts view is a whole-brain view, so it aggregates across branches.
func (s *Server) factBranches() []string {
	out := []string{}
	seen := map[string]bool{}
	add := func(b string) {
		b = strings.TrimSpace(b)
		if b != "" && !seen[b] {
			seen[b] = true
			out = append(out, b)
		}
	}
	add(s.Branch)
	if s.Manifest != nil && s.Manifest.Sources != nil && s.Manifest.Sources.Facts != nil {
		for _, b := range s.Manifest.Sources.Facts.Branches {
			add(b)
		}
	}
	if len(out) == 0 {
		out = append(out, s.Branch)
	}
	return out
}

func (s *Server) HandleFacts(w http.ResponseWriter, r *http.Request) {
	guard, ok := s.requestSessionGuard(w)
	if !ok {
		return
	}
	limit := vizQueryLimit(r, 3000)
	seen := map[string]bool{}
	all := []factmerge.Record{}
	var warnings []string
	for _, br := range s.factBranches() {
		fs, err := factstore.Load(s.BrainDir, br)
		if err != nil {
			warnings = append(warnings, "facts["+br+"]: "+err.Error())
			continue
		}
		fs = factstore.GuardRecords(guard, fs)
		for _, f := range fs {
			// Graph node IDs must be unique and non-empty; a record without one
			// (corrupt or hand-edited store) would collide with its siblings in
			// the frontend's byId map. factstore.Load doesn't validate IDs — skip here.
			if f.ID == "" || seen[f.ID] {
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
	nodes := make([]GNode, 0, len(all))
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
		nodes = append(nodes, GNode{ID: f.ID, Name: vizShortLabel(f.Text, 6), Kind: f.Kind, Group: "fact", Color: colorFact, Text: f.Text, Meta: vizJoinMeta(f.Kind, f.Status, f.Origin, conf), Link: link, LinkLabel: label})
		for _, l := range f.Locus {
			key := strings.ToLower(strings.TrimSpace(l))
			if key != "" {
				locusMap[key] = append(locusMap[key], f.ID)
			}
		}
	}
	edges := []GEdge{}
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
			edges = append(edges, GEdge{From: f.ID, To: rid, Type: "related"})
		}
	}
	edges = append(edges, linkBySharedKey(locusMap, "shared locus", 6)...)
	writeJSONHTTP(w, http.StatusOK, FeatureGraph{Nodes: nodes, Edges: edges, Total: total, Truncated: trunc, Warnings: warnings})
}

// vizSessionFallbackText builds a compact, factual markdown body for a session
// with no recorded summary, so its sidebar is never blank. It leans on data we
// already have — the basenames of the files it touched (capped) — and falls back
// to a short note only when truly nothing is known.
func vizSessionFallbackText(se bmanifest.Session) string {
	fps := make([]string, 0, len(se.FilesTouched))
	for _, fp := range se.FilesTouched {
		if fp = strings.TrimSpace(fp); fp != "" {
			fps = append(fps, fp)
		}
	}
	if len(fps) == 0 {
		return "No summary recorded."
	}
	const maxFiles = 8
	shown := fps
	if len(shown) > maxFiles {
		shown = shown[:maxFiles]
	}
	var b strings.Builder
	b.WriteString("**Files touched**\n")
	for _, fp := range shown {
		b.WriteString("- " + filepath.Base(filepath.ToSlash(fp)) + "\n")
	}
	if extra := len(fps) - len(shown); extra > 0 {
		fmt.Fprintf(&b, "- _(+%d more)_\n", extra)
	}
	return strings.TrimRight(b.String(), "\n")
}

func (s *Server) HandleSessions(w http.ResponseWriter, r *http.Request) {
	guard, ok := s.requestSessionGuard(w)
	if !ok {
		return
	}
	limit := vizQueryLimit(r, 3000)
	// Same defense as facts/history: node IDs must be unique and non-empty.
	// Filter before counting so Total and the slider's "N of total" stay
	// consistent with the nodes actually served.
	all := s.sessionsFromManifest(guard)
	sessions := make([]bmanifest.Session, 0, len(all))
	for _, se := range all {
		if se.SessionID != "" {
			sessions = append(sessions, se)
		}
	}
	total := len(sessions)
	trunc := limit > 0 && total > limit
	if trunc {
		sessions = sessions[:limit]
	}
	nodes := make([]GNode, 0, len(sessions))
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
			text = strings.TrimSpace(base.FirstNonEmpty(se.Summary.Intent, se.Summary.Outcome))
		}
		if text == "" {
			// No recorded summary → synthesize a factual body from the files it
			// touched (as markdown, so the sidebar renders a real list) so the
			// session's detail pane is never blank.
			text = vizSessionFallbackText(se)
		}
		link := s.sessionLink(se.SessionID)
		label := ""
		if link != "" {
			label = "Open in Entire"
		}
		nodes = append(nodes, GNode{ID: se.SessionID, Name: name, Kind: se.Kind, Group: "session", Color: ColorSession, Text: text, Meta: vizJoinMeta(created, files, cps), Link: link, LinkLabel: label})
		for _, fp := range se.FilesTouched {
			if fp = strings.TrimSpace(fp); fp != "" {
				fileMap[fp] = append(fileMap[fp], se.SessionID)
			}
		}
	}
	edges := linkBySharedKey(fileMap, "shared file", 8)
	writeJSONHTTP(w, http.StatusOK, FeatureGraph{Nodes: nodes, Edges: edges, Total: total, Truncated: trunc})
}

type ReplayStep struct {
	File string   `json:"file"`
	IDs  []string `json:"ids"`
}

type ReplayResp struct {
	Session  *GNode       `json:"session,omitempty"`
	Files    []string     `json:"files"`
	Steps    []ReplayStep `json:"steps"`
	Nodes    []GNode      `json:"nodes"`
	Edges    []GEdge      `json:"edges"`
	Total    int          `json:"total"`
	Warnings []string     `json:"warnings,omitempty"`
}

// vizReplayMaxNodes bounds a replay subgraph so a session that touched hundreds
// of files can't drag the force sim to its knees.
const vizReplayMaxNodes = 1500

// HandleSessionReplay returns a session's touched-file symbols as a focused
// semantic subgraph plus an ordered, per-file step list. The UI "replays" the
// session across the code graph: light up each file's symbols in turn, tracing
// the path the agent took through the codebase.
func (s *Server) HandleSessionReplay(w http.ResponseWriter, r *http.Request) {
	guard, ok := s.requestSessionGuard(w)
	if !ok {
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	resp := ReplayResp{Files: []string{}, Steps: []ReplayStep{}, Nodes: []GNode{}, Edges: []GEdge{}}
	if id == "" {
		writeJSONHTTP(w, http.StatusBadRequest, ReplayResp{Warnings: []string{"missing id"}})
		return
	}
	sessions := s.sessionsFromManifest(guard)
	var se *bmanifest.Session
	for i := range sessions {
		if sessions[i].SessionID == id {
			se = &sessions[i]
			break
		}
	}
	if se == nil {
		writeJSONHTTP(w, http.StatusNotFound, ReplayResp{Warnings: []string{"session not found: " + id}})
		return
	}

	created := ""
	if !se.CreatedAt.IsZero() {
		created = se.CreatedAt.UTC().Format("2006-01-02 15:04Z")
	}
	name := vizJoinMeta(se.Agent, se.Model)
	if name == "" {
		name = "session " + vizShortID(se.SessionID)
	}
	text := ""
	if se.Summary != nil {
		text = strings.TrimSpace(base.FirstNonEmpty(se.Summary.Intent, se.Summary.Outcome))
	}
	if text == "" {
		// No recorded summary → synthesize a factual body from the files it
		// touched (as markdown, so the sidebar renders a real list) so the
		// session's detail pane is never blank.
		text = vizSessionFallbackText(*se)
	}
	link := s.sessionLink(se.SessionID)
	sessLabel := ""
	if link != "" {
		sessLabel = "Open in Entire"
	}
	resp.Session = &GNode{
		ID: se.SessionID, Name: name, Kind: se.Kind, Group: "session", Color: ColorSession, Text: text,
		Meta: vizJoinMeta(created, fmt.Sprintf("%d files", len(se.FilesTouched)), fmt.Sprintf("%d checkpoints", se.CheckpointsCount)),
		Link: link, LinkLabel: sessLabel,
	}

	touched := map[string]bool{}
	for _, fp := range se.FilesTouched {
		fp = strings.TrimSpace(filepath.ToSlash(fp))
		if fp != "" && !touched[fp] {
			touched[fp] = true
			resp.Files = append(resp.Files, fp)
		}
	}
	if len(resp.Files) == 0 {
		resp.Warnings = append(resp.Warnings, "this session records no touched files")
		writeJSONHTTP(w, http.StatusOK, resp)
		return
	}

	sem := s.semanticSource()
	if sem == nil {
		resp.Warnings = append(resp.Warnings, "no code graph — run `"+base.BrainInvocation(os.LookupEnv, base.SemanticIndexVerb)+"`")
		writeJSONHTTP(w, http.StatusOK, resp)
		return
	}
	// By-file fetch (indexed on SQLite, filtered scan on a snapshot) instead of
	// loading the whole symbol table to keep a handful of files' worth. The cap
	// sits well above vizReplayMaxNodes so truncation happens at the node
	// budget below, not mid-file here.
	syms, err := semstore.SymbolsForFiles(s.BrainDir, sem, resp.Files, vizReplayMaxNodes*4)
	if err != nil {
		resp.Warnings = append(resp.Warnings, "code graph symbols: "+err.Error())
		writeJSONHTTP(w, http.StatusOK, resp)
		return
	}
	byFile := map[string][]semstore.Record{}
	for i := range syms {
		f := filepath.ToSlash(syms[i].FilePath)
		if touched[f] {
			byFile[f] = append(byFile[f], syms[i])
		}
	}

	idset := map[string]bool{}
	kept := []semstore.Record{}
	for _, fp := range resp.Files {
		step := ReplayStep{File: fp, IDs: []string{}}
		for i := range byFile[fp] {
			sy := byFile[fp][i]
			if !idset[sy.ID] {
				if len(kept) >= vizReplayMaxNodes {
					continue
				}
				idset[sy.ID] = true
				kept = append(kept, sy)
				resp.Nodes = append(resp.Nodes, GNode{
					ID: sy.ID, Name: sy.Name, Kind: sy.Kind, Group: sy.Kind,
					File: filepath.ToSlash(sy.FilePath), Line: sy.StartLine,
					Link: s.sourceLink(sy.FilePath, sy.StartLine), LinkLabel: vizSourceLabel(s.Provider),
				})
			}
			step.IDs = append(step.IDs, sy.ID)
		}
		resp.Steps = append(resp.Steps, step)
	}
	resp.Total = len(resp.Nodes)

	if len(kept) > 0 {
		if rels, rerr := s.relationsForSymbols(sem, kept, len(kept)*8); rerr == nil {
			for i := range rels {
				e := rels[i]
				if idset[e.FromID] && idset[e.ToID] {
					resp.Edges = append(resp.Edges, GEdge{From: e.FromID, To: e.ToID, Type: e.Type})
				}
			}
		}
	}
	writeJSONHTTP(w, http.StatusOK, resp)
}

func vizSourceLabel(provider string) string {
	if provider == "gh" {
		return "View source"
	}
	return ""
}

func vizHistoryColor(kind string) string {
	switch k := strings.ToLower(kind); {
	case strings.Contains(k, "decision"):
		return ColorSession
	case strings.Contains(k, "learning"):
		return "#22d3ee"
	default:
		return ColorHistory
	}
}

func (s *Server) HandleHistory(w http.ResponseWriter, r *http.Request) {
	guard, ok := s.requestSessionGuard(w)
	if !ok {
		return
	}
	// History can hold hundreds of thousands of records — far too many to render
	// legibly — so it stays capped (the UI shows an honest "N of total"), but the
	// node-count slider lets you dial it up. The 0 sentinel and the vizGraphMaxView
	// safety ceiling are enforced inside vizQueryLimit for every endpoint.
	limit := vizQueryLimit(r, 1200)
	if s.Manifest == nil || s.Manifest.Sources == nil || s.Manifest.Sources.History == nil {
		writeJSONHTTP(w, http.StatusOK, FeatureGraph{Nodes: []GNode{}, Edges: []GEdge{}, Warnings: []string{"no history yet — run `" + base.BrainInvocation(os.LookupEnv, "refresh") + "`"}})
		return
	}
	idx, err := historystore.LoadIndex(s.BrainDir, s.Manifest.Sources.History)
	if err != nil {
		writeJSONHTTP(w, http.StatusOK, FeatureGraph{Nodes: []GNode{}, Edges: []GEdge{}, Warnings: []string{"history unavailable: " + err.Error()}})
		return
	}
	// Same defense as facts/docs: node IDs must be unique and non-empty, and
	// the timeline spine below chains recs by ID — filter before counting.
	recs := make([]historystore.Record, 0, len(idx.Records))
	seen := make(map[string]bool, len(idx.Records))
	for _, h := range idx.Records {
		if h.ID == "" || seen[h.ID] || historystore.GuardBlocksRecord(guard, h) {
			continue
		}
		seen[h.ID] = true
		recs = append(recs, h)
	}
	total := len(recs)
	trunc := limit > 0 && total > limit
	if trunc {
		recs = recs[:limit]
	}
	nodes := make([]GNode, 0, len(recs))
	termMap := map[string][]string{}
	pathMap := map[string][]string{}
	for _, h := range recs {
		nodes = append(nodes, GNode{ID: h.ID, Name: vizShortLabel(h.Summary, 7), Kind: h.Kind, Group: "history", Color: vizHistoryColor(h.Kind), Text: h.Summary, Meta: vizJoinMeta(h.Kind, h.Branch, h.Path)})
		for _, t := range h.Terms {
			key := strings.ToLower(strings.TrimSpace(t))
			if len(key) > 2 {
				termMap[key] = append(termMap[key], h.ID)
			}
		}
		if p := strings.TrimSpace(filepath.ToSlash(h.Path)); p != "" {
			pathMap[p] = append(pathMap[p], h.ID)
		}
	}
	// Timeline spine first: chain consecutive records so history reads as one
	// connected stream over time rather than a scatter of isolated points.
	// Spine before shared-key edges so the edge budget below truncates the
	// fill-in edges, never the connectivity.
	edges := make([]GEdge, 0, len(recs))
	for i := 1; i < len(recs); i++ {
		edges = append(edges, GEdge{From: recs[i-1].ID, To: recs[i].ID, Type: "then"})
	}
	// Link by shared topic AND co-located history (same file); records touching
	// the same file are related, which fills in the otherwise-sparse graph.
	edges = append(edges, linkBySharedKey(termMap, "shared topic", 8)...)
	edges = append(edges, linkBySharedKey(pathMap, "same file", 8)...)
	edges = dedupEdges(edges)
	// Same scaled edge budget as HandleGraph — the renderer is tuned for at
	// most vizGraphMaxEdges regardless of which feature produced the graph.
	edgeCap := len(recs) * 4
	if edgeCap < 12000 {
		edgeCap = 12000
	}
	if edgeCap > vizGraphMaxEdges {
		edgeCap = vizGraphMaxEdges
	}
	if len(edges) > edgeCap {
		edges = edges[:edgeCap]
	}
	writeJSONHTTP(w, http.StatusOK, FeatureGraph{Nodes: nodes, Edges: edges, Total: total, Truncated: trunc})
}

func (s *Server) HandleDocs(w http.ResponseWriter, r *http.Request) {
	limit := vizQueryLimit(r, 2000)
	idx, err := docstore.Load(s.BrainDir)
	if err != nil {
		writeJSONHTTP(w, http.StatusOK, FeatureGraph{Nodes: []GNode{}, Edges: []GEdge{}, Warnings: []string{"docs unavailable: " + err.Error()}})
		return
	}
	// Same defense as facts/history: an empty ID (corrupt/hand-edited index)
	// would make every such record collide on the node ID "doc:". Filter
	// before counting so Total and the slider's "N of total" stay consistent.
	recs := make([]docstore.Record, 0, len(idx.Records))
	for _, d := range idx.Records {
		if d.ID != "" {
			recs = append(recs, d)
		}
	}
	total := len(recs)
	trunc := limit > 0 && total > limit
	if trunc {
		recs = recs[:limit]
	}
	nodes := make([]GNode, 0, len(recs))
	pathMap := map[string][]string{}
	for _, d := range recs {
		name := base.FirstNonEmpty(d.Heading, filepath.Base(d.Path))
		text := d.Text
		if len(text) > 500 {
			text = text[:500]
		}
		link := s.sourceLink(d.Path, d.Line)
		label := ""
		if link != "" {
			label = "View source"
		}
		// "doc:" prefix matches the unified retrieval convention (retrieve.go),
		// so /api/search doc hits resolve to these nodes in the UI.
		id := "doc:" + d.ID
		nodes = append(nodes, GNode{ID: id, Name: name, Group: "doc", Color: colorDoc, Text: text, Meta: d.Path, File: d.Path, Line: d.Line, Link: link, LinkLabel: label})
		if d.Path != "" {
			pathMap[d.Path] = append(pathMap[d.Path], id)
		}
	}
	edges := linkBySharedKey(pathMap, "same file", 8)
	writeJSONHTTP(w, http.StatusOK, FeatureGraph{Nodes: nodes, Edges: edges, Total: total, Truncated: trunc})
}

func vizShortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// semanticSymbolPool returns the first `pool` symbols, serving them from the
// largest prefix already loaded when possible (see the symPool field docs).
// Callers must not mutate the returned slice's records.
func (s *Server) semanticSymbolPool(sem *bmanifest.SemanticSource, pool int) ([]semstore.Record, error) {
	if pool <= 0 {
		return semstore.LoadSymbols(s.BrainDir, sem, pool)
	}
	s.symMu.Lock()
	defer s.symMu.Unlock()
	if s.symPoolFull || len(s.symPool) >= pool {
		if len(s.symPool) > pool {
			return s.symPool[:pool], nil
		}
		return s.symPool, nil
	}
	syms, err := semstore.LoadSymbols(s.BrainDir, sem, pool)
	if err != nil {
		return nil, err
	}
	s.symPool = syms
	s.symPoolFull = len(syms) < pool
	return syms, nil
}

func (s *Server) semanticSource() *bmanifest.SemanticSource {
	if s.Manifest == nil || s.Manifest.Sources == nil {
		return nil
	}
	return s.Manifest.Sources.Semantic
}

func (s *Server) HandleGraph(w http.ResponseWriter, r *http.Request) {
	// `limit` is the number of symbols to RENDER (the node-count slider drives it).
	// We load a wider pool, rank it by degree, and keep the top `limit` so the view
	// is always the most-connected slice at any size the user picks. vizQueryLimit
	// owns the shared policy: 0 ("render all") and oversized values clamp to the
	// vizGraphMaxView safety ceiling.
	view := vizQueryLimit(r, vizGraphViewCap)
	// Scale the edge budget with the node count so a bigger view isn't artificially
	// sparse. ~4 edges/node reads as a real constellation; floored so small views
	// still show structure, ceilinged so a huge view can't blow up payload/draw.
	edgeCap := view * 4
	if edgeCap < 12000 {
		edgeCap = 12000
	}
	if edgeCap > vizGraphMaxEdges {
		edgeCap = vizGraphMaxEdges
	}
	resp := GraphResp{Nodes: []Node{}, Edges: []Edge{}}
	sem := s.semanticSource()
	if sem == nil {
		resp.Warnings = append(resp.Warnings, "no code graph yet — run `"+base.BrainInvocation(os.LookupEnv, base.SemanticIndexVerb)+"`")
		writeJSONHTTP(w, http.StatusOK, resp)
		return
	}
	pool := view * 3
	if pool < 4000 {
		pool = 4000
	}
	// The pool is ranking headroom, not render size — cap it at the same
	// safety ceiling so a maxed slider can't triple the documented worst case.
	// As view approaches the ceiling the headroom shrinks until view == pool,
	// where ranking degenerates to keep-everything — which is exactly what
	// "render all" means at that size.
	if pool > vizGraphMaxView {
		pool = vizGraphMaxView
	}
	syms, err := s.semanticSymbolPool(sem, pool)
	if err != nil {
		resp.Warnings = append(resp.Warnings, "code graph symbols: "+err.Error())
		writeJSONHTTP(w, http.StatusOK, resp)
		return
	}
	if len(syms) == 0 {
		resp.Warnings = append(resp.Warnings, "the code index has no symbols yet — run `"+base.BrainInvocation(os.LookupEnv, base.SemanticIndexVerb)+"`")
		writeJSONHTTP(w, http.StatusOK, resp)
		return
	}
	resp.Total = sem.Symbols

	// Rank the pool by degree so any `view` size keeps the most-connected symbols
	// (semstore.LoadSymbols returns storage order, so an unranked cap keeps leaves).
	poolSet := make(map[string]bool, len(syms))
	for i := range syms {
		poolSet[syms[i].ID] = true
	}
	// Relations feed degree ranking (which wants a wider sample than the render
	// budget) and then edge selection (hard-capped at edgeCap). Past a few
	// multiples of the max edge budget, extra relations barely move the ranking
	// but cost real load/merge time — ceiling the fetch keeps a 300k-node
	// request from pulling millions of rows it can never render.
	relLimit := pool * 4
	if relLimit > 4*vizGraphMaxEdges {
		relLimit = 4 * vizGraphMaxEdges
	}
	var rels []semstore.Record
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
	if view > 0 && len(syms) > view {
		ranked := make([]semstore.Record, len(syms))
		copy(ranked, syms)
		sort.SliceStable(ranked, func(i, j int) bool { return deg[ranked[i].ID] > deg[ranked[j].ID] })
		kept = ranked[:view]
	}
	keptSet := make(map[string]bool, len(kept))
	for i := range kept {
		resp.Nodes = append(resp.Nodes, nodeJSON(kept[i]))
		keptSet[kept[i].ID] = true
	}
	// Cap edges for render/sim budget, but choose them for COVERAGE first: a naive
	// first-N cap piles all edges onto a few hubs and leaves most nodes edgeless, so
	// they fly apart under repulsion and the whole graph fits to a dot. Pass 1 keeps
	// edges that connect a still-unconnected node; pass 2 fills the remaining budget.
	seen := make(map[string]bool)
	covered := make(map[string]int)
	addEdge := func(e semstore.Record) {
		k := vizEdgeKey(e.FromID, e.ToID)
		if seen[k] {
			return
		}
		seen[k] = true
		resp.Edges = append(resp.Edges, edgeJSON(e))
		covered[e.FromID]++
		covered[e.ToID]++
	}
	for i := range rels {
		if len(resp.Edges) >= edgeCap {
			break
		}
		e := rels[i]
		if keptSet[e.FromID] && keptSet[e.ToID] && (covered[e.FromID] == 0 || covered[e.ToID] == 0) {
			addEdge(e)
		}
	}
	for i := range rels {
		if len(resp.Edges) >= edgeCap {
			break
		}
		e := rels[i]
		if keptSet[e.FromID] && keptSet[e.ToID] {
			addEdge(e)
		}
	}
	resp.Truncated = sem.Symbols > len(kept)
	writeJSONHTTP(w, http.StatusOK, resp)
}

func (s *Server) HandleNode(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	resp := NodeResp{Neighbors: []Node{}, Relations: []Edge{}}
	if id == "" {
		writeJSONHTTP(w, http.StatusBadRequest, NodeResp{Warnings: []string{"missing id"}})
		return
	}
	sem := s.semanticSource()
	if sem == nil {
		resp.Warnings = append(resp.Warnings, "no code graph")
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
		writeJSONHTTP(w, http.StatusNotFound, NodeResp{Neighbors: []Node{}, Relations: []Edge{}, Warnings: []string{"symbol not found: " + id}})
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

func (s *Server) HandleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	limit := 25
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	// Hits render in the rail / ⌘K palette, so anything beyond a few hundred
	// per source is never useful — cap it so a crafted ?limit can't force an
	// arbitrarily expensive retrieval.
	if limit > vizSearchMaxHits {
		limit = vizSearchMaxHits
	}
	resp := SearchResp{Hits: []Hit{}}
	if q == "" {
		writeJSONHTTP(w, http.StatusOK, resp)
		return
	}
	// Symbol matches first so ⌘K / rail search can jump straight to a graph node.
	if sem := s.semanticSource(); sem != nil {
		if syms, _, _, err := semstore.ContextFacts(s.BrainDir, sem, q, limit, 0); err == nil {
			for _, sy := range syms {
				resp.Hits = append(resp.Hits, Hit{Source: "symbol", ID: sy.ID, Title: sy.Name, Text: sy.QualifiedName, Kind: sy.Kind, Path: sy.FilePath, Line: sy.StartLine})
			}
		}
	}
	// Memory hits (facts / history / docs), same code path as `entire brain search`.
	if u, err := s.Deps.RetrieveUnified(s.RepoDir, s.BrainDir, s.Branch, q, limit, retrieval.ModeLexical); err == nil {
		for _, h := range u {
			text := h.Text
			if h.VerificationRequired {
				text = "VERIFY AGAINST CURRENT EVIDENCE: " + text
			}
			resp.Hits = append(resp.Hits, Hit{Source: h.Source, ID: h.ID, Title: h.Heading, Text: text, Path: h.Path, Line: h.Line, Score: h.Score})
		}
	}
	writeJSONHTTP(w, http.StatusOK, resp)
}

// relationsForSymbols returns relations INCIDENT to the given symbols — either
// endpoint may be outside the set (the queries match from_id OR to_id); callers
// that need both endpoints inside filter the result themselves. Mirrors the
// SQLite-vs-snapshot branch semstore.ContextFacts uses (semantic.go).
func (s *Server) relationsForSymbols(sem *bmanifest.SemanticSource, syms []semstore.Record, limit int) ([]semstore.Record, error) {
	storePath, err := semstore.ValidateDeclaredStore(s.BrainDir, sem)
	if err != nil {
		return nil, err
	}
	return semstore.FindRelationsForSymbolsInSQLite(storePath, syms, limit)
}

// nodeDetail resolves one symbol by exact id plus its relations and neighbors,
// mirroring semstore.ContextFacts but seeded by id rather than a search query.
func (s *Server) nodeDetail(sem *bmanifest.SemanticSource, id string) (*semstore.Record, []semstore.Record, []semstore.Record, error) {
	const limit = 80
	storePath, err := semstore.ValidateDeclaredStore(s.BrainDir, sem)
	if err != nil {
		return nil, nil, nil, err
	}
	byID, err := semstore.LoadSymbolsByIDsSQLite(storePath, []string{id})
	if err != nil {
		return nil, nil, nil, err
	}
	node, ok := byID[id]
	if !ok {
		return nil, nil, nil, nil
	}
	rels, err := semstore.FindRelationsForSymbolsInSQLite(storePath, []semstore.Record{node}, limit*4)
	if err != nil {
		return nil, nil, nil, err
	}
	neighbors, err := s.resolveNeighbors(node, rels, func(ids []string) (map[string]semstore.Record, error) {
		return semstore.LoadSymbolsByIDsSQLite(storePath, ids)
	}, limit)
	if err != nil {
		return nil, nil, nil, err
	}
	return &node, neighbors, rels, nil
}

func (s *Server) resolveNeighbors(node semstore.Record, rels []semstore.Record, load func([]string) (map[string]semstore.Record, error), limit int) ([]semstore.Record, error) {
	ids := semstore.NeighborCandidateIDs([]semstore.Record{node}, rels)
	if len(ids) == 0 {
		return nil, nil
	}
	byID, err := load(ids)
	if err != nil {
		return nil, err
	}
	return semstore.ResolveContextNeighbors([]semstore.Record{node}, rels, byID, limit), nil
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
