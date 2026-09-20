package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestAbstractHealthRejectsInvalidCitations(t *testing.T) {
	b, _ := writeSessionNavigationFixture(t)
	v := sessionViewForTest(t, b)
	d := sessionViewDigest(v)
	a := validAbstractForView(v, d)
	a.Overview.EvidenceIDs = []string{"conversation:invented"}
	data, e := json.Marshal(a)
	if e != nil {
		t.Fatal(e)
	}
	writeAbstractBytesForTest(t, b, d, data)
	h := memoryAbstractHealth(b)
	if h["scan_degraded"] != false {
		t.Fatalf("validation must not degrade successful scan: %v", h)
	}
	issues := h["issues"].([]memoryHealthIssue)
	if len(issues) != 1 || issues[0].Path != abstractRel(d) {
		t.Fatalf("wrong issue path: %+v", issues)
	}
	if h["corrupt"] != 1 {
		t.Fatalf("precondition: expected corrupt=%v", h)
	}
	if h["state"] == "current" || h["current_artifacts"] != 0 || h["issue_count"] != 1 {
		t.Fatalf("invalid artifact is simultaneously reported current: %v", h)
	}
}

func TestAbstractHealthSeparatesSchemaFromUsability(t *testing.T) {
	for _, kind := range []string{"valid", "stale", "unverifiable"} {
		t.Run(kind, func(t *testing.T) {
			b, _ := writeSessionNavigationFixture(t)
			v := sessionViewForTest(t, b)
			d := sessionViewDigest(v)
			a := validAbstractForView(v, d)
			if kind == "stale" {
				a.SessionRef = "conversation-session:missing"
			}
			data, err := json.Marshal(a)
			if err != nil {
				t.Fatal(err)
			}
			writeAbstractBytesForTest(t, b, d, data)
			if kind == "unverifiable" {
				if err := os.WriteFile(filepath.Join(b, sessionTombstonesPath), []byte("{broken"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			h := memoryAbstractHealth(b)
			if h["scan_degraded"] != false {
				t.Fatalf("validation must not degrade successful scan: %v", h)
			}
			if kind == "unverifiable" {
				issues := h["issues"].([]memoryHealthIssue)
				if len(issues) != 1 || issues[0].Kind != "tombstones" || issues[0].Path != sessionTombstonesPath {
					t.Fatalf("wrong prerequisite issue: %+v", issues)
				}
			}
			wantState, wantCurrent := "current", 1
			if kind != "valid" {
				wantState = kind
				wantCurrent = 0
			}
			if h["schema_state"] != "current" || h["state"] != wantState || h["current_artifacts"] != wantCurrent {
				t.Fatalf("health=%v", h)
			}
			if kind != "valid" && h["issue_count"] != 1 {
				t.Fatalf("missing issue: %v", h)
			}
		})
	}
}
