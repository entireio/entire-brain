package entityindex

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// The entity index's record keys and delta documents are git-meta records: once
// the store is shared (P1), every one of them is bytes another member wrote.
// These harnesses assert the encode/decode pairs are total (never panic) and
// honest (a decode either reverses the encode or reports failure — never returns
// a confidently wrong answer).
//
//	go test ./internal/entityindex -run xxx -fuzz FuzzName

// FuzzEntityRecordKeyRoundTrip pins the hex-segment encoding that exists because
// entity keys contain "/" and ":" — the two characters the git-meta key grammar
// forbids. Encoding then decoding must return the original key exactly, for any
// key, including ones carrying separators, unicode, or NUL.
func FuzzEntityRecordKeyRoundTrip(f *testing.F) {
	f.Add("internal/cli/facts.go#function#parseFactsFile")
	f.Add("")
	f.Add("a:b:c#kind#name")
	f.Add("a/b#k#n")
	f.Add("héllo#kind#name")
	f.Add("x\x00y#k#n")
	f.Add(strings.Repeat("a/", 4096) + "#k#n")

	f.Fuzz(func(t *testing.T, entityKey string) {
		for _, tc := range []struct {
			name      string
			encode    func(string) string
			wantAlias bool
		}{
			{"entity", EntityRecordKey, false},
			{"alias", AliasRecordKey, true},
		} {
			recordKey := tc.encode(entityKey)
			got, alias, ok := DecodeEntityKey(recordKey)
			if !ok {
				t.Fatalf("%s: DecodeEntityKey rejected a key EntityRecordKey produced: %q", tc.name, recordKey)
			}
			if alias != tc.wantAlias {
				t.Fatalf("%s: namespace flipped for %q (alias=%v)", tc.name, recordKey, alias)
			}
			if got != entityKey {
				t.Fatalf("%s: round trip changed the key: %q -> %q -> %q", tc.name, entityKey, recordKey, got)
			}
		}
		branch := entityKey
		if got, ok := DecodeWindowBranch(WindowKey(branch)); !ok || got != branch {
			t.Fatalf("window branch round trip failed: %q -> %q -> %q (ok=%v)", branch, WindowKey(branch), got, ok)
		}
	})
}

// FuzzDecodeEntityKey drives the decoder with arbitrary record keys, including
// ones from other namespaces and non-hex segments. It must never panic and must
// never claim a key from another namespace.
func FuzzDecodeEntityKey(f *testing.F) {
	f.Add("brain:entity:6162")
	f.Add("brain:entity-alias:6162")
	f.Add("brain:entity:zz")
	f.Add("brain:entity:")
	f.Add("brain:entities")
	f.Add("brain:entity-alias:6")
	f.Fuzz(func(t *testing.T, recordKey string) {
		key, alias, ok := DecodeEntityKey(recordKey)
		if !ok {
			return
		}
		want := EntityRecordKey(key)
		if alias {
			want = AliasRecordKey(key)
		}
		// A decoder that accepts a key must accept only keys its own encoder can
		// produce; anything else means two distinct record keys decode to the
		// same entity, which silently merges two entities' commit lists.
		if !strings.EqualFold(want, recordKey) {
			t.Fatalf("DecodeEntityKey accepted %q, which its encoder renders as %q", recordKey, want)
		}
	})
}

// FuzzSplitEntityKey asserts the "<path>#<kind>#<name>" split is the exact
// inverse of EntityKey for every triple, including paths that themselves contain
// "#" (legal on every filesystem git supports — the documented reason the split
// cuts from the right).
func FuzzSplitEntityKey(f *testing.F) {
	f.Add("a/b.go", "function", "Foo")
	f.Add("a#b.go", "function", "Foo")
	f.Add("", "", "")
	f.Add("p", "k", "n#n")
	f.Fuzz(func(t *testing.T, path, kind, name string) {
		if strings.Contains(kind, "#") || strings.Contains(name, "#") {
			return // documented precondition: kind and name never contain "#"
		}
		key := EntityKey(path, kind, name)
		gotPath, gotKind, gotName, ok := SplitEntityKey(key)
		if !ok {
			t.Fatalf("SplitEntityKey rejected a key EntityKey produced: %q", key)
		}
		if gotPath != path || gotKind != kind || gotName != name {
			t.Fatalf("split round trip changed the triple: (%q,%q,%q) -> %q -> (%q,%q,%q)",
				path, kind, name, key, gotPath, gotKind, gotName)
		}
	})
}

// FuzzDecodeWindow drives the coverage-claim record. An unreadable cursor must
// report ok=false (costing a re-walk) and must NEVER report a partial window as
// a coverage claim.
func FuzzDecodeWindow(f *testing.F) {
	f.Add(`{"floor":"aaa","tip":"bbb"}`)
	f.Add(`{"floor":"","tip":"bbb"}`)
	f.Add(`{"floor":`)
	f.Add(``)
	f.Add(`   `)
	f.Add(`{"floor":123,"tip":456}`)
	f.Fuzz(func(t *testing.T, raw string) {
		w, ok := DecodeWindow(raw)
		if !ok {
			if w != (IndexWindow{}) {
				t.Fatalf("DecodeWindow(%q) returned ok=false with a non-zero window: %#v", raw, w)
			}
			return
		}
		if w.Empty() {
			t.Fatalf("DecodeWindow(%q) accepted an incomplete window: %#v", raw, w)
		}
		// An accepted window must survive re-encoding, or a stored cursor stops
		// meaning what it did when it was read.
		again, ok2 := DecodeWindow(EncodeWindow(w))
		if !ok2 || again != w {
			t.Fatalf("window round trip changed the claim: %#v -> %q -> %#v (ok=%v)", w, EncodeWindow(w), again, ok2)
		}
	})
}

// FuzzDeltaDocument drives the forward delta document — the git-meta record
// value another member's producer wrote. Decoding must never panic, and an
// accepted document must re-encode to something that decodes identically.
func FuzzDeltaDocument(f *testing.F) {
	f.Add([]byte(`{"schema_version":"1.0","producer":"entire-graph","base":"a","head":"b","computed_at":"t","entities":[{"change":"added","kind":"function","name":"F","path":"a.go","start_line":1}]}`))
	f.Add([]byte(`{"entities":null}`))
	f.Add([]byte(`{"entities":[{"start_line":-9223372036854775808}]}`))
	f.Add([]byte(`{`))
	f.Add([]byte(``))
	f.Fuzz(func(t *testing.T, raw []byte) {
		var d Delta
		if err := json.Unmarshal(raw, &d); err != nil {
			return
		}
		for _, e := range d.Entities {
			_ = e.Key()
			_, _ = e.OldKey()
		}
		encoded, err := json.Marshal(d)
		if err != nil {
			t.Fatalf("a decoded delta failed to re-encode: %v", err)
		}
		var again Delta
		if err := json.Unmarshal(encoded, &again); err != nil {
			t.Fatalf("a re-encoded delta failed to decode: %v", err)
		}
		if !reflect.DeepEqual(normalizeDelta(d), normalizeDelta(again)) {
			t.Fatalf("delta round trip lost content:\nfirst  %#v\nsecond %#v", d, again)
		}
	})
}

// normalizeDelta folds the nil/empty slice distinction that JSON cannot carry.
func normalizeDelta(d Delta) Delta {
	if len(d.Entities) == 0 {
		d.Entities = nil
	}
	return d
}
