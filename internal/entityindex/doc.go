// Package entityindex is the keystone entity -> checkpoint index: a persisted,
// git-native answer to "which checkpoints/sessions changed entity X" that never
// re-parses history.
//
// Every indexed commit gets ONE forward record — its semantic delta document,
// computed once from `entire graph diff --base <parent> --head <commit>` — plus
// one reverse list append per changed entity. Both live as git-meta records in
// the brain's local git-meta store (refs/meta/local/main), so the index is
// durable, inspectable with plain git, and rebuildable from nothing but the
// repository.
//
// The wire contract (frozen; shared with the Entire CLI's producer side):
//
//	forward   target=commit <sha>   key "brain:entities"                 op set
//	          value = the compact JSON delta document below
//	reverse   target=project        key "brain:entity:<entitykey>"       op list:push
//	          value = <commit sha>
//	alias     target=project        key "brain:entity-alias:<oldkey>"    op set
//	          value = <new entitykey>          (renames and moves only)
//	window    target=project        key "brain:entities-window:<branch>" op set
//	          value = {"floor":"<sha>","tip":"<sha>"}
//
// The window is the index's coverage claim, and it is a RANGE, not a mark:
// every first-parent commit from floor to tip inclusive carries a forward
// document. A single "newest indexed commit" mark cannot say that — it silently
// implies coverage of everything below it, so any pass that skipped commits
// (a --checkpoints-only pass, a partial backfill) left them permanently
// unindexed behind a mark that had already moved past them. Two cursors make
// the claim checkable: ticks push the tip forward over commits that landed
// since, backfill chunks pull the floor backward, and a filtered pass moves
// neither.
//
// <entitykey> is "<path>#<kind>#<name>" using the POST-change path and name.
// Because the git-meta exchange format splits keys on ":" and forbids "/" in a
// key segment (gitmeta.ValidateKey — a slash-bearing segment is REJECTED by
// Serialize, not silently accepted), an entity key can never be spliced into a
// key literally: paths always contain "/". It is therefore hex-encoded into a
// single segment, exactly as factgitmeta already hex-encodes repo keys and
// branch names for the same reason. EntityRecordKey/AliasRecordKey are the one
// place that encoding lives; DecodeEntityKey reverses it.
package entityindex

import (
	"encoding/hex"
	"encoding/json"
	"strings"
)

const (
	// SchemaVersion is the frozen delta-document version.
	SchemaVersion = "1.0"
	// Producer names the tool that computed a delta document.
	Producer = "entire-graph"

	// ForwardKey is the git-meta key holding a commit's delta document.
	ForwardKey = "brain:entities"
	// reverseKeyPrefix namespaces the entity -> commits reverse list.
	reverseKeyPrefix = "brain:entity:"
	// aliasKeyPrefix namespaces the old-key -> new-key rename/move alias.
	aliasKeyPrefix = "brain:entity-alias:"
	// windowKeyPrefix namespaces the per-branch INDEXED WINDOW record: the
	// floor and tip of one contiguous, fully-indexed first-parent range.
	windowKeyPrefix = "brain:entities-window:"

	// EmptyTreeSHA is git's canonical empty tree. It is the diff base for a root
	// commit, so the very first commit's entities are indexed as `added` rather
	// than skipped.
	EmptyTreeSHA = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
)

// Change values in a delta document. The graph provider's richer vocabulary
// (body_changed / signature_changed) folds into "modified"; the contract keeps
// the five stable kinds a consumer can reason about.
const (
	ChangeAdded    = "added"
	ChangeRemoved  = "removed"
	ChangeModified = "modified"
	ChangeRenamed  = "renamed"
	ChangeMoved    = "moved"
)

// Delta is one commit's entity-change document (schema_version "1.0"). Field
// order matches the frozen contract. Truncated/EntityCount are ADDITIVE
// (schema_version stays "1.0"): a reader that does not know them still decodes
// every other field, and their zero values (false / 0) are exactly what an
// untruncated document already means, so they cost nothing on the common path.
type Delta struct {
	SchemaVersion   string        `json:"schema_version"`
	Producer        string        `json:"producer"`
	ProducerVersion string        `json:"producer_version,omitempty"`
	Base            string        `json:"base"`
	Head            string        `json:"head"`
	ComputedAt      string        `json:"computed_at"`
	Entities        []EntityDelta `json:"entities"`
	// Truncated reports that Entities was capped at maxDeltaEntities and does
	// NOT list every entity this commit actually changed. Omitted (false) on
	// every document written before this field existed, which is the correct
	// reading: nothing was ever truncated silently before the cap existed
	// either. See EntityCount for the true total.
	Truncated bool `json:"truncated,omitempty"`
	// EntityCount is the TRUE number of entities this commit changed, recorded
	// only when Truncated is true (0 otherwise, and absent from JSON). It lets
	// a reader tell "this commit touched exactly maxDeltaEntities entities"
	// apart from "this commit touched many more than we stored", and lets a
	// later pass decide whether raising the cap would recover more of it.
	EntityCount int `json:"entity_count,omitempty"`
}

// EntityDelta is one changed entity inside a Delta. Path/Name are the
// POST-change spelling; OldPath/OldName carry the pre-change spelling for
// renames and moves.
type EntityDelta struct {
	Change       string `json:"change"`
	Kind         string `json:"kind"`
	Name         string `json:"name"`
	OldName      string `json:"old_name,omitempty"`
	Path         string `json:"path"`
	OldPath      string `json:"old_path,omitempty"`
	Signature    string `json:"signature,omitempty"`
	OldSignature string `json:"old_signature,omitempty"`
	StartLine    int    `json:"start_line"`
	// Fingerprint is a contract field NO CURRENT PRODUCER POPULATES. `entire
	// graph diff` emits no per-change fingerprint (see sem.EntityChange), so
	// nothing decodes into it and it is always omitted from stored documents.
	// It stays in the frozen shape so a future producer can start emitting one
	// without a schema bump; do not add a decode for it until one does.
	Fingerprint string `json:"fingerprint,omitempty"`
}

// Key is the entity's index key: "<path>#<kind>#<name>" (post-change).
func (e EntityDelta) Key() string { return EntityKey(e.Path, e.Kind, e.Name) }

// OldKey is the entity's PRE-change index key, and ok=false when the change did
// not move or rename the entity (so no alias is needed).
func (e EntityDelta) OldKey() (string, bool) {
	oldPath, oldName := e.OldPath, e.OldName
	if oldPath == "" {
		oldPath = e.Path
	}
	if oldName == "" {
		oldName = e.Name
	}
	key := EntityKey(oldPath, e.Kind, oldName)
	if key == e.Key() {
		return "", false
	}
	return key, true
}

// EntityKey builds the frozen "<path>#<kind>#<name>" index key.
func EntityKey(path, kind, name string) string {
	return path + "#" + kind + "#" + name
}

// SplitEntityKey decomposes an index key back into (path, kind, name). It cuts
// from the RIGHT on the last two separators so a path containing "#" (legal on
// every filesystem git supports) still round-trips: kind and name never contain
// "#", the path may.
func SplitEntityKey(key string) (path, kind, name string, ok bool) {
	sep := strings.LastIndex(key, "#")
	if sep < 0 {
		return "", "", "", false
	}
	name = key[sep+1:]
	rest := key[:sep]
	sep = strings.LastIndex(rest, "#")
	if sep < 0 {
		return "", "", "", false
	}
	return rest[:sep], rest[sep+1:], name, true
}

// EntityRecordKey is the git-meta project key holding an entity's reverse
// commit list.
func EntityRecordKey(entityKey string) string {
	return reverseKeyPrefix + hexSegment(entityKey)
}

// AliasRecordKey is the git-meta project key holding a renamed/moved entity's
// old-key -> new-key alias.
func AliasRecordKey(oldEntityKey string) string {
	return aliasKeyPrefix + hexSegment(oldEntityKey)
}

// IndexWindow is a branch's contiguous, fully-indexed first-parent range: every
// commit from Floor to Tip inclusive carries a forward delta document. Both
// ends are commit shas; the zero value means "nothing indexed on this branch".
type IndexWindow struct {
	Floor string `json:"floor"`
	Tip   string `json:"tip"`
}

// Empty reports whether the window makes no coverage claim at all.
func (w IndexWindow) Empty() bool { return w.Floor == "" || w.Tip == "" }

// EncodeWindow renders a window for storage as one git-meta string record.
func EncodeWindow(w IndexWindow) string {
	data, err := json.Marshal(w)
	if err != nil {
		// Two plain sha fields cannot fail to marshal; an empty value reads
		// back as "no window", which is the safe direction (re-index).
		return ""
	}
	return string(data)
}

// DecodeWindow parses a stored window record, reporting ok=false for anything
// it cannot read as a complete range (so an unreadable cursor costs a re-walk,
// never a false coverage claim).
func DecodeWindow(raw string) (IndexWindow, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return IndexWindow{}, false
	}
	var w IndexWindow
	if err := json.Unmarshal([]byte(trimmed), &w); err != nil {
		return IndexWindow{}, false
	}
	if w.Empty() {
		return IndexWindow{}, false
	}
	return w, true
}

// WindowKey is the git-meta project key holding a branch's indexed window.
func WindowKey(branch string) string {
	return windowKeyPrefix + hexSegment(branch)
}

// DecodeEntityKey reverses EntityRecordKey/AliasRecordKey for a stored git-meta
// key, returning ok=false when the key belongs to another namespace or its
// segment is not valid hex.
func DecodeEntityKey(recordKey string) (entityKey string, alias bool, ok bool) {
	switch {
	case strings.HasPrefix(recordKey, aliasKeyPrefix):
		decoded, err := hex.DecodeString(strings.TrimPrefix(recordKey, aliasKeyPrefix))
		if err != nil {
			return "", false, false
		}
		return string(decoded), true, true
	case strings.HasPrefix(recordKey, reverseKeyPrefix):
		decoded, err := hex.DecodeString(strings.TrimPrefix(recordKey, reverseKeyPrefix))
		if err != nil {
			return "", false, false
		}
		return string(decoded), false, true
	default:
		return "", false, false
	}
}

// DecodeWindowBranch reverses WindowKey.
func DecodeWindowBranch(recordKey string) (string, bool) {
	if !strings.HasPrefix(recordKey, windowKeyPrefix) {
		return "", false
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(recordKey, windowKeyPrefix))
	if err != nil {
		return "", false
	}
	return string(decoded), true
}

func hexSegment(s string) string { return hex.EncodeToString([]byte(s)) }
