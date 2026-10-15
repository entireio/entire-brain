package issues

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestVisibilityRejectsMalformedCommentParents(t *testing.T) {
	for _, kind := range []string{"self", "cycle", "project", "wrong-issue", "wrong-workspace"} {
		t.Run(kind, func(t *testing.T) {
			s := fixture(t)
			st, err := s.Load()
			if err != nil {
				t.Fatal(err)
			}
			child := rec("comment", comment, "child")
			parent := rec("issue", issue, "parent")
			switch kind {
			case "self":
				child.Issue = child.ID
				parent = child
			case "cycle":
				parent.Kind = "comment"
				parent.Issue = child.ID
			case "project":
				parent = rec("project", project, "project")
			case "wrong-issue":
				parent.ID = "77777777-7777-7777-7777-777777777777"
			case "wrong-workspace":
				parent.Workspace = "88888888-8888-8888-8888-888888888888"
			}
			for _, r := range []Record{child, parent} {
				data, _ := json.Marshal(r)
				ref := r.Key() + "@" + Hash(data)
				st.Snapshots[ref] = r
				st.Current[r.Key()] = ref
			}
			// Deliberately bypass import/Load integrity checks and alias the
			// issue namespace to an invalid parent, including a two-node cycle.
			st.Current["issue:"+ws+":issue:"+child.Issue] = st.Current[parent.Key()]
			if kind == "cycle" {
				st.Current["issue:"+ws+":issue:"+parent.Issue] = st.Current[child.Key()]
			}
			if visible(&st, child) {
				t.Fatal("malformed parent made comment visible")
			}
			if kind == "cycle" && visible(&st, parent) {
				t.Fatal("cycle parent visible")
			}

			data, _ := json.Marshal(st)
			if err := os.WriteFile(filepath.Join(s.Root, "state.json"), data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Load(); err == nil {
				t.Fatal("Load accepted malformed parent pointer")
			}
			if _, err := s.Records(); err == nil {
				t.Fatal("Records accepted corrupt state")
			}
			if _, _, err := s.Get(child.Key()); err == nil {
				t.Fatal("Get accepted corrupt state")
			}
			if _, err := s.Status(now); err == nil {
				t.Fatal("Status accepted corrupt state")
			}
		})
	}
}

func TestImportRejectsCommentParentCyclesAtomically(t *testing.T) {
	for _, self := range []bool{false, true} {
		s := fixture(t)
		a := rec("comment", comment, "first")
		b := rec("comment", issue, "second")
		b.Issue = a.ID
		if self {
			a.Issue = a.ID
		}
		if _, err := s.Import(batch(1, a, b)); err == nil {
			t.Fatal("comment parent cycle imported")
		}
		st, err := s.Load()
		if err != nil || len(st.Current) != 0 || len(st.Snapshots) != 0 {
			t.Fatal("rejected batch partially persisted", err)
		}
	}
}

func TestCommentParentUsesTypedIdentity(t *testing.T) {
	s := fixture(t)
	// UUID equality across object kinds is not a cycle. The :issue: namespace
	// resolves the real issue, not the comment that happens to have the same UUID.
	p := rec("issue", comment, "parent issue")
	c := rec("comment", comment, "child comment")
	c.Issue = p.ID
	importOK(t, s, 1, p, c)
	if _, ok, err := s.Get(c.Key()); err != nil || !ok {
		t.Fatal("valid typed parent rejected", err)
	}
}
