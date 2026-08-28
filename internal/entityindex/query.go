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
	// lastTouch is the latest reverse-index entry timestamp (ms) recorded
	// under each entity key. It exists ONLY to break a cycle in the alias
	// graph (see resolveAlias) — the alias records themselves carry no
	// "which edge is newer" information, but the reverse list entries do,
	// because every push is stamped from the commit that produced it.
	lastTouch map[string]int64
}

// Load materializes a Snapshot from a git-meta state.
func Load(state gitmeta.State) *Snapshot {
	snap := &Snapshot{
		commitsByEntity: map[string][]string{},
		aliases:         map[string]string{},
		forward:         map[string]string{},
		windows:         map[string]IndexWindow{},
		lastTouch:       map[string]int64{},
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
		var latest int64
		for _, e := range entries {
			if e.Timestamp > latest {
				latest = e.Timestamp
			}
			if _, dup := seen[e.Value]; dup {
				continue
			}
			seen[e.Value] = struct{}{}
			commits = append(commits, e.Value)
		}
		snap.commitsByEntity[entityKey] = commits
		snap.lastTouch[entityKey] = latest
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
	return resolveAlias(s.aliases, s.lastTouch, entityKey)
}

// Commits returns the commits that changed an entity in INDEX order — the order
// the reverse list was appended in, which is the only order the exchange format
// preserves (a late-arriving older entry is appended, not re-sorted). Callers
// that need chronology sort by the commits' own dates. The WHOLE rename/move
// family contributes — the earlier spellings that resolve INTO entityKey as
// well as the chain forward from it — so asking by any spelling the symbol ever
// had, its current one included, returns its full history.
func (s *Snapshot) Commits(entityKey string) []string {
	if s == nil {
		return nil
	}
	var out []string
	seen := map[string]struct{}{}
	for _, key := range AliasFamily(s.aliases, entityKey) {
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

// AliasFamily returns every key one entity has ever been known by: the earlier
// spellings that resolve INTO entityKey (oldest first), entityKey itself, then
// the chain forward to its current key.
//
// Following the chain only FORWARD is not enough for a history lookup. Aliases
// point old key -> new key, and the reverse list records a commit under the
// entity's POST-change key, so the pre-rename commits live under the OLD key. A
// forward-only walk therefore leaves a query by the entity's CURRENT name — the
// one that actually exists in the tree, and therefore the one a human or an
// agent types — seeing nothing before the rename, and answering with no warning
// as if the symbol had no earlier history.
//
// Traversal is breadth-first over both directions with a visited set, so a
// cycle (A -> B -> A, an entity renamed and then renamed back) terminates and
// nothing is silently truncated: the family is bounded by the alias map itself.
// Predecessors are sorted at every level, so the result is deterministic.
func AliasFamily(aliases map[string]string, entityKey string) []string {
	if len(aliases) == 0 {
		return []string{entityKey}
	}
	predecessors := make(map[string][]string, len(aliases))
	for oldKey, newKey := range aliases {
		predecessors[newKey] = append(predecessors[newKey], oldKey)
	}
	for _, olds := range predecessors {
		sort.Strings(olds)
	}

	seen := map[string]struct{}{entityKey: {}}
	// Walk BACK level by level, then emit oldest level first, so a linear
	// rename chain comes out in the order it was renamed in.
	var levels [][]string
	frontier := []string{entityKey}
	for len(frontier) > 0 {
		var next []string
		for _, key := range frontier {
			for _, older := range predecessors[key] {
				if _, dup := seen[older]; dup {
					continue
				}
				seen[older] = struct{}{}
				next = append(next, older)
			}
		}
		if len(next) == 0 {
			break
		}
		sort.Strings(next)
		levels = append(levels, next)
		frontier = next
	}
	out := make([]string, 0, len(seen))
	for i := len(levels) - 1; i >= 0; i-- {
		out = append(out, levels[i]...)
	}
	out = append(out, entityKey)
	for current := entityKey; ; {
		next, ok := aliases[current]
		if !ok {
			break
		}
		if _, dup := seen[next]; dup {
			break
		}
		seen[next] = struct{}{}
		out = append(out, next)
		current = next
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
//
// lastTouch is the per-key recency signal resolveAlias needs to break a
// rename-back cycle (see its doc comment); a nil or incomplete map only
// degrades that ONE tie-break, never panics — every other key still ranks and
// resolves normally.
func SearchKeys(keys []string, aliases map[string]string, lastTouch map[string]int64, query string, limit int) []KeyMatch {
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
		if resolved := resolveAlias(aliases, lastTouch, key); resolved != key {
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
		resolved := resolveAlias(aliases, lastTouch, oldKey)
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
	keyHits := SearchKeys(s.Keys(), s.aliases, s.lastTouch, query, limit)
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

// LastTouch returns a copy of the per-entity-key latest-reverse-index-entry
// timestamp (ms). It is the recency signal a caller holding a derived copy of
// Aliases (rather than the Snapshot itself) must carry alongside it to get the
// same rename-back-cycle resolution SearchKeys/resolveAlias give a Snapshot —
// see resolveAlias's doc comment.
func (s *Snapshot) LastTouch() map[string]int64 {
	if s == nil {
		return map[string]int64{}
	}
	out := make(map[string]int64, len(s.lastTouch))
	for k, v := range s.lastTouch {
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
//
// A cycle is not merely a pathological safety valve here: a rename-BACK (A
// renamed to B, and later B renamed back to A) genuinely produces one. Each
// rename writes ONE alias record keyed by its OLD spelling
// (AliasRecordKey(oldKey) = newKey), so after A -> B -> A the store holds BOTH
// "A -> B" (from the first rename, now stale) and "B -> A" (from the second,
// current) forever — nothing ever retracts the first edge, because the second
// rename touches a different key. Chasing the chain from "A" therefore visits
// "A -> B -> A" and closes a real 2-cycle; returning the pre-repeat node
// (the old behavior) answers with "B", a spelling nothing in the tree is
// called any more.
//
// The alias map alone cannot tell which edge is stale: both are ordinary,
// individually-valid rename records with no "this one is newer" bit. The
// reverse index does carry that signal, though — every entry is stamped from
// the commit that produced it — so lastTouch (latest reverse-index entry per
// key, see Snapshot.lastTouch) breaks the tie: whichever of the two keys that
// closed the cycle was touched by a LATER commit is the one actually live in
// the tree. A nil or incomplete lastTouch degrades to the old "return the
// pre-repeat node" behavior rather than panicking.
func resolveAlias(aliases map[string]string, lastTouch map[string]int64, entityKey string) string {
	seen := map[string]struct{}{entityKey: {}}
	current := entityKey
	for hop := 0; hop < maxAliasHops; hop++ {
		next, ok := aliases[current]
		if !ok || next == current {
			return current
		}
		if _, cycle := seen[next]; cycle {
			if lastTouch[next] > lastTouch[current] {
				return next
			}
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
