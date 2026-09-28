package issues

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const ws = "11111111-1111-1111-1111-111111111111"
const project = "22222222-2222-2222-2222-222222222222"
const issue = "33333333-3333-3333-3333-333333333333"
const comment = "44444444-4444-4444-4444-444444444444"

var now = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

func fixture(t *testing.T) Store {
	t.Helper()
	var mu sync.Mutex
	s := Store{Root: t.TempDir(), Lock: func(fn func() error) error { mu.Lock(); defer mu.Unlock(); return fn() }, Write: func(p string, b []byte) error { return os.WriteFile(p, b, 0600) }}
	if err := s.Configure(Binding{Version: 1, Provider: "linear", Workspace: ws, Projects: []string{project}}); err != nil {
		t.Fatal(err)
	}
	return s
}
func rec(kind, id, text string) Record {
	r := Record{Kind: kind, Workspace: ws, ID: id, Project: project, Alias: "ENG-123", URL: "https://linear.app/test/issue/ENG-123", Title: "Retry recovery", Text: text, UpdatedAt: now, ObservedAt: now}
	if kind == "comment" {
		r.Issue = issue
	}
	return r
}
func batch(n int, records ...Record) []byte {
	b, _ := json.Marshal(Envelope{Version: 1, Workspace: ws, RunID: "55555555-5555-5555-5555-555555555555", BatchID: fmt.Sprintf("66666666-6666-6666-6666-%012d", n), Records: records})
	return b
}
func importOK(t *testing.T, s Store, n int, rs ...Record) ImportResult {
	t.Helper()
	out, err := s.Import(batch(n, rs...))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestReplayRevisionsAndImmutableCitations(t *testing.T) {
	s := fixture(t)
	r := rec("issue", issue, "original")
	importOK(t, s, 1, r)
	first, _ := s.Records()
	out := importOK(t, s, 1, r)
	if !out.Replay {
		t.Fatal("not idempotent")
	}
	r.Text = "changed"
	out = importOK(t, s, 2, r)
	if len(out.Conflicts) != 1 {
		t.Fatal("equal timestamp conflict not reported")
	}
	r.UpdatedAt = now.Add(time.Hour)
	r.ObservedAt = r.UpdatedAt
	importOK(t, s, 3, r)
	old := rec("issue", issue, "older")
	out = importOK(t, s, 4, old)
	if out.Ignored != 1 {
		t.Fatal("revision regressed")
	}
	got, ok, err := s.Get(first[0].Ref)
	if err != nil || !ok || got.Text != "original" {
		t.Fatal("citation lost", got, err)
	}
	r.ObservedAt = r.ObservedAt.Add(time.Hour)
	importOK(t, s, 5, r)
	st, _ := s.Load()
	if len(st.Current) != 1 || len(st.Snapshots) != 3 {
		t.Fatalf("state: %+v", st)
	}
	if _, err := s.Import(batch(1, r)); err == nil {
		t.Fatal("reused batch accepted")
	}
}

func TestAtomicValidationAndFailedPublication(t *testing.T) {
	s := fixture(t)
	good := rec("issue", issue, "good")
	bad := rec("issue", "../../escape", "bad")
	if _, err := s.Import(batch(1, good, bad)); err == nil {
		t.Fatal("accepted invalid ID")
	}
	rs, _ := s.Records()
	if len(rs) != 0 {
		t.Fatal("partial write")
	}
	tooMany := make([]Record, 101)
	for i := range tooMany {
		tooMany[i] = good
	}
	if _, err := s.Import(batch(2, tooMany...)); err == nil {
		t.Fatal("accepted oversized record count")
	}
	if _, err := s.Import([]byte(strings.Repeat(" ", MaxBatchBytes+1))); err == nil {
		t.Fatal("accepted oversized input")
	}
	s.Write = func(string, []byte) error { return errors.New("disk full") }
	if _, err := s.Import(batch(3, good)); err == nil {
		t.Fatal("expected publication failure")
	}
	rs, _ = s.Records()
	if len(rs) != 0 {
		t.Fatal("failed batch visible")
	}
}

func TestPartialPaginationResumePreservesComments(t *testing.T) {
	s := fixture(t)
	var e Envelope
	_ = json.Unmarshal(batch(1, rec("issue", issue, "description"), rec("comment", comment, "retained")), &e)
	e.Progress = &Progress{Project: project, WindowStart: now.AddDate(0, 0, -90), Issues: Page{Cursor: "next"}, Comments: map[string]Page{issue: {Cursor: "next-comment"}}}
	b, _ := json.Marshal(e)
	if _, err := s.Import(b); err != nil {
		t.Fatal(err)
	}
	e.BatchID = "66666666-6666-6666-6666-000000000002"
	e.Records = nil
	e.Progress.Complete = true
	e.Progress.Issues = Page{Complete: true}
	b, _ = json.Marshal(e)
	if _, err := s.Import(b); err == nil {
		t.Fatal("incomplete comment run accepted")
	}
	e.Progress.Comments[issue] = Page{Complete: true}
	b, _ = json.Marshal(e)
	if _, err := s.Import(b); err != nil {
		t.Fatal(err)
	}
	rs, _ := s.Records()
	if len(rs) != 2 {
		t.Fatal("omitted comment disappeared")
	}
	st, _ := s.Load()
	for _, p := range st.Runs {
		if !p.Complete || !p.Comments[issue].Complete {
			t.Fatal("resume not completed")
		}
	}
}

func TestDisconnectBeforeConfigure(t *testing.T) {
	for _, purge := range []bool{false, true} {
		t.Run(fmt.Sprintf("purge=%t", purge), func(t *testing.T) {
			writes, cleanups := 0, 0
			s := Store{
				Root: t.TempDir(),
				Lock: func(fn func() error) error { return fn() },
				Write: func(p string, b []byte) error {
					writes++
					return os.WriteFile(p, b, 0600)
				},
				CleanDerived: func() error { cleanups++; return nil },
			}
			if err := s.Disconnect(project, purge); err == nil || err.Error() != "issue store is not configured" {
				t.Errorf("expected unconfigured store error, got %v", err)
			}
			if writes != 0 || cleanups != 0 {
				t.Errorf("unconfigured disconnect caused side effects: writes=%d cleanups=%d", writes, cleanups)
			}
			if _, err := os.Stat(filepath.Join(s.Root, "state.json")); !os.IsNotExist(err) {
				t.Errorf("state file should not exist after disconnect: %v", err)
			}
			if _, err := s.Load(); err != nil {
				t.Errorf("load after disconnect: %v", err)
			}
			if err := s.Configure(Binding{Version: 1, Provider: "linear", Workspace: ws, Projects: []string{project}}); err != nil {
				t.Fatalf("configure after disconnect: %v", err)
			}
			st, err := s.Load()
			if err != nil {
				t.Fatal(err)
			}
			if st.Binding.Version != 1 || st.Binding.Workspace != ws || len(st.Binding.Projects) != 1 || st.Binding.Projects[0] != project {
				t.Fatalf("unexpected binding after configure: %+v", st.Binding)
			}
		})
	}
}

func TestRemovalMoveDisconnectAndPurge(t *testing.T) {
	for _, action := range []string{"deleted", "inaccessible", "moved", "disconnect", "project-deleted", "purge"} {
		t.Run(action, func(t *testing.T) {
			s := fixture(t)
			r := rec("issue", issue, "private marker")
			importOK(t, s, 1, rec("project", project, "project"), r, rec("comment", comment, "comment"))
			switch action {
			case "deleted", "inaccessible":
				r.Availability = action
				r.UpdatedAt = now.Add(time.Hour)
				importOK(t, s, 2, r)
			case "moved":
				r.Project = "77777777-7777-7777-7777-777777777777"
				r.UpdatedAt = now.Add(time.Hour)
				importOK(t, s, 2, r)
			case "project-deleted":
				p := rec("project", project, "project")
				p.Availability = "deleted"
				p.UpdatedAt = now.Add(time.Hour)
				importOK(t, s, 2, p)
			default:
				if err := s.Disconnect(project, action == "purge"); err != nil {
					t.Fatal(err)
				}
			}
			rs, _ := s.Records()
			for _, r := range rs {
				if r.Kind != "project" {
					t.Fatal("suppressed record visible", r)
				}
			}
			if action == "purge" {
				b, _ := os.ReadFile(filepath.Join(s.Root, "state.json"))
				if strings.Contains(string(b), "private marker") {
					t.Fatal("purge retained evidence")
				}
			}
		})
	}
}

func TestRepositoryWorkspaceIsolationAndStaleness(t *testing.T) {
	a := fixture(t)
	b := fixture(t)
	r := rec("issue", issue, "first repo")
	r.ObservedAt = now.Add(-25 * time.Hour)
	importOK(t, a, 1, r)
	rs, _ := b.Records()
	if len(rs) != 0 {
		t.Fatal("cross repo leak")
	}
	bad := Binding{Version: 1, Provider: "linear", Workspace: "99999999-9999-9999-9999-999999999999", Projects: []string{project}}
	if err := a.Configure(bad); err == nil {
		t.Fatal("workspace switch accepted")
	}
	r.Workspace = bad.Workspace
	if _, err := a.Import(batch(2, r)); err == nil {
		t.Fatal("workspace mismatch accepted")
	}
	st, _ := a.Status(now)
	if st.Stale != 1 {
		t.Fatal("missing stale count")
	}
}

func TestReceiptsRequirePendingAndDoNotRetryAmbiguousWrites(t *testing.T) {
	s := fixture(t)
	r := rec("issue", issue, "requirements")
	importOK(t, s, 1, r)
	op := Operation{ID: "88888888-8888-8888-8888-888888888888", Target: r.Key(), Action: "comment", PayloadHash: Hash([]byte("test comment")), ObservedAt: now, Outcome: "succeeded"}
	if err := s.Operation(op); err == nil {
		t.Fatal("unobserved intent accepted")
	}
	op.Outcome = "pending"
	if err := s.Operation(op); err != nil {
		t.Fatal(err)
	}
	op.Outcome = "unknown"
	if err := s.Operation(op); err != nil {
		t.Fatal(err)
	}
	op.Outcome = "pending"
	if err := s.Operation(op); err == nil {
		t.Fatal("blind retry accepted")
	}
	op.Outcome = "succeeded"
	op.ObjectID = comment
	op.URL = r.URL + "#comment"
	if err := s.Operation(op); err != nil {
		t.Fatal(err)
	}
	if err := s.Operation(op); err != nil {
		t.Fatal("receipt replay", err)
	}
	op.Outcome = "failed"
	if err := s.Operation(op); err == nil {
		t.Fatal("terminal receipt changed")
	}
	l := Link{Issue: r.Key(), Kind: "commit", Value: "abc123"}
	if err := s.Link(l); err != nil {
		t.Fatal(err)
	}
	_ = s.Link(l)
	st, _ := s.Load()
	if len(st.Links) != 1 || len(st.Operations[op.ID]) != 3 {
		t.Fatal("receipts duplicated")
	}
}

func TestConcurrentImportsDoNotLoseUpdates(t *testing.T) {
	s := fixture(t)
	var wg sync.WaitGroup
	for i := 1; i <= 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			r := rec("issue", fmt.Sprintf("33333333-3333-3333-3333-%012d", n), "parallel")
			if _, err := s.Import(batch(n, r)); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	rs, err := s.Records()
	if err != nil || len(rs) != 20 {
		t.Fatal(len(rs), err)
	}
}

func TestCanonicalReplayAcrossObjectTransports(t *testing.T) {
	s := fixture(t)
	r := rec("issue", issue, "transport")
	r.Fields = map[string]json.RawMessage{"status": json.RawMessage(`{"name":"Todo","id":"status-id"}`)}
	raw := batch(1, r)
	if _, err := s.Import(raw); err != nil {
		t.Fatal(err)
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	other, _ := json.Marshal(decoded)
	out, err := s.Import(other)
	if err != nil || !out.Replay {
		t.Fatal("MCP/CLI replay disagrees", out, err)
	}
}

func TestCorruptSnapshotCannotMasqueradeAsSourceEvidence(t *testing.T) {
	s := fixture(t)
	importOK(t, s, 1, rec("issue", issue, "original"))
	st, _ := s.Load()
	for ref, r := range st.Snapshots {
		r.Text = "tampered"
		st.Snapshots[ref] = r
	}
	b, _ := json.Marshal(st)
	if err := os.WriteFile(filepath.Join(s.Root, "state.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Records(); err == nil {
		t.Fatal("corrupt snapshot accepted")
	}
}

func TestExplicitOlderIssueDoesNotExpandImportWindow(t *testing.T) {
	s := fixture(t)
	r := rec("issue", issue, "explicit older reference")
	r.UpdatedAt = now.AddDate(-1, 0, 0)
	importOK(t, s, 1, r)
	var e Envelope
	_ = json.Unmarshal(batch(2), &e)
	// Explicit one-off fetches and the subsequent window import are distinct runs.
	e.RunID = "77777777-7777-7777-7777-777777777777"
	e.Progress = &Progress{Project: project, WindowStart: now.AddDate(0, 0, -90), Issues: Page{Complete: true}, Comments: map[string]Page{}, Complete: true}
	b, _ := json.Marshal(e)
	if _, err := s.Import(b); err != nil {
		t.Fatal("older separately fetched issue blocks window", err)
	}
}

func TestPartialRefreshReportsCoverageWithoutErasingEvidence(t *testing.T) {
	s := fixture(t)
	importOK(t, s, 1, rec("issue", issue, "cached"))
	var e Envelope
	_ = json.Unmarshal(batch(2), &e)
	e.Progress = &Progress{Project: project, WindowStart: now.AddDate(0, 0, -90), Issues: Page{}, Limitations: []string{"refresh failed: remote unavailable"}}
	b, _ := json.Marshal(e)
	if _, err := s.Import(b); err != nil {
		t.Fatal(err)
	}
	rs, _ := s.Records()
	if len(rs) != 1 || len(rs[0].Coverage) != 2 {
		t.Fatal("refresh failure lost cached evidence or warning", rs)
	}
}

func TestPurgeDoesNotForgetOtherProjectsBatchReceipts(t *testing.T) {
	s := fixture(t)
	otherProject := "99999999-9999-9999-9999-999999999999"
	if err := s.Configure(Binding{Version: 1, Provider: "linear", Workspace: ws, Projects: []string{project, otherProject}}); err != nil {
		t.Fatal(err)
	}
	importOK(t, s, 1, rec("issue", issue, "first project"))
	other := rec("issue", "77777777-7777-7777-7777-777777777777", "second project")
	other.Project = otherProject
	importOK(t, s, 2, other)
	if err := s.Disconnect(project, true); err != nil {
		t.Fatal(err)
	}
	out := importOK(t, s, 2, other)
	if !out.Replay {
		t.Fatal("purge erased another project's retry receipt")
	}
}

func TestAccessibilityObservationDoesNotInventSourceRevision(t *testing.T) {
	s := fixture(t)
	r := rec("issue", issue, "cached source revision")
	importOK(t, s, 1, r)
	r.Availability = "inaccessible"
	r.ObservedAt = now.Add(time.Hour)
	out := importOK(t, s, 2, r)
	if len(out.Conflicts) != 0 {
		t.Fatal("accessibility is not a source text conflict")
	}
	rs, _ := s.Records()
	if len(rs) != 0 {
		t.Fatal("inaccessible evidence remains visible")
	}
	r.Availability = "available"
	r.ObservedAt = now.Add(2 * time.Hour)
	importOK(t, s, 3, r)
	rs, _ = s.Records()
	if len(rs) != 1 {
		t.Fatal("confirmed access restoration failed")
	}
}

func TestFailedOperationRemainsTerminal(t *testing.T) {
	s := fixture(t)
	r := rec("issue", issue, "target")
	importOK(t, s, 1, r)
	op := Operation{ID: "88888888-8888-8888-8888-888888888888", Target: r.Key(), Action: "status", PayloadHash: Hash([]byte("update status")), ObservedAt: now, Outcome: "pending"}
	if err := s.Operation(op); err != nil {
		t.Fatal(err)
	}
	op.Outcome = "failed"
	op.Detail = "remote rejected invalid status"
	if err := s.Operation(op); err != nil {
		t.Fatal(err)
	}
	if err := s.Operation(op); err != nil {
		t.Fatal("receipt retry failed", err)
	}
	op.Outcome = "pending"
	if err := s.Operation(op); err == nil {
		t.Fatal("terminal failure was blindly retried")
	}
}

func TestOperationPreservesBeforeSnapshot(t *testing.T) {
	for _, outcome := range []string{"unknown", "failed", "succeeded"} {
		t.Run(outcome, func(t *testing.T) {
			s := fixture(t)
			r := rec("issue", issue, "requirements")
			importOK(t, s, 1, r)
			op := Operation{ID: "88888888-8888-8888-8888-888888888888", Target: r.Key(), Action: "status", PayloadHash: Hash([]byte("update")), ObservedAt: now, Outcome: "pending", Before: map[string]json.RawMessage{"status": json.RawMessage(`{"name":"open","id":"old"}`)}}
			if err := s.Operation(op); err != nil {
				t.Fatal(err)
			}
			op.Outcome, op.ObjectID, op.URL = outcome, issue, r.URL
			for _, changed := range []map[string]json.RawMessage{
				{"status": json.RawMessage(`{"name":"closed","id":"new"}`)},
				nil,
				{"status": json.RawMessage(`{"name":"open","id":"old"}`), "priority": json.RawMessage(`1`)},
			} {
				op.Before = changed
				if err := s.Operation(op); err == nil {
					t.Fatal("operation replaced its original before snapshot")
				}
				st, err := s.Load()
				if err != nil || len(st.Operations[op.ID]) != 1 {
					t.Fatalf("rejected operation altered history: %+v, %v", st.Operations, err)
				}
			}
			// Formatting and key order may differ between CLI and MCP callers.
			op.Before = map[string]json.RawMessage{"status": json.RawMessage(`{ "id": "old", "name": "open" }`)}
			if err := s.Operation(op); err != nil {
				t.Fatalf("unchanged before snapshot rejected: %v", err)
			}
		})
	}
}
