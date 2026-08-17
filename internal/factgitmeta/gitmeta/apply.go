// Vendored git-meta exchange engine. Originally copied verbatim from
// github.com/entirehq/git-meta-service/internal/gitmeta @
// feat/git-meta-service-integration (f3ec153) — do not edit here.
//
// PROVENANCE HAS MOVED: git-meta-service was a proof of concept, and the shipping
// implementation now lives in github.com/entirehq/entire-api internal/gitmeta. Re-vendor
// from THERE, not from the PoC repo, whose PR was superseded rather than merged.
//
// The two copies have since diverged in both directions — entire-api grew path-target
// support (PathTargetSubtree, ValidatePathTargetValue) and typed accessors, while this
// copy carries helpers of its own and the older ValidateTargetValue name. That is inert
// today because this engine only ever drives the BARE LOCAL store at
// refs/meta/local/main: no remote, no push or fetch, and no ref that entire-api also
// writes, so no record crosses between the two implementations. It stops being inert the
// moment brain exchanges git-meta records with entire-api over a shared ref, which is
// what makes deduplicating this the right follow-up: both are internal packages, so
// neither can import the other and a real fix means extracting the engine into a shared
// module.

package gitmeta

import (
	"slices"
	"sort"
)

// Mutation is a single metadata edit applied to a State. Exactly one of the
// operation fields is meaningful per Op. The server applies one Mutation onto
// the freshly-fetched full state, then re-serializes — so concurrent writers
// converge via CAS-retry without a general 3-way merge.
type Mutation struct {
	Op     MutationOp
	Target Target
	Key    string
	Value  string // for SetString / ListPush / SetAdd / SetRemove / ListRemove / CompareAndSet
	// Expected is the precondition for OpCompareAndSet: the value the caller
	// believes (target,key) currently holds. Empty means "expect no current
	// string value" (a create). The condition is enforced by the write engine
	// against the freshly-fetched state, not here — Apply(OpCompareAndSet) is
	// an unconditional string set, identical to OpSetString.
	Expected string
	// NowMS is the millisecond timestamp to stamp list appends with. The caller
	// supplies it (the package never reads the clock) for deterministic tests.
	NowMS int64
}

// MutationOp enumerates the supported edits, mirroring the git-meta CLI verbs.
type MutationOp string

const (
	OpSetString  MutationOp = "set"       // replace a string value
	OpRemoveKey  MutationOp = "rm"        // delete a whole key (tombstone)
	OpListPush   MutationOp = "list:push" // append a list entry
	OpListRemove MutationOp = "list:rm"   // remove a single list entry (by value)
	OpSetAdd     MutationOp = "set:add"   // add a set member
	OpSetRemove  MutationOp = "set:rm"    // remove a set member
	// OpCompareAndSet replaces a string value IFF it currently equals
	// Mutation.Expected (empty Expected = must be absent). The precondition is
	// checked by the engine against freshly-fetched state; the state mutation
	// itself is an unconditional string set. This lifts entiredb's ref-level
	// CAS to a VALUE-level compare-and-swap: a lost precondition is a terminal
	// 412 (ErrCASMismatch), distinct from a mechanical ref race (retried).
	OpCompareAndSet MutationOp = "cas"
)

// Apply returns a new State with the mutation applied. The input State is the
// full materialized current state (from the latest metadata tree); the result
// is re-serialized into the next metadata commit.
func (s State) Apply(m Mutation) State {
	out := s.clone()
	switch m.Op {
	case OpSetString, OpCompareAndSet:
		// CompareAndSet's precondition is enforced by the engine before Apply;
		// the state change itself is an ordinary string set.
		out.clearKey(m.Target, m.Key)
		out.Strings = append(out.Strings, StringVal{Target: m.Target, Key: m.Key, Value: m.Value})
		out.dropTombstone(m.Target, m.Key)
	case OpRemoveKey:
		out.clearKey(m.Target, m.Key)
		out.Tombstones = append(out.Tombstones, Tombstone{Target: m.Target, Key: m.Key})
	case OpListPush:
		lv := out.listFor(m.Target, m.Key)
		ts := m.NowMS
		if max := maxTimestamp(lv.Entries); ts <= max {
			ts = max + 1 // keep appended entries ordered after existing ones
		}
		lv.Entries = append(lv.Entries, ListEntry{Value: m.Value, Timestamp: ts})
		out.putList(*lv)
		out.dropTombstone(m.Target, m.Key)
	case OpListRemove:
		lv := out.listFor(m.Target, m.Key)
		kept := lv.Entries[:0]
		for _, e := range lv.Entries {
			if e.Value == m.Value {
				out.Tombstones = append(out.Tombstones, Tombstone{
					Target: m.Target, Key: m.Key,
					Entry: MakeEntryName(e.Timestamp, e.Value), Content: e.Value,
				})
				continue
			}
			kept = append(kept, e)
		}
		lv.Entries = kept
		out.putList(*lv)
	case OpSetAdd:
		sv := out.setFor(m.Target, m.Key)
		if !slices.Contains(sv.Members, m.Value) {
			sv.Members = append(sv.Members, m.Value)
			sort.Strings(sv.Members)
		}
		out.putSet(*sv)
		out.dropTombstone(m.Target, m.Key)
	case OpSetRemove:
		sv := out.setFor(m.Target, m.Key)
		kept := sv.Members[:0]
		for _, mem := range sv.Members {
			if mem == m.Value {
				out.Tombstones = append(out.Tombstones, Tombstone{
					Target: m.Target, Key: m.Key, Member: SetMemberID(mem), Content: mem,
				})
				continue
			}
			kept = append(kept, mem)
		}
		sv.Members = kept
		out.putSet(*sv)
	}
	return out
}

func (s State) clone() State {
	out := State{
		Strings:    append([]StringVal(nil), s.Strings...),
		Tombstones: append([]Tombstone(nil), s.Tombstones...),
	}
	for _, lv := range s.Lists {
		out.Lists = append(out.Lists, ListVal{Target: lv.Target, Key: lv.Key, Entries: append([]ListEntry(nil), lv.Entries...)})
	}
	for _, sv := range s.Sets {
		out.Sets = append(out.Sets, SetVal{Target: sv.Target, Key: sv.Key, Members: append([]string(nil), sv.Members...)})
	}
	return out
}

// clearKey removes any string/list/set value for (target,key) from the State.
func (s *State) clearKey(t Target, key string) {
	s.Strings = dropKey(s.Strings, t, key, func(v StringVal) (Target, string) { return v.Target, v.Key })
	s.Lists = dropKey(s.Lists, t, key, func(v ListVal) (Target, string) { return v.Target, v.Key })
	s.Sets = dropKey(s.Sets, t, key, func(v SetVal) (Target, string) { return v.Target, v.Key })
}

// dropKey returns in with every element for (target,key) removed, using id to
// extract each element's (target,key). Collapses the per-type filter loops.
func dropKey[T any](in []T, t Target, key string, id func(T) (Target, string)) []T {
	out := in[:0]
	for _, v := range in {
		if vt, vk := id(v); vt == t && vk == key {
			continue
		}
		out = append(out, v)
	}
	return out
}

func (s *State) dropTombstone(t Target, key string) {
	kept := s.Tombstones[:0]
	for _, ts := range s.Tombstones {
		if ts.Target == t && ts.Key == key && ts.Entry == "" && ts.Member == "" {
			continue
		}
		kept = append(kept, ts)
	}
	s.Tombstones = kept
}

func (s *State) listFor(t Target, key string) *ListVal {
	for i := range s.Lists {
		if s.Lists[i].Target == t && s.Lists[i].Key == key {
			return &s.Lists[i]
		}
	}
	s.Lists = append(s.Lists, ListVal{Target: t, Key: key})
	return &s.Lists[len(s.Lists)-1]
}

func (s *State) putList(lv ListVal) {
	for i := range s.Lists {
		if s.Lists[i].Target == lv.Target && s.Lists[i].Key == lv.Key {
			s.Lists[i] = lv
			return
		}
	}
	s.Lists = append(s.Lists, lv)
}

func (s *State) setFor(t Target, key string) *SetVal {
	for i := range s.Sets {
		if s.Sets[i].Target == t && s.Sets[i].Key == key {
			return &s.Sets[i]
		}
	}
	s.Sets = append(s.Sets, SetVal{Target: t, Key: key})
	return &s.Sets[len(s.Sets)-1]
}

func (s *State) putSet(sv SetVal) {
	for i := range s.Sets {
		if s.Sets[i].Target == sv.Target && s.Sets[i].Key == sv.Key {
			s.Sets[i] = sv
			return
		}
	}
	s.Sets = append(s.Sets, sv)
}

func maxTimestamp(entries []ListEntry) int64 {
	var max int64
	for _, e := range entries {
		if e.Timestamp > max {
			max = e.Timestamp
		}
	}
	return max
}
