package entityindex

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/ashtom/entire-brain/internal/factgitmeta/gitmeta"
)

// maxAliasHops bounds transitive alias resolution. Renames chain (A -> B -> C)
// and a pathological history could, in principle, produce a cycle; a hop budget
// makes resolution total instead of hanging.
const maxAliasHops = 16

// Snapshot is the materialized, read-side view of the git-meta entity index:
// the reverse map (entity key -> commits, in index order), the rename/move alias
// map, the per-commit forward documents, and the per-branch indexed windows.
// It is derived state — always rebuildable from the git-meta ref alone.
type Snapshot struct {
	commitsByEntity map[string][]string
	aliases         map[string]string
	forward         map[string]string // commit sha -> raw delta document JSON
	windows         map[string]IndexWindow
}

// Load materializes a Snapshot from a git-meta state.
func Load(state gitmeta.State) *Snapshot {
	snap := &Snapshot{
		commitsByEntity: map[string][]string{},
		aliases:         map[string]string{},
		forward:         map[string]string{},
		windows:         map[string]IndexWindow{},
	}
	for _, lv := range state.Lists {
		if lv.Target != projectTarget {
			continue
		}
		entityKey, alias, ok := DecodeEntityKey(lv.Key)
		if !ok || alias {
			continue
		}
		entries := append([]gitmeta.ListEntry(nil), lv.Entries...)
		gitmeta.SortListEntries(entries)
		commits := make([]string, 0, len(entries))
		seen := map[string]struct{}{}
		for _, e := range entries {
			if _, dup := seen[e.Value]; dup {
				continue
			}
			seen[e.Value] = struct{}{}
			commits = append(commits, e.Value)
		}
		snap.commitsByEntity[entityKey] = commits
	}
	for _, sv := range state.Strings {
		switch {
		case sv.Target.Type == gitmeta.TargetCommit && sv.Key == ForwardKey:
			snap.forward[sv.Target.Value] = sv.Value
		case sv.Target == projectTarget:
			if branch, ok := DecodeWindowBranch(sv.Key); ok {
				if window, decoded := DecodeWindow(sv.Value); decoded {
					snap.windows[branch] = window
				}
				continue
			}
			if oldKey, alias, ok := DecodeEntityKey(sv.Key); ok && alias {
				snap.aliases[oldKey] = sv.Value
			}
		}
	}
	return snap
}

// Empty reports whether the index holds nothing at all.
func (s *Snapshot) Empty() bool {
	return s == nil || (len(s.commitsByEntity) == 0 && len(s.forward) == 0)
}

// Keys returns every indexed entity key, sorted.
func (s *Snapshot) Keys() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.commitsByEntity))
	for key := range s.commitsByEntity {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// Window returns a branch's contiguous indexed range, if one is recorded.
func (s *Snapshot) Window(branch string) (IndexWindow, bool) {
	if s == nil {
		return IndexWindow{}, false
	}
	window, ok := s.windows[branch]
	return window, ok
}

// Resolve follows the rename/move alias chain from an entity key to its current
// key. An unknown key resolves to itself.
func (s *Snapshot) Resolve(entityKey string) string {
	if s == nil {
		return entityKey
	}
	return resolveAlias(s.aliases, entityKey)
}

// AliasChain returns every key an entity has been known by from entityKey
// forward to its current key, oldest spelling first. An unaliased key yields a
// one-element chain.
func AliasChain(aliases map[string]string, entityKey string) []string {
	chain := []string{entityKey}
	seen := map[string]struct{}{entityKey: {}}
	current := entityKey
	for hop := 0; hop < maxAliasHops; hop++ {
		next, ok := aliases[current]
		if !ok || next == current {
			break
		}
		if _, cycle := seen[next]; cycle {
			break
		}
		seen[next] = struct{}{}
		chain = append(chain, next)
		current = next
	}
	return chain
}

// Commits returns the commits that changed an entity in INDEX order — the order
// the reverse list was appended in, which is the only order the exchange format
// preserves (a late-arriving older entry is appended, not re-sorted). Callers
// that need chronology sort by the commits' own dates. The WHOLE rename/move
// chain from entityKey to its current key contributes, so asking by any
// spelling the symbol ever had returns its full history — including the commits
// recorded under intermediate names.
func (s *Snapshot) Commits(entityKey string) []string {
	if s == nil {
		return nil
	}
	var out []string
	seen := map[string]struct{}{}
	for _, key := range AliasChain(s.aliases, entityKey) {
		for _, commit := range s.commitsByEntity[key] {
			if _, dup := seen[commit]; dup {
				continue
			}
			seen[commit] = struct{}{}
			out = append(out, commit)
		}
	}
	return out
}

// KeyMatch is one entity key that satisfied a query, already decomposed.
type KeyMatch struct {
	EntityKey string `json:"entity_key"`
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	// AliasOf is the pre-rename key that matched, when the hit came in through
	// the alias map rather than the current key.
	AliasOf string `json:"alias_of,omitempty"`
}

// Match is one entity that satisfied a query, with its commit list.
type Match struct {
	KeyMatch
	Commits []string `json:"commits"`
}

// SearchKeys ranks entity keys against a query (case-insensitive substring over
// the key, the symbol name, and the path). An exact key or exact name hit ranks
// first, then name prefixes, then name substrings, then path/key substrings;
// ties break on the key so results are stable. A blank query returns the first
// `limit` keys in sorted order.
//
// aliases maps a pre-rename key to its current key; a hit that matches only the
// old spelling is reported under the CURRENT key with AliasOf set, so asking by
// a symbol's former name still finds it. It is shared by the git-meta-backed
// Snapshot and by any derived cache built from it so both rank identically.
func SearchKeys(keys []string, aliases map[string]string, query string, limit int) []KeyMatch {
	if limit <= 0 {
		limit = 20
	}
	needle := strings.ToLower(strings.TrimSpace(query))

	type scored struct {
		match KeyMatch
		rank  int
	}
	var hits []scored
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)
	for _, key := range sorted {
		path, kind, name, ok := SplitEntityKey(key)
		if !ok {
			path, kind, name = key, "", key
		}
		rank, matched := rankEntity(needle, key, path, name)
		if !matched {
			continue
		}
		// A key that has since been renamed or moved is reported under its
		// CURRENT spelling with the matched (old) key recorded, so a query never
		// answers with a name that no longer exists in the tree.
		if resolved := resolveAlias(aliases, key); resolved != key {
			rPath, rKind, rName, rOK := SplitEntityKey(resolved)
			if !rOK {
				rPath, rKind, rName = path, kind, name
			}
			hits = append(hits, scored{match: KeyMatch{EntityKey: resolved, Path: rPath, Kind: rKind, Name: rName, AliasOf: key}, rank: rank})
			continue
		}
		hits = append(hits, scored{match: KeyMatch{EntityKey: key, Path: path, Kind: kind, Name: name}, rank: rank})
	}
	oldKeys := make([]string, 0, len(aliases))
	for oldKey := range aliases {
		oldKeys = append(oldKeys, oldKey)
	}
	sort.Strings(oldKeys)
	for _, oldKey := range oldKeys {
		path, kind, name, ok := SplitEntityKey(oldKey)
		if !ok {
			continue
		}
		if _, matched := rankEntity(needle, oldKey, path, name); !matched {
			continue
		}
		resolved := resolveAlias(aliases, oldKey)
		if resolved == oldKey {
			continue
		}
		rPath, rKind, rName, ok := SplitEntityKey(resolved)
		if !ok {
			rPath, rKind, rName = path, kind, name
		}
		hits = append(hits, scored{
			match: KeyMatch{EntityKey: resolved, Path: rPath, Kind: rKind, Name: rName, AliasOf: oldKey},
			rank:  3,
		})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].rank != hits[j].rank {
			return hits[i].rank < hits[j].rank
		}
		return hits[i].match.EntityKey < hits[j].match.EntityKey
	})
	out := make([]KeyMatch, 0, limit)
	seen := map[string]struct{}{}
	for _, hit := range hits {
		dedupeKey := hit.match.EntityKey + "|" + hit.match.AliasOf
		if _, dup := seen[dedupeKey]; dup {
			continue
		}
		seen[dedupeKey] = struct{}{}
		out = append(out, hit.match)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// Search ranks the index's entities against a query and attaches each hit's
// commit list.
func (s *Snapshot) Search(query string, limit int) []Match {
	if s == nil {
		return nil
	}
	keyHits := SearchKeys(s.Keys(), s.aliases, query, limit)
	out := make([]Match, 0, len(keyHits))
	for _, hit := range keyHits {
		lookup := hit.EntityKey
		if hit.AliasOf != "" {
			lookup = hit.AliasOf
		}
		out = append(out, Match{KeyMatch: hit, Commits: s.Commits(lookup)})
	}
	return out
}

// Aliases returns a copy of the rename/move alias map (old key -> new key).
func (s *Snapshot) Aliases() map[string]string {
	if s == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(s.aliases))
	for k, v := range s.aliases {
		out[k] = v
	}
	return out
}

// rankEntity scores one entity against a lowercased query.
func rankEntity(needle, key, path, name string) (int, bool) {
	if needle == "" {
		return 4, true
	}
	lowerKey, lowerPath, lowerName := strings.ToLower(key), strings.ToLower(path), strings.ToLower(name)
	switch {
	case lowerKey == needle, lowerName == needle:
		return 0, true
	case strings.HasPrefix(lowerName, needle):
		return 1, true
	case strings.Contains(lowerName, needle):
		return 2, true
	case strings.Contains(lowerPath, needle), strings.Contains(lowerKey, needle):
		return 3, true
	default:
		return 0, false
	}
}

// resolveAlias follows a rename/move chain with a hop budget, so a cycle can
// never hang resolution.
func resolveAlias(aliases map[string]string, entityKey string) string {
	seen := map[string]struct{}{entityKey: {}}
	current := entityKey
	for hop := 0; hop < maxAliasHops; hop++ {
		next, ok := aliases[current]
		if !ok || next == current {
			return current
		}
		if _, cycle := seen[next]; cycle {
			return current
		}
		seen[next] = struct{}{}
		current = next
	}
	return current
}

// Delta returns a commit's stored delta document.
func (s *Snapshot) Delta(commit string) (Delta, bool) {
	raw, ok := s.RawDelta(commit)
	if !ok {
		return Delta{}, false
	}
	var delta Delta
	if err := json.Unmarshal([]byte(raw), &delta); err != nil {
		return Delta{}, false
	}
	return delta, true
}

// RawDelta returns a commit's stored delta document exactly as written, so an
// inspection surface can print the bytes that are actually in git.
func (s *Snapshot) RawDelta(commit string) (string, bool) {
	if s == nil {
		return "", false
	}
	if raw, ok := s.forward[commit]; ok {
		return raw, true
	}
	// Tolerate an abbreviated sha: the human-facing surfaces accept one.
	if len(commit) >= 4 {
		var found string
		matches := 0
		for sha, raw := range s.forward {
			if strings.HasPrefix(sha, commit) {
				matches++
				found = raw
			}
		}
		if matches == 1 {
			return found, true
		}
	}
	return "", false
}

// IndexedCommits returns every commit carrying a forward document, sorted.
func (s *Snapshot) IndexedCommits() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.forward))
	for sha := range s.forward {
		out = append(out, sha)
	}
	sort.Strings(out)
	return out
}
