package entityindex

import (
	"github.com/ashtom/entire-brain/internal/factgitmeta/gitmeta"
)

// stateKey identifies one (target, key) record slot.
type stateKey struct {
	target gitmeta.Target
	key    string
}

// applyBatch folds a whole backfill's mutations onto a State in one pass.
//
// gitmeta.State.Apply is the reference semantics, but it CLONES the entire
// state per mutation and finds list values by linear scan, so folding it over a
// backfill is O(mutations x state) — quadratic on exactly the workload this
// package exists for (thousands of commits x thousands of entities). applyBatch
// is the same semantics with position maps: one clone, O(1) per mutation.
// TestApplyBatchMatchesReferenceApply pins the equivalence.
//
// Only the two operations this index writes are supported (set and list:push);
// any other op is applied through gitmeta.State.Apply so semantics can never
// silently diverge for an op this function does not model.
func applyBatch(st gitmeta.State, muts []gitmeta.Mutation) gitmeta.State {
	out := gitmeta.State{
		Strings:    append([]gitmeta.StringVal(nil), st.Strings...),
		Tombstones: append([]gitmeta.Tombstone(nil), st.Tombstones...),
	}
	for _, lv := range st.Lists {
		out.Lists = append(out.Lists, gitmeta.ListVal{Target: lv.Target, Key: lv.Key, Entries: append([]gitmeta.ListEntry(nil), lv.Entries...)})
	}
	for _, sv := range st.Sets {
		out.Sets = append(out.Sets, gitmeta.SetVal{Target: sv.Target, Key: sv.Key, Members: append([]string(nil), sv.Members...)})
	}

	stringAt, listAt := positionMaps(out)

	for _, m := range muts {
		id := stateKey{m.Target, m.Key}
		switch m.Op {
		case gitmeta.OpSetString:
			if idx, ok := stringAt[id]; ok {
				out.Strings[idx].Value = m.Value
				// Apply drops the key's tombstone on EVERY set, including an
				// in-place update of an existing value (a tombstone and a live
				// string can coexist on one key). Skipping it here would leave
				// a state the reference implementation never produces.
				out.Tombstones = dropKeyTombstone(out.Tombstones, m.Target, m.Key)
				continue
			}
			// A set replaces ANY prior value for the key, list and set alike,
			// mirroring Apply's clearKey. Those cases do not arise for this
			// index's keyspace (a key is only ever one type), so the rare
			// collision falls back to the reference path rather than being
			// modelled twice.
			if _, hasList := listAt[id]; hasList || hasSet(out, id) {
				out = out.Apply(m)
				stringAt, listAt = positionMaps(out)
				continue
			}
			out.Strings = append(out.Strings, gitmeta.StringVal{Target: m.Target, Key: m.Key, Value: m.Value})
			stringAt[id] = len(out.Strings) - 1
			out.Tombstones = dropKeyTombstone(out.Tombstones, m.Target, m.Key)
		case gitmeta.OpListPush:
			idx, ok := listAt[id]
			if !ok {
				out.Lists = append(out.Lists, gitmeta.ListVal{Target: m.Target, Key: m.Key})
				idx = len(out.Lists) - 1
				listAt[id] = idx
			}
			ts := m.NowMS
			if max := maxTimestamp(out.Lists[idx].Entries); ts <= max {
				ts = max + 1 // keep appended entries ordered after existing ones
			}
			out.Lists[idx].Entries = append(out.Lists[idx].Entries, gitmeta.ListEntry{Value: m.Value, Timestamp: ts})
			out.Tombstones = dropKeyTombstone(out.Tombstones, m.Target, m.Key)
		default:
			// Unmodelled op: fall back to the reference implementation and
			// rebuild the position maps, correctness over speed.
			out = out.Apply(m)
			stringAt, listAt = positionMaps(out)
		}
	}
	return out
}

// positionMaps indexes a State's string and list records by (target, key) so
// applyBatch can find a record in O(1) instead of gitmeta's linear scan.
func positionMaps(st gitmeta.State) (map[stateKey]int, map[stateKey]int) {
	stringAt := make(map[stateKey]int, len(st.Strings))
	for i, sv := range st.Strings {
		stringAt[stateKey{sv.Target, sv.Key}] = i
	}
	listAt := make(map[stateKey]int, len(st.Lists))
	for i, lv := range st.Lists {
		listAt[stateKey{lv.Target, lv.Key}] = i
	}
	return stringAt, listAt
}

func hasSet(st gitmeta.State, id stateKey) bool {
	for _, sv := range st.Sets {
		if sv.Target == id.target && sv.Key == id.key {
			return true
		}
	}
	return false
}

func dropKeyTombstone(in []gitmeta.Tombstone, target gitmeta.Target, key string) []gitmeta.Tombstone {
	kept := in[:0]
	for _, ts := range in {
		if ts.Target == target && ts.Key == key && ts.Entry == "" && ts.Member == "" {
			continue
		}
		kept = append(kept, ts)
	}
	return kept
}

func maxTimestamp(entries []gitmeta.ListEntry) int64 {
	var max int64
	for _, e := range entries {
		if e.Timestamp > max {
			max = e.Timestamp
		}
	}
	return max
}
