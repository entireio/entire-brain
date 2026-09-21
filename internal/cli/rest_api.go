package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// A REST surface over the same listener as MCP.
//
// MCP is the right protocol for an agent and the wrong one for everything else.
// Putting a brain's knowledge into a dashboard, a CI check, a Slack bot or a
// notebook meant either speaking JSON-RPC by hand or shelling out to the CLI and
// parsing its output. Pieces is local-first and still serves a localhost HTTP
// API with Python and TypeScript clients, which is the proof that "we run
// locally" never explained why we shipped no HTTP surface at all.
//
// This adds plain resource endpoints beside the MCP one, on the same listener,
// under the same rules: absent unless --http is passed, loopback unless told
// otherwise, and the same bearer token. Sharing the listener is deliberate —
// two ports with two auth configurations is two chances to get it wrong.
//
// The endpoints are read-mostly and deliberately small. This is a way to get
// facts out of a brain, not a second API surface that has to keep pace with
// every CLI command.

const restAPIPrefix = "/v1/"

type restError struct {
	Error string `json:"error"`
	Hint  string `json:"hint,omitempty"`
}

func writeRESTError(w http.ResponseWriter, status int, message, hint string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(restError{Error: message, Hint: hint})
}

func writeRESTJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(value)
}

// restLimit reads a bounded limit. An unbounded limit on an endpoint that
// serialises records is a way to turn one request into all of memory, so the
// ceiling is enforced here rather than trusted from the caller.
func restLimit(r *http.Request, fallback, max int) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("limit"))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("limit must be an integer")
	}
	if value < 1 {
		return 0, fmt.Errorf("limit must be at least 1")
	}
	if value > max {
		return 0, fmt.Errorf("limit must be at most %d", max)
	}
	return value, nil
}

const (
	restDefaultLimit = 20
	restMaxLimit     = 200
)

// registerRESTRoutes adds the resource endpoints to an existing mux. The caller
// has already applied authentication, so nothing here re-implements it — one
// auth path for both surfaces is the point of sharing a listener.
func registerRESTRoutes(mux *http.ServeMux, opts Options) {
	mux.HandleFunc(restAPIPrefix, func(w http.ResponseWriter, r *http.Request) {
		resource := strings.Trim(strings.TrimPrefix(r.URL.Path, restAPIPrefix), "/")
		switch {
		case resource == "status" && r.Method == http.MethodGet:
			restStatus(w, r, opts)
		case resource == "facts" && r.Method == http.MethodGet:
			restFacts(w, r, opts)
		case resource == "search" && r.Method == http.MethodGet:
			restSearch(w, r, opts)
		case resource == "":
			// A bare /v1/ lists what exists. An API you can discover from the
			// root is one fewer document to keep in sync.
			writeRESTJSON(w, map[string]any{
				"endpoints": []map[string]string{
					{"method": "GET", "path": "/v1/status", "description": "brain freshness and source counts"},
					{"method": "GET", "path": "/v1/facts", "description": "durable facts; ?limit= and ?branch="},
					{"method": "GET", "path": "/v1/search", "description": "hybrid retrieval; ?q= required, ?limit=, ?source="},
					{"method": "POST", "path": "/", "description": "MCP JSON-RPC (the same tools the CLI exposes)"},
				},
				"auth": "Bearer token, the same one the MCP endpoint uses",
			})
		default:
			writeRESTError(w, http.StatusNotFound,
				fmt.Sprintf("no endpoint %s %s", r.Method, r.URL.Path),
				"GET /v1/ lists the endpoints that exist")
		}
	})
}

func restResolveTarget(r *http.Request, opts Options) (brainDir, branch string, err error) {
	requested := strings.TrimSpace(r.URL.Query().Get("branch"))
	_, brainDir, branch, err = resolveFactsTarget(r.Context(), opts, agentSurfaceTarget(opts, nil), requested)
	return brainDir, branch, err
}

func restStatus(w http.ResponseWriter, r *http.Request, opts Options) {
	brainDir, branch, err := restResolveTarget(r, opts)
	if err != nil {
		writeRESTError(w, http.StatusBadRequest, err.Error(), "")
		return
	}
	facts, err := loadFacts(brainDir, branch)
	if err != nil {
		writeRESTError(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	active, superseded, retracted := 0, 0, 0
	for _, fact := range facts {
		switch fact.Status {
		case "", factStatusActive:
			active++
		case "superseded":
			superseded++
		default:
			retracted++
		}
	}
	writeRESTJSON(w, map[string]any{
		"branch": branch,
		"facts": map[string]int{
			"active": active, "superseded": superseded, "retracted": retracted, "total": len(facts),
		},
	})
}

func restFacts(w http.ResponseWriter, r *http.Request, opts Options) {
	limit, err := restLimit(r, restDefaultLimit, restMaxLimit)
	if err != nil {
		writeRESTError(w, http.StatusBadRequest, err.Error(), fmt.Sprintf("limit is 1..%d", restMaxLimit))
		return
	}
	brainDir, branch, err := restResolveTarget(r, opts)
	if err != nil {
		writeRESTError(w, http.StatusBadRequest, err.Error(), "")
		return
	}
	facts, err := loadFacts(brainDir, branch)
	if err != nil {
		writeRESTError(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	// Retracted and superseded facts are excluded unless asked for. An API that
	// returned them by default would hand a dashboard things the brain no
	// longer believes.
	includeAll := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("all")), "true")
	out := make([]map[string]any, 0, limit)
	for _, fact := range facts {
		if !includeAll && fact.Status != "" && fact.Status != factStatusActive {
			continue
		}
		if len(out) == limit {
			break
		}
		out = append(out, map[string]any{
			"id": fact.ID, "text": fact.Text, "kind": fact.Kind, "paths": fact.Paths,
			"status": fact.Status, "origin": fact.Origin,
			"created_at": fact.CreatedAt, "updated_at": fact.UpdatedAt,
		})
	}
	writeRESTJSON(w, map[string]any{"branch": branch, "count": len(out), "limit": limit, "facts": out})
}

func restSearch(w http.ResponseWriter, r *http.Request, opts Options) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeRESTError(w, http.StatusBadRequest, "q is required", "GET /v1/search?q=retry+policy")
		return
	}
	limit, err := restLimit(r, restDefaultLimit, restMaxLimit)
	if err != nil {
		writeRESTError(w, http.StatusBadRequest, err.Error(), fmt.Sprintf("limit is 1..%d", restMaxLimit))
		return
	}
	brainDir, branch, err := restResolveTarget(r, opts)
	if err != nil {
		writeRESTError(w, http.StatusBadRequest, err.Error(), "")
		return
	}
	repoDir := opts.Env.RepoRoot
	// Through the same parser the CLI and MCP use. Passed raw, an unrecognised
	// value — "facts" for "fact" — turns every include flag off downstream and
	// returns 200 with zero results: an empty search presented as a complete
	// answer, where the same typo on the CLI is an error.
	source, err := parseRetrievalSource(r.URL.Query().Get("source"))
	if err != nil {
		// parseRetrievalSource already names the valid values, so the hint
		// shows the shape of a working call instead of repeating them.
		writeRESTError(w, http.StatusBadRequest, err.Error(), "GET /v1/search?q=retry+policy&source=fact")
		return
	}
	results, err := retrieveUnifiedWithOptions(repoDir, brainDir, branch, query, limit, modeHybrid,
		retrievalOptions{Source: source})
	if err != nil {
		writeRESTError(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	writeRESTJSON(w, map[string]any{"branch": branch, "query": query, "count": len(results), "results": results})
}
