// Package issues stores external source evidence. It never executes remote
// requests, interprets source text as instructions, or creates durable facts.
package issues

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const MaxBatchBytes = 2 << 20
const MaxRecords = 100

var uuid = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type Binding struct {
	Version       int      `json:"version"`
	Provider      string   `json:"provider"`
	Workspace     string   `json:"workspace"`
	WorkspaceName string   `json:"workspace_name,omitempty"`
	Projects      []string `json:"projects"`
}
type Completeness struct {
	Missing     []string `json:"missing,omitempty"`
	Truncated   []string `json:"truncated,omitempty"`
	Limitations []string `json:"limitations,omitempty"`
}
type Record struct {
	Kind         string                     `json:"kind"`
	Workspace    string                     `json:"workspace"`
	ID           string                     `json:"id"`
	Project      string                     `json:"project"`
	Issue        string                     `json:"issue,omitempty"`
	Alias        string                     `json:"alias,omitempty"`
	URL          string                     `json:"url"`
	Title        string                     `json:"title,omitempty"`
	Text         string                     `json:"text"`
	Fields       map[string]json.RawMessage `json:"fields,omitempty"`
	UpdatedAt    time.Time                  `json:"updated_at"`
	ObservedAt   time.Time                  `json:"observed_at"`
	Completeness Completeness               `json:"completeness"`
	// Only an explicit remote observation may set deleted or inaccessible.
	Availability string `json:"availability,omitempty"`
}

func (r Record) Key() string { return "issue:" + r.Workspace + ":" + r.Kind + ":" + r.ID }
func (r Record) Group() string {
	if r.Kind == "comment" {
		return r.Workspace + ":" + r.Issue
	}
	return r.Workspace + ":" + r.ID
}
func Hash(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }

// Normalize nested RawMessage objects too, so CLI JSON and MCP's decoded
// object transport agree despite object-key ordering or insignificant spaces.
func canonicalJSON(value any) []byte {
	b, _ := json.Marshal(value)
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var normalized any
	_ = d.Decode(&normalized)
	out, _ := json.Marshal(normalized)
	return out
}
func (r Record) ContentHash() string {
	r.ObservedAt = time.Time{}
	// Accessibility is a local observation of this source revision. Remote
	// deletion/access denial often does not provide a new source update time.
	r.Availability = ""
	return Hash(canonicalJSON(r))
}

type Snapshot struct {
	Record
	Hash     string   `json:"content_hash"`
	Ref      string   `json:"snapshot"`
	Coverage []string `json:"coverage,omitempty"`
}
type Page struct {
	Complete bool   `json:"complete"`
	Cursor   string `json:"cursor,omitempty"`
}
type Progress struct {
	Project     string          `json:"project"`
	WindowStart time.Time       `json:"window_start"`
	Issues      Page            `json:"issues"`
	Comments    map[string]Page `json:"comments"`
	IssueIDs    []string        `json:"issue_ids,omitempty"`
	Complete    bool            `json:"complete"`
	Limitations []string        `json:"limitations,omitempty"`
}
type Envelope struct {
	Version   int       `json:"version"`
	Workspace string    `json:"workspace"`
	RunID     string    `json:"run_id"`
	BatchID   string    `json:"batch_id"`
	Records   []Record  `json:"records"`
	Progress  *Progress `json:"progress,omitempty"`
}
type Link struct {
	Issue string `json:"issue"`
	Kind  string `json:"kind"`
	Value string `json:"value"`
}
type Operation struct {
	ID          string                     `json:"id"`
	Target      string                     `json:"target"`
	Action      string                     `json:"action"`
	PayloadHash string                     `json:"payload_hash"`
	Before      map[string]json.RawMessage `json:"before,omitempty"`
	Outcome     string                     `json:"outcome"`
	ObjectID    string                     `json:"object_id,omitempty"`
	URL         string                     `json:"url,omitempty"`
	RemoteAt    time.Time                  `json:"remote_at,omitempty"`
	ObservedAt  time.Time                  `json:"observed_at"`
	Detail      string                     `json:"detail,omitempty"`
}
type State struct {
	Binding       Binding                `json:"binding"`
	Current       map[string]string      `json:"current"`
	Snapshots     map[string]Record      `json:"snapshots"`
	Batches       map[string]string      `json:"batches"`
	BatchProjects map[string][]string    `json:"batch_projects,omitempty"`
	Runs          map[string]Progress    `json:"runs"`
	RunIssues     map[string][]string    `json:"run_issues,omitempty"`
	Links         []Link                 `json:"links,omitempty"`
	Operations    map[string][]Operation `json:"operations"`
}

// The adapter supplies Brain's cross-process lock and platform-specific atomic
// writer. All durable state is published by one replacement while holding it.
type Store struct {
	Root         string
	Lock         func(func() error) error
	Write        func(string, []byte) error
	Read         func(string) ([]byte, error)
	CleanDerived func() error
}

func (s Store) Load() (State, error) {
	st := State{Current: map[string]string{}, Snapshots: map[string]Record{}, Batches: map[string]string{}, Runs: map[string]Progress{}, Operations: map[string][]Operation{}}
	read := s.Read
	if read == nil {
		read = os.ReadFile
	}
	b, err := read(filepath.Join(s.Root, "state.json"))
	if os.IsNotExist(err) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	err = json.Unmarshal(b, &st)
	if st.BatchProjects == nil {
		st.BatchProjects = map[string][]string{}
	}
	if err == nil && (st.Current == nil || st.Snapshots == nil || st.Batches == nil || st.Runs == nil || st.Operations == nil || st.Binding.Version != 1) {
		err = errors.New("invalid issue state")
	}
	if err == nil {
		for ref, r := range st.Snapshots {
			raw, _ := json.Marshal(r)
			if validRecord(r) != nil || ref != r.Key()+"@"+Hash(raw) || r.Workspace != st.Binding.Workspace {
				return st, errors.New("issue snapshot integrity failure")
			}
		}
		for key, ref := range st.Current {
			r, ok := st.Snapshots[ref]
			if !ok || key != r.Key() {
				return st, errors.New("issue current pointer integrity failure")
			}
		}
	}
	return st, err
}
func (s Store) mutate(fn func(*State) error) error {
	if s.Lock == nil || s.Write == nil {
		return errors.New("issue mutations require lock and atomic writer")
	}
	return s.Lock(func() error {
		st, err := s.Load()
		if err != nil {
			return err
		}
		if err = fn(&st); err != nil {
			return err
		}
		b, err := json.Marshal(st)
		if err != nil {
			return err
		}
		return s.Write(filepath.Join(s.Root, "state.json"), b)
	})
}
func Decode(data []byte, out any) error {
	if len(data) > MaxBatchBytes {
		return errors.New("issue input exceeds 2 MiB")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("expected exactly one JSON value")
	}
	return nil
}
func selected(st *State, p string) bool {
	for _, v := range st.Binding.Projects {
		if v == p {
			return true
		}
	}
	return false
}
func (s Store) Configure(b Binding) error {
	if b.Version != 1 || b.Provider == "" || !uuid.MatchString(b.Workspace) {
		return errors.New("version 1, provider and workspace UUID required")
	}
	seen := map[string]bool{}
	for _, p := range b.Projects {
		if !uuid.MatchString(p) || seen[p] {
			return errors.New("projects must be unique UUIDs")
		}
		seen[p] = true
	}
	return s.mutate(func(st *State) error {
		if st.Binding.Workspace != "" && (st.Binding.Workspace != b.Workspace || st.Binding.Provider != b.Provider) {
			return errors.New("repository already bound to another workspace/provider")
		}
		st.Binding = b
		return nil
	})
}
func validRecord(r Record) error {
	if !uuid.MatchString(r.ID) || !uuid.MatchString(r.Workspace) || !uuid.MatchString(r.Project) {
		return errors.New("record identities must be UUIDs")
	}
	switch r.Kind {
	case "project":
		if r.ID != r.Project {
			return errors.New("project identity mismatch")
		}
	case "issue":
	case "comment":
		if !uuid.MatchString(r.Issue) {
			return errors.New("comment requires issue UUID")
		}
	default:
		return errors.New("unknown record kind")
	}
	if r.Kind != "comment" && r.Issue != "" {
		return errors.New("only comments have an issue parent")
	}
	if r.UpdatedAt.IsZero() || r.ObservedAt.IsZero() {
		return errors.New("record timestamps required")
	}
	u, err := url.Parse(r.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("canonical HTTPS URL required")
	}
	if r.Availability != "" && r.Availability != "available" && r.Availability != "deleted" && r.Availability != "inaccessible" {
		return errors.New("invalid availability")
	}
	return nil
}

type ImportResult struct {
	Imported  int      `json:"imported"`
	Ignored   int      `json:"ignored"`
	Conflicts []string `json:"conflicts,omitempty"`
	Replay    bool     `json:"replay"`
}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

// Equal source timestamps may acquire better observations, but must not
// overwrite fields previously observed in full. Parent project membership is
// validated separately: moving an issue does not edit its comments remotely.
func compatibleObservation(old, next Record) bool {
	canComplete := func(names ...string) bool {
		incomplete := false
		for _, name := range names {
			if contains(next.Completeness.Missing, name) || contains(next.Completeness.Truncated, name) {
				return false
			}
			incomplete = incomplete || contains(old.Completeness.Missing, name) || contains(old.Completeness.Truncated, name)
		}
		return incomplete
	}
	completeText := func(before, after string, names ...string) string {
		if !canComplete(names...) {
			return before
		}
		// Providers may add ellipses or truncation notices; the available text
		// need not be a literal prefix of the complete field.
		return after
	}
	old.Text = completeText(old.Text, next.Text, "text", "description", "body")
	old.Title = completeText(old.Title, next.Title, "title")
	old.Alias = completeText(old.Alias, next.Alias, "alias", "identifier")
	old.URL = completeText(old.URL, next.URL, "url", "comment_url")
	fields := make(map[string]json.RawMessage, len(old.Fields))
	for key, value := range old.Fields {
		fields[key] = value
	}
	for key, value := range next.Fields {
		if canComplete("fields", key, "fields."+key) {
			fields[key] = value
		}
	}
	old.Fields = fields
	if old.Kind == "comment" {
		old.Project = next.Project
	}
	old.Completeness = next.Completeness
	return old.ContentHash() == next.ContentHash()
}

func (s Store) Import(data []byte) (res ImportResult, err error) {
	var e Envelope
	if err = Decode(data, &e); err != nil {
		return
	}
	if e.Version != 1 || !uuid.MatchString(e.Workspace) || !uuid.MatchString(e.RunID) || !uuid.MatchString(e.BatchID) || len(e.Records) > MaxRecords {
		return res, errors.New("invalid import version, UUIDs, or more than 100 records")
	}
	err = s.mutate(func(st *State) error {
		if st.Binding.Workspace != e.Workspace {
			return errors.New("workspace mismatch")
		}
		batch := e.RunID + ":" + e.BatchID
		digest := Hash(canonicalJSON(e))
		if prior, ok := st.Batches[batch]; ok {
			if prior != digest {
				return errors.New("batch ID reused with different payload")
			}
			res.Replay = true
			return nil
		}
		for _, r := range e.Records {
			if err := validRecord(r); err != nil {
				return err
			}
			if r.Workspace != e.Workspace {
				return errors.New("record workspace mismatch")
			}
			if !selected(st, r.Project) {
				// A previously imported issue may explicitly move outside scope. Keep that
				// observation so both the old issue and its comments disappear immediately.
				old, ok := st.Snapshots[st.Current[r.Key()]]
				if r.Kind != "issue" || !ok || old.Project == r.Project {
					return errors.New("project is not selected")
				}
			}
		}
		for _, r := range e.Records {
			ref := r.Key() + "@" + r.ContentHash()
			old, ok := st.Snapshots[st.Current[r.Key()]]
			if ok && old.UpdatedAt.After(r.UpdatedAt) {
				res.Ignored++
				continue
			}
			if ok && old.UpdatedAt.Equal(r.UpdatedAt) && !compatibleObservation(old, r) {
				res.Conflicts = append(res.Conflicts, r.Key())
				continue
			}
			if ok && old.UpdatedAt.Equal(r.UpdatedAt) && old.ObservedAt.After(r.ObservedAt) {
				res.Ignored++
				continue
			}
			// Snapshot includes observation time in its identity; a refresh never mutates
			// an earlier citation even when remote content did not change.
			raw, _ := json.Marshal(r)
			ref = r.Key() + "@" + Hash(raw)
			st.Snapshots[ref] = r
			st.Current[r.Key()] = ref
			res.Imported++
		}
		for _, r := range e.Records {
			if r.Kind == "comment" {
				parentKey := "issue:" + r.Workspace + ":issue:" + r.Issue
				p, ok := st.Snapshots[st.Current[parentKey]]
				if !ok || p.Project != r.Project {
					return errors.New("comment parent missing or project mismatch")
				}
			}
		}
		// Membership belongs to the run, even when a page is split into several
		// bounded batches and only the final batch carries pagination metadata.
		if st.RunIssues == nil {
			st.RunIssues = map[string][]string{}
		}
		for _, r := range e.Records {
			if r.Kind != "issue" {
				continue
			}
			key := e.RunID + ":" + r.Project
			if !contains(st.RunIssues[key], r.ID) {
				st.RunIssues[key] = append(st.RunIssues[key], r.ID)
				sort.Strings(st.RunIssues[key])
			}
			if prior, ok := st.Runs[key]; ok {
				if !contains(prior.IssueIDs, r.ID) {
					prior.IssueIDs = append(prior.IssueIDs, r.ID)
					sort.Strings(prior.IssueIDs)
				}
				if !prior.Comments[r.ID].Complete {
					prior.Complete = false
				}
				st.Runs[key] = prior
			}
		}
		if p := e.Progress; p != nil {
			if !selected(st, p.Project) || p.WindowStart.IsZero() {
				return errors.New("progress requires selected project and requested window")
			}
			key := e.RunID + ":" + p.Project
			prior, ok := st.Runs[key]
			if ok && !prior.WindowStart.Equal(p.WindowStart) {
				return errors.New("run window cannot change")
			}
			merged := map[string]Page{}
			seenIssues := map[string]bool{}
			for _, id := range st.RunIssues[key] {
				seenIssues[id] = true
			}
			for _, id := range prior.IssueIDs {
				seenIssues[id] = true
			}
			for id := range prior.Comments {
				seenIssues[id] = true
			}
			for _, r := range e.Records {
				if r.Kind == "issue" && r.Project == p.Project {
					seenIssues[r.ID] = true
				}
			}
			for _, id := range p.IssueIDs {
				seenIssues[id] = true
			}
			for k, v := range prior.Comments {
				merged[k] = v
			}
			for k, v := range p.Comments {
				if !uuid.MatchString(k) || v.Complete && v.Cursor != "" {
					return errors.New("invalid comment pagination")
				}
				if prev, ok := merged[k]; ok && prev.Complete && !v.Complete {
					return errors.New("comment pagination cannot regress")
				}
				merged[k] = v
			}
			p.Comments = merged
			for id := range merged {
				seenIssues[id] = true
			}
			p.IssueIDs = nil
			for id := range seenIssues {
				r, ok := st.Snapshots[st.Current["issue:"+e.Workspace+":issue:"+id]]
				if !uuid.MatchString(id) || !ok || r.Project != p.Project {
					return errors.New("pagination issue missing or outside project")
				}
				p.IssueIDs = append(p.IssueIDs, id)
			}
			sort.Strings(p.IssueIDs)
			if p.Issues.Complete && p.Issues.Cursor != "" || prior.Issues.Complete && !p.Issues.Complete {
				return errors.New("invalid issue pagination")
			}
			if p.Complete {
				if !p.Issues.Complete || len(res.Conflicts) > 0 {
					return errors.New("cannot complete unfinished or conflicting import")
				}
				for _, id := range p.IssueIDs {
					if !p.Comments[id].Complete {
						return errors.New("cannot complete before comment pagination finishes")
					}
				}
			}
			st.Runs[key] = *p
		}
		st.Batches[batch] = digest
		if st.BatchProjects == nil {
			st.BatchProjects = map[string][]string{}
		}
		projects := map[string]bool{}
		for _, r := range e.Records {
			projects[r.Project] = true
		}
		if e.Progress != nil {
			projects[e.Progress.Project] = true
		}
		for p := range projects {
			st.BatchProjects[batch] = append(st.BatchProjects[batch], p)
		}
		sort.Strings(st.BatchProjects[batch])
		return nil
	})
	return
}
func visible(st *State, r Record) bool {
	if !visibleRecord(st, r) {
		return false
	}
	if r.Kind != "comment" {
		return true
	}
	// Parent identity is typed: a comment must resolve directly to an issue.
	// Do not traverse arbitrary parent chains, even in malformed in-memory state.
	parentKey := "issue:" + r.Workspace + ":issue:" + r.Issue
	p, ok := st.Snapshots[st.Current[parentKey]]
	return ok && p.Kind == "issue" && p.Key() == parentKey && p.Project == r.Project && visibleRecord(st, p)
}

// visibleRecord checks only the record, its current revision and its project;
// it never follows a comment parent or calls visible.
func visibleRecord(st *State, r Record) bool {
	if !selected(st, r.Project) || r.Availability == "deleted" || r.Availability == "inaccessible" {
		return false
	}
	if r.Kind != "project" {
		p, ok := st.Snapshots[st.Current["issue:"+r.Workspace+":project:"+r.Project]]
		if ok && (p.Availability == "deleted" || p.Availability == "inaccessible") {
			return false
		}
	}
	current, ok := st.Snapshots[st.Current[r.Key()]]
	if !ok || current.Key() != r.Key() || !selected(st, current.Project) || current.Availability == "deleted" || current.Availability == "inaccessible" {
		return false
	}
	return true
}
func (s Store) Records() ([]Snapshot, error) {
	st, err := s.Load()
	if err != nil {
		return nil, err
	}
	out := []Snapshot{}
	for _, ref := range st.Current {
		r := st.Snapshots[ref]
		if visible(&st, r) {
			out = append(out, snapshotFor(&st, r, ref))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out, nil
}
func (s Store) Get(id string) (Snapshot, bool, error) {
	st, err := s.Load()
	if err != nil {
		return Snapshot{}, false, err
	}
	ref := id
	if !strings.Contains(id, "@") {
		ref = st.Current[id]
	}
	r, ok := st.Snapshots[ref]
	if !ok || !visible(&st, r) {
		return Snapshot{}, false, nil
	}
	return snapshotFor(&st, r, ref), true, nil
}

func snapshotFor(st *State, r Record, ref string) Snapshot {
	result := Snapshot{Record: r, Hash: r.ContentHash(), Ref: ref}
	coverage := map[string]bool{}
	for _, run := range st.Runs {
		if run.Project != r.Project {
			continue
		}
		if !run.Complete {
			coverage["Project import has unfinished pagination; absence is not evidence of deletion."] = true
		}
		for _, v := range run.Limitations {
			coverage[v] = true
		}
	}
	for v := range coverage {
		result.Coverage = append(result.Coverage, v)
	}
	sort.Strings(result.Coverage)
	return result
}
func (s Store) Resolve(value string) (Snapshot, error) {
	rs, err := s.Records()
	if err != nil {
		return Snapshot{}, err
	}
	var out []Snapshot
	for _, r := range rs {
		if r.Kind == "issue" && (strings.EqualFold(r.Alias, value) || r.URL == value || r.ID == value || r.Key() == value) {
			out = append(out, r)
		}
	}
	if len(out) != 1 {
		return Snapshot{}, fmt.Errorf("issue %q resolved to %d records in selected projects", value, len(out))
	}
	return out[0], nil
}
func (s Store) Link(l Link) error {
	return s.mutate(func(st *State) error {
		r, ok := st.Snapshots[st.Current[l.Issue]]
		if !ok || r.Kind != "issue" || !visible(st, r) {
			return errors.New("link requires a visible issue evidence ID")
		}
		switch l.Kind {
		case "session", "checkpoint", "commit", "pr":
		default:
			return errors.New("invalid link kind")
		}
		if strings.TrimSpace(l.Value) == "" {
			return errors.New("link value required")
		}
		for _, old := range st.Links {
			if old == l {
				return nil
			}
		}
		st.Links = append(st.Links, l)
		return nil
	})
}
func (s Store) Operation(op Operation) error {
	if !uuid.MatchString(op.ID) || !hashPattern.MatchString(op.PayloadHash) || op.ObservedAt.IsZero() {
		return errors.New("operation UUID, SHA256 payload hash and observation required")
	}
	switch op.Action {
	case "create", "comment", "status", "assignee", "priority", "labels", "project":
	default:
		return errors.New("unsupported operation action")
	}
	switch op.Outcome {
	case "pending", "succeeded", "failed", "unknown":
	default:
		return errors.New("invalid operation outcome")
	}
	return s.mutate(func(st *State) error {
		history := st.Operations[op.ID]
		if len(history) == 0 {
			if op.Outcome != "pending" {
				return errors.New("operation must start pending")
			}
			if op.Action == "create" {
				if !selected(st, op.Target) {
					return errors.New("create target must be selected project UUID")
				}
			} else {
				r, ok := st.Snapshots[st.Current[op.Target]]
				if !ok || r.Kind != "issue" || !visible(st, r) {
					return errors.New("operation target must be a visible issue ID")
				}
			}
		} else {
			prev := history[len(history)-1]
			if prev.Target != op.Target || prev.Action != op.Action || prev.PayloadHash != op.PayloadHash {
				return errors.New("operation intent cannot change")
			}
			a, _ := json.Marshal(prev)
			b, _ := json.Marshal(op)
			if bytes.Equal(a, b) {
				return nil
			}
			if prev.Outcome == "succeeded" || prev.Outcome == "failed" || op.Outcome == "pending" || op.ObservedAt.Before(prev.ObservedAt) {
				return errors.New("invalid operation transition")
			}
		}
		if op.Outcome == "succeeded" && (op.ObjectID == "" || op.URL == "") {
			return errors.New("success requires returned object ID and URL")
		}
		st.Operations[op.ID] = append(history, op)
		return nil
	})
}
func (s Store) Disconnect(project string, purge bool) error {
	if !uuid.MatchString(project) {
		return errors.New("project UUID required")
	}
	return s.mutate(func(st *State) error {
		if st.Binding.Version != 1 {
			return errors.New("issue store is not configured")
		}
		ps := []string{}
		for _, p := range st.Binding.Projects {
			if p != project {
				ps = append(ps, p)
			}
		}
		st.Binding.Projects = ps
		if purge {
			removed := map[string]bool{}
			for ref, r := range st.Snapshots {
				if r.Project == project {
					removed[r.Key()] = true
					delete(st.Snapshots, ref)
				}
			}
			for key, ref := range st.Current {
				if _, ok := st.Snapshots[ref]; !ok {
					delete(st.Current, key)
				}
			}
			links := st.Links[:0]
			for _, l := range st.Links {
				if !removed[l.Issue] {
					links = append(links, l)
				}
			}
			st.Links = links
			for id, ops := range st.Operations {
				if len(ops) > 0 && (removed[ops[0].Target] || ops[0].Target == project) {
					delete(st.Operations, id)
				}
			}
			for id, p := range st.Runs {
				if p.Project == project {
					delete(st.Runs, id)
				}
			}
			for key := range st.RunIssues {
				if strings.HasSuffix(key, ":"+project) {
					delete(st.RunIssues, key)
				}
			}
			for batch := range st.Batches {
				projects, known := st.BatchProjects[batch]
				remove := !known
				for _, p := range projects {
					if p == project {
						remove = true
					}
				}
				if remove {
					delete(st.Batches, batch)
					delete(st.BatchProjects, batch)
				}
			}
			if s.CleanDerived != nil {
				return s.CleanDerived()
			}
		}
		return nil
	})
}

type Status struct {
	Binding     Binding                `json:"binding"`
	Records     int                    `json:"records"`
	Stale       int                    `json:"stale"`
	Incomplete  int                    `json:"incomplete"`
	PendingRuns int                    `json:"pending_runs"`
	LimitedRuns int                    `json:"limited_runs"`
	Runs        map[string]Progress    `json:"runs,omitempty"`
	Links       []Link                 `json:"links,omitempty"`
	Operations  map[string][]Operation `json:"operations,omitempty"`
}

func (s Store) Status(now time.Time) (Status, error) {
	st, err := s.Load()
	if err != nil {
		return Status{}, err
	}
	out := Status{Binding: st.Binding, Runs: st.Runs, Links: st.Links, Operations: st.Operations}
	for _, run := range st.Runs {
		if !selected(&st, run.Project) {
			continue
		}
		if !run.Complete {
			out.PendingRuns++
		}
		if len(run.Limitations) > 0 {
			out.LimitedRuns++
		}
	}
	for _, ref := range st.Current {
		r := st.Snapshots[ref]
		if !visible(&st, r) {
			continue
		}
		out.Records++
		if now.Sub(r.ObservedAt) > 24*time.Hour {
			out.Stale++
		}
		if len(r.Completeness.Missing)+len(r.Completeness.Truncated)+len(r.Completeness.Limitations) > 0 {
			out.Incomplete++
		}
	}
	return out, nil
}
