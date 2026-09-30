package issues

import (
	"encoding/json"
	"testing"
	"time"
)

func TestMoveBetweenSelectedProjectsRetainsUnchangedComments(t *testing.T) {
	s := fixture(t)
	const next = "77777777-7777-7777-7777-777777777777"
	if err := s.Configure(Binding{Version: 1, Provider: "linear", Workspace: ws, Projects: []string{project, next}}); err != nil {
		t.Fatal(err)
	}
	i, c := rec("issue", issue, "description"), rec("comment", comment, "unchanged comment")
	importOK(t, s, 1, i, c)
	i.Project = next
	i.UpdatedAt = now.Add(time.Hour)
	i.ObservedAt = i.UpdatedAt
	c.Project = next
	c.ObservedAt = i.UpdatedAt
	// Parent validation must use the batch's accepted current issue, regardless
	// of whether the unchanged comment comes before it in the envelope.
	result := importOK(t, s, 2, c, i)
	got, ok, err := s.Get(c.Key())
	if err != nil || !ok || got.Project != next || len(result.Conflicts) != 0 {
		t.Fatalf("comment lost after move: %+v, found=%v err=%v", result, ok, err)
	}
	c.Project = project
	c.ObservedAt = c.ObservedAt.Add(time.Hour)
	if _, err := s.Import(batch(3, c)); err == nil {
		t.Fatal("comment accepted in a project without its parent")
	}
}

func TestFullObservationEnrichesSameRevisionWithoutOverwritingKnownFields(t *testing.T) {
	s := fixture(t)
	r := rec("issue", issue, "partial… [truncated by provider]")
	r.Fields = map[string]json.RawMessage{"status": json.RawMessage(`"Todo"`)}
	r.Completeness = Completeness{Truncated: []string{"description"}, Missing: []string{"labels"}}
	importOK(t, s, 1, r)
	first, _, _ := s.Get(r.Key())
	r.Text = "partial description now fetched in full"
	r.Fields = map[string]json.RawMessage{"status": json.RawMessage(`"Todo"`), "labels": json.RawMessage(`["bug"]`)}
	r.Completeness = Completeness{}
	r.ObservedAt = now.Add(time.Hour)
	result := importOK(t, s, 2, r)
	got, ok, err := s.Get(r.Key())
	if err != nil || !ok || got.Text != r.Text || len(result.Conflicts) != 0 {
		t.Fatalf("full refresh rejected: %+v got=%q err=%v", result, got.Text, err)
	}
	old, ok, err := s.Get(first.Ref)
	if err != nil || !ok || old.Text != first.Text || len(old.Completeness.Truncated) != 1 {
		t.Fatal("enrichment changed earlier citation", old, err)
	}
	r.Fields["status"] = json.RawMessage(`"Done"`)
	r.ObservedAt = r.ObservedAt.Add(time.Hour)
	if result := importOK(t, s, 3, r); len(result.Conflicts) != 1 {
		t.Fatal("same-revision change to a known field accepted", result)
	}
	// Even an incomplete record cannot overwrite a fully observed sibling field.
	s = fixture(t)
	importOK(t, s, 1, first.Record)
	if result := importOK(t, s, 2, r); len(result.Conflicts) != 1 {
		t.Fatal("enrichment bypassed conflict on known status", result)
	}
}

func TestRunCompletionIncludesBatchesWithoutProgress(t *testing.T) {
	s := fixture(t)
	importOK(t, s, 1, rec("issue", issue, "comment pages still not fetched"))
	// Reopen persisted state before resuming a split page.
	s = Store{Root: s.Root, Lock: s.Lock, Write: s.Write}
	var e Envelope
	_ = json.Unmarshal(batch(2), &e)
	e.Progress = &Progress{Project: project, WindowStart: now.AddDate(0, 0, -90), Issues: Page{Complete: true}, Complete: true}
	b, _ := json.Marshal(e)
	if out, err := s.Import(b); err == nil {
		t.Fatalf("completed without earlier issue comments: %+v", out)
	}
	e.Progress.Comments = map[string]Page{issue: {Complete: true}}
	b, _ = json.Marshal(e)
	if _, err := s.Import(b); err != nil {
		t.Fatal("cannot resume completion", err)
	}
	if result, err := s.Import(b); err != nil || !result.Replay {
		t.Fatal("completion replay", result, err)
	}
	status, _ := s.Status(now)
	if status.PendingRuns != 0 {
		t.Fatal("completed run still pending", status)
	}
	other := rec("issue", "88888888-8888-8888-8888-888888888888", "another split batch")
	importOK(t, s, 3, other)
	status, _ = s.Status(now)
	if status.PendingRuns != 1 {
		t.Fatal("new issue retained false completeness", status)
	}
	if err := s.Disconnect(project, true); err != nil {
		t.Fatal(err)
	}
	state, _ := s.Load()
	if len(state.RunIssues) != 0 {
		t.Fatal("purge retained run membership")
	}
}
