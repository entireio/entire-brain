package cli

import (
	"context"
	"encoding/json"
	"os"
	"strings"
)

// queryExpansionPrompt instructs the agent to rewrite a task into retrieval
// terms. The measured gap is that a high-level request ("add a path subcommand")
// shares little vocabulary with the specific facts the work produced; expansion
// bridges it by naming the identifiers, components, and synonyms likely to
// appear in those facts. Like the other agent prompts it must not begin with a
// dash.
func queryExpansionPrompt() string {
	return `You expand a software task into search terms for retrieving durable engineering
notes about that work. The query on stdin is a developer's request; the notes use
specific technical vocabulary the request may not.

Output a single line: 6 to 15 space-separated terms — likely identifiers, package
and component names, synonyms, and key technical nouns that notes about this task
would contain. No prose, no punctuation beyond what a code identifier needs.`
}

// parseExpansion takes the agent's expansion output and returns a bounded,
// single-line term string to append to the query.
func parseExpansion(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return truncateString(strings.Join(strings.Fields(line), " "), 240)
	}
	return ""
}

// expandQuery returns extra retrieval terms for a query, consulting the cache
// first so repeated evals are deterministic and cheap.
func expandQuery(ctx context.Context, run distillAgentRunner, args []string, repoDir, query string, cache *expansionCache) (string, error) {
	if v, ok := cache.get(query); ok {
		return v, nil
	}
	out, err := run(ctx, repoDir, args, []byte(query), defaultDistillTimeout)
	if err != nil {
		return "", err
	}
	exp := parseExpansion(out)
	cache.set(query, exp)
	return exp, nil
}

// expandedQuery is query plus its expansion terms; an empty expansion leaves the
// query unchanged.
func expandedQuery(query, expansion string) string {
	if strings.TrimSpace(expansion) == "" {
		return query
	}
	return query + " " + expansion
}

// expansionCache persists query -> expansion terms so an expanded eval re-runs
// without re-calling the agent.
type expansionCache struct {
	path  string
	terms map[string]string
	dirty bool
}

func loadExpansionCache(path string) *expansionCache {
	c := &expansionCache{path: path, terms: map[string]string{}}
	if strings.TrimSpace(path) == "" {
		return c
	}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &c.terms)
	}
	return c
}

func (c *expansionCache) get(query string) (string, bool) {
	if c == nil {
		return "", false
	}
	v, ok := c.terms[query]
	return v, ok
}

func (c *expansionCache) set(query, expansion string) {
	if c == nil {
		return
	}
	c.terms[query] = expansion
	c.dirty = true
}

func (c *expansionCache) save() error {
	if c == nil || !c.dirty || strings.TrimSpace(c.path) == "" {
		return nil
	}
	data, err := json.MarshalIndent(c.terms, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(c.path, append(data, '\n'), 0o600)
}
