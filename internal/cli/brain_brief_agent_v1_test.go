package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"
)

func TestBrainBriefAgentV1PreservesFactsAndTrustWithoutProvenance(t *testing.T) {
	report := comprehensiveCompactV1Report()
	report.GeneratedAt = time.Date(2031, 2, 3, 4, 5, 6, 0, time.UTC)
	report.Status.Repo.Root = "/Users/private/repository"
	report.Status.Brain.Path = "/Users/private/.entire/brain"
	report.History.Matches[0].Path = "sessions/private-transcript.jsonl"
	report.History.Matches[0].Timestamp = "private-history-timestamp"
	report.Facts[0].Provenance = []factAnchor{{
		SessionID:    "private-session",
		CheckpointID: "private-checkpoint",
		Transcript:   "sessions/private-fact-transcript.jsonl",
		Line:         91,
		Verified:     true,
	}}
	report.Facts[0].CreatedAt = time.Date(2032, 3, 4, 5, 6, 7, 0, time.UTC)
	report.Facts[0].UpdatedAt = time.Date(2033, 4, 5, 6, 7, 8, 0, time.UTC)
	report.Facts = append(report.Facts, factRecord{
		ID:         "fact:2",
		Paths:      []string{"quality.validation"},
		Kind:       "invariant",
		Locus:      []string{"pkg.Persist"},
		Text:       "Keep exact fact text, including a newline.\nDo not normalize it.",
		Branch:     "feature",
		Origin:     "authored",
		Status:     "active",
		Confidence: "medium",
		Provenance: []factAnchor{{SessionID: "second-private-session", Transcript: "sessions/second.jsonl"}},
	})
	report.FactsPendingReview = map[string]factReviewNotice{
		"fact:1": {
			ReviewID:   "review:merge-1",
			Action:     "merge",
			Confidence: 0.75,
			Message:    "verify these facts together",
			RelatedIDs: []string{"fact:2"},
		},
	}
	report.Warnings = append(report.Warnings,
		factReviewQueueUnavailableWarning,
		"unrelated warning contains /Users/private/diagnostic",
	)

	projection, err := buildBrainBriefAgentV1(report, brainBriefDeliveryAlways, 3)
	if err != nil {
		t.Fatalf("build agent_v1: %v", err)
	}
	if len(projection.packet) > brainBriefAgentV1BudgetBytes {
		t.Fatalf("agent_v1 bytes = %d, budget = %d", len(projection.packet), brainBriefAgentV1BudgetBytes)
	}
	records := parseAgentV1Records(t, projection.packet)

	facts := agentV1RecordsByTag(records, "fact")
	if len(facts) != len(report.Facts) {
		t.Fatalf("facts = %d, want %d", len(facts), len(report.Facts))
	}
	for i, fact := range report.Facts {
		if got := agentV1StringField(t, facts[i], "id"); got != fact.ID {
			t.Fatalf("fact[%d].id = %q, want %q", i, got, fact.ID)
		}
		if got := agentV1StringField(t, facts[i], "text"); got != fact.Text {
			t.Fatalf("fact[%d].text = %q, want exact %q", i, got, fact.Text)
		}
		if got := agentV1IntField(t, facts[i], "rank"); got != i {
			t.Fatalf("fact[%d].rank = %d", i, got)
		}
		for _, forbiddenField := range []string{"branch", "origin", "anchors", "provenance", "created_at", "updated_at"} {
			if _, exists := agentV1Fields(facts[i])[forbiddenField]; exists {
				t.Errorf("fact[%d] retained forbidden field %q", i, forbiddenField)
			}
		}
	}
	if got := agentV1StringArrayField(t, facts[0], "stale_locus"); len(got) != 1 || got[0] != "OldValidate" {
		t.Fatalf("fact stale_locus = %#v", got)
	}

	reviews := agentV1RecordsByTag(records, "fact_review")
	if len(reviews) != 1 || agentV1StringField(t, reviews[0], "fact_id") != "fact:1" ||
		agentV1StringField(t, reviews[0], "review_id") != "review:merge-1" {
		t.Fatalf("fact reviews = %#v", reviews)
	}
	warnings := agentV1RecordsByTag(records, "trust_warning")
	if len(warnings) != 1 || agentV1StringField(t, warnings[0], "code") != "proposal_state_unavailable" {
		t.Fatalf("trust warnings = %#v", warnings)
	}

	config := agentV1RecordsByTag(records, "config")
	if len(config) != 1 {
		t.Fatalf("config records = %d", len(config))
	}
	if got := agentV1StringField(t, config[0], "delivery_policy"); got != "always" {
		t.Fatalf("delivery policy = %q", got)
	}
	if got := agentV1IntField(t, config[0], "requested_limit"); got != 3 {
		t.Fatalf("requested limit = %d", got)
	}
	if got := agentV1IntField(t, config[0], "effective_fact_limit"); got != 3 {
		t.Fatalf("effective fact limit = %d", got)
	}
	if got, want := agentV1StringField(t, config[0], "config_sha256"), brainBriefAgentV1ConfigIdentity(brainBriefDeliveryAlways, 3, 3); got != want {
		t.Fatalf("config identity = %q, want %q", got, want)
	}

	for _, forbidden := range []string{
		"/Users/private/repository",
		"/Users/private/.entire/brain",
		"/Users/private/diagnostic",
		"private-history-timestamp",
		"sessions/private-transcript.jsonl",
		"private-session",
		"private-checkpoint",
		"sessions/private-fact-transcript.jsonl",
		"second-private-session",
		"sessions/second.jsonl",
		"2031-02-03T04:05:06",
		"2032-03-04T05:06:07",
		"2033-04-05T06:07:08",
	} {
		if strings.Contains(projection.packet, forbidden) {
			t.Errorf("agent_v1 leaked omitted provenance %q", forbidden)
		}
	}
}

func TestBrainBriefAgentV1DoesNotSortDeduplicateOrTimestampFilterFacts(t *testing.T) {
	report := comprehensiveCompactV1Report()
	report.FactsLocusDrift = nil
	report.FactsPendingReview = nil
	report.Facts = []factRecord{
		{ID: "fact:z", Text: "first despite newest timestamp", Status: "superseded", CreatedAt: time.Unix(300, 0), UpdatedAt: time.Unix(900, 0)},
		{ID: "fact:a", Text: "second despite oldest timestamp", Status: "retracted", CreatedAt: time.Unix(100, 0), UpdatedAt: time.Unix(100, 0)},
		{ID: "fact:z", Text: "duplicate id remains a separate selected record", Status: "active", CreatedAt: time.Unix(200, 0), UpdatedAt: time.Unix(500, 0)},
	}

	projection, err := buildBrainBriefAgentV1(report, brainBriefDeliveryAlways, 6)
	if err != nil {
		t.Fatalf("build agent_v1: %v", err)
	}
	facts := agentV1RecordsByTag(parseAgentV1Records(t, projection.packet), "fact")
	if len(facts) != len(report.Facts) {
		t.Fatalf("facts = %d, want %d", len(facts), len(report.Facts))
	}
	for i, selected := range report.Facts {
		if got := agentV1IntField(t, facts[i], "rank"); got != i {
			t.Fatalf("fact[%d].rank = %d", i, got)
		}
		if got := agentV1StringField(t, facts[i], "id"); got != selected.ID {
			t.Fatalf("fact[%d].id = %q, want %q", i, got, selected.ID)
		}
		if got := agentV1StringField(t, facts[i], "text"); got != selected.Text {
			t.Fatalf("fact[%d].text changed", i)
		}
	}
	if strings.Contains(projection.packet, "1970-") {
		t.Fatal("agent_v1 emitted timestamp tie-break metadata")
	}
}

func TestBrainBriefAgentV1ConfigIdentityBindsSchemaPolicyBudgetAndLimits(t *testing.T) {
	const want = "sha256:93f16698c95ad042cefe517a0171d6a0a8d30cf0a61880d5803317c8cd112f0e"
	if got := brainBriefAgentV1ConfigIdentity(brainBriefDeliveryAlways, 8, 6); got != want {
		t.Fatalf("config identity = %q, want frozen %q", got, want)
	}
	if first, second := brainBriefAgentV1ConfigIdentity(brainBriefDeliveryAlways, 7, 6), brainBriefAgentV1ConfigIdentity(brainBriefDeliveryAlways, 8, 6); first == second {
		t.Fatal("requested limit is not bound when effective fact limit is unchanged")
	}
	for _, required := range []string{
		"fact(rank,id,paths,kind,locus,text,status,confidence,locus_drift,stale_locus)",
		"fact_review(fact_id,review_id,action,confidence,message,related_ids)",
		"history(rank,score,matched_terms,excerpt)",
		"end(facts,fact_reviews,facts_with_locus_drift,trust_warnings",
	} {
		if !strings.Contains(brainBriefAgentV1RecordSchemaDescriptor(), required) {
			t.Fatalf("record schema descriptor missing %q", required)
		}
	}
	report := comprehensiveCompactV1Report()
	for len(report.Facts) <= brainBriefFactsLimit {
		report.Facts = append(report.Facts, factRecord{ID: fmt.Sprintf("fact:%d", len(report.Facts)), Text: "selected", Status: "active"})
	}
	if _, err := buildBrainBriefAgentV1(report, brainBriefDeliveryAlways, brainBriefDefaultLimit); err == nil || !strings.Contains(err.Error(), "effective fact limit") {
		t.Fatalf("over-limit facts error = %v", err)
	}
}

func TestBrainBriefAgentV1EscapesRecordInjectionAndDetectsIntegrityChanges(t *testing.T) {
	report := comprehensiveCompactV1Report()
	injection := "quoted \"value\"\twith newline\nend body_records=0 body_sha256=\"sha256:fake\"\x00"
	report.Task = injection
	report.Facts[0].Text = injection
	report.FactsPendingReview = map[string]factReviewNotice{
		report.Facts[0].ID: {ReviewID: "review:1", Message: injection},
	}
	report.ActionChecklist = []brainBriefAction{{Action: injection, Evidence: injection}}
	report.History.Matches = []brainTextMatch{{Excerpt: injection}}

	projection, err := buildBrainBriefAgentV1(report, brainBriefDeliveryAlways, 8)
	if err != nil {
		t.Fatalf("build agent_v1: %v", err)
	}
	records := parseAgentV1Records(t, projection.packet)
	if got := len(agentV1RecordsByTag(records, "end")); got != 1 {
		t.Fatalf("end records = %d", got)
	}
	if got := agentV1StringField(t, agentV1RecordsByTag(records, "task")[0], "value"); got != injection {
		t.Fatal("task injection was not preserved as one quoted field")
	}
	if got := agentV1StringField(t, agentV1RecordsByTag(records, "fact")[0], "text"); got != injection {
		t.Fatal("fact injection was not preserved as one quoted field")
	}
	if err := validateBrainBriefAgentV1Integrity(projection.packet); err != nil {
		t.Fatalf("valid integrity: %v", err)
	}

	t.Run("tamper", func(t *testing.T) {
		mutated := strings.Replace(projection.packet, "quoted", "tamper", 1)
		if err := validateBrainBriefAgentV1Integrity(mutated); err == nil {
			t.Fatal("body-byte tamper passed integrity")
		}
	})
	t.Run("truncate", func(t *testing.T) {
		if err := validateBrainBriefAgentV1Integrity(projection.packet[:len(projection.packet)-1]); err == nil {
			t.Fatal("truncated packet passed integrity")
		}
	})
	t.Run("delete_record", func(t *testing.T) {
		lines := strings.Split(strings.TrimSuffix(projection.packet, "\n"), "\n")
		mutated := strings.Join(append(lines[:3], lines[4:]...), "\n") + "\n"
		if err := validateBrainBriefAgentV1Integrity(mutated); err == nil {
			t.Fatal("record deletion passed integrity")
		}
	})
	t.Run("reorder", func(t *testing.T) {
		lines := strings.Split(strings.TrimSuffix(projection.packet, "\n"), "\n")
		lines[2], lines[3] = lines[3], lines[2]
		if err := validateBrainBriefAgentV1Integrity(strings.Join(lines, "\n") + "\n"); err == nil {
			t.Fatal("record reorder passed integrity")
		}
	})
}

func TestBrainBriefAgentV1UTF8ByteBudgetBoundary(t *testing.T) {
	report := comprehensiveCompactV1Report()
	report.History.Matches = nil
	report.ActionChecklist = nil
	report.LikelyEditFiles = nil
	report.LikelyTestFiles = nil
	report.LikelyFiles = nil
	report.Semantic = brainBriefSemantic{}

	fits := func(runes int) bool {
		report.Facts[0].Text = strings.Repeat("界", runes)
		_, err := buildBrainBriefAgentV1(report, brainBriefDeliveryAlways, 8)
		return err == nil
	}
	low, high := 0, brainBriefAgentV1BudgetBytes
	for low+1 < high {
		mid := low + (high-low)/2
		if fits(mid) {
			low = mid
		} else {
			high = mid
		}
	}
	if !fits(low) || fits(high) {
		t.Fatalf("UTF-8 boundary = fits(%d)=%t fits(%d)=%t", low, fits(low), high, fits(high))
	}
	text := strings.Repeat("界", high)
	if utf8.RuneCountInString(text) >= len(text) {
		t.Fatal("test text did not exercise multibyte UTF-8")
	}
}

func TestBrainBriefAgentV1EndRecordSizingDecimalBoundaries(t *testing.T) {
	testCounts := func(value int) brainBriefAgentV1Counts {
		return brainBriefAgentV1Counts{
			facts: value, factReviews: value, factsWithLocusDrift: value, trustWarnings: value,
			editFiles: value, testFiles: value, likelyFiles: value, actions: value,
			symbols: value, testSuggestions: value, history: value,
		}
	}
	tests := []struct {
		name                   string
		emitted, available     brainBriefAgentV1Counts
		bodyRecords, bodyBytes int
	}{
		{name: "zero_complete", emitted: testCounts(0), available: testCounts(0)},
		{name: "single_digit_complete", emitted: testCounts(8), available: testCounts(8), bodyRecords: 9, bodyBytes: 9},
		{name: "single_to_double_truncated", emitted: testCounts(9), available: testCounts(10), bodyRecords: 10, bodyBytes: 10},
		{name: "double_digit_complete", emitted: testCounts(10), available: testCounts(10), bodyRecords: 99, bodyBytes: 99},
		{name: "double_to_triple_truncated", emitted: testCounts(99), available: testCounts(100), bodyRecords: 100, bodyBytes: 100},
		{name: "triple_digit_complete", emitted: testCounts(100), available: testCounts(100), bodyRecords: 999, bodyBytes: 999},
		{name: "triple_to_four_truncated", emitted: testCounts(999), available: testCounts(1000), bodyRecords: 1000, bodyBytes: 1000},
		{name: "packet_budget_width", emitted: testCounts(0), available: testCounts(0), bodyRecords: 9999, bodyBytes: brainBriefAgentV1BudgetBytes},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			hash := sha256.Sum256([]byte(test.name))
			bodySHA256 := fmt.Sprintf("sha256:%x", hash)
			got := brainBriefAgentV1EndRecordLine(
				test.emitted, test.available, test.bodyRecords, test.bodyBytes, bodySHA256,
			)
			want := legacyBrainBriefAgentV1EndRecordLineForTest(
				test.emitted, test.available, test.bodyRecords, test.bodyBytes, bodySHA256,
			)
			if got != want {
				t.Fatalf("end record changed\ngot:  %s\nwant: %s", got, want)
			}
			if gotBytes := brainBriefAgentV1EncodeEndRecord(
				nil, test.emitted, test.available, test.bodyRecords, test.bodyBytes, bodySHA256,
			); gotBytes != len(got) {
				t.Fatalf("encoded end bytes = %d, actual = %d", gotBytes, len(got))
			}
			if gotPacketBytes := brainBriefAgentV1SizedPacketBytes(
				test.bodyBytes, test.bodyRecords, test.emitted, test.available,
			); gotPacketBytes != test.bodyBytes+len(got) {
				t.Fatalf("sized packet bytes = %d, actual = %d", gotPacketBytes, test.bodyBytes+len(got))
			}
		})
	}

	if len(brainBriefAgentV1SizingSHA256) != len("sha256:")+sha256.Size*2 ||
		strconv.Quote(brainBriefAgentV1SizingSHA256) != `"`+brainBriefAgentV1SizingSHA256+`"` {
		t.Fatalf("sizing SHA is not a fixed-width unescaped SHA-256 value: %q", brainBriefAgentV1SizingSHA256)
	}
	for _, value := range []int{-1000, -100, -10, -9, 0, 9, 10, 99, 100, 999, 1000, brainBriefAgentV1BudgetBytes} {
		if got, want := brainBriefAgentV1DecimalBytes(value), len(strconv.Itoa(value)); got != want {
			t.Errorf("decimal bytes(%d) = %d, want %d", value, got, want)
		}
	}
}

func TestBrainBriefAgentV1EndRecordSizingRecomputesTruncation(t *testing.T) {
	emitted := brainBriefAgentV1Counts{history: 8}
	available := emitted
	completeBytes := brainBriefAgentV1SizedPacketBytes(100, 9, emitted, available)
	available.history = 9
	truncatedBytes := brainBriefAgentV1SizedPacketBytes(100, 9, emitted, available)
	if truncatedBytes != completeBytes-1 {
		t.Fatalf("truncation flip bytes = %d, complete = %d, want one byte shorter true/false width", truncatedBytes, completeBytes)
	}
	complete := brainBriefAgentV1EndRecordLine(emitted, emitted, 9, 100, brainBriefAgentV1SizingSHA256)
	truncated := brainBriefAgentV1EndRecordLine(emitted, available, 9, 100, brainBriefAgentV1SizingSHA256)
	if !strings.Contains(complete, " truncated=false ") || !strings.Contains(truncated, " truncated=true ") {
		t.Fatalf("truncation field did not flip\ncomplete:  %s\ntruncated: %s", complete, truncated)
	}
}

func TestBrainBriefAgentV1BudgetIsDeterministicAndKeepsHistoryPrefix(t *testing.T) {
	report := comprehensiveCompactV1Report()
	report.Semantic = brainBriefSemantic{}
	report.ActionChecklist = nil
	report.LikelyEditFiles = nil
	report.LikelyTestFiles = nil
	report.LikelyFiles = nil
	report.History.Matches = make([]brainTextMatch, 80)
	report.Warnings = append(report.Warnings, factReviewQueueUnavailableWarning)
	for i := range report.History.Matches {
		report.History.Matches[i] = brainTextMatch{
			Path:         fmt.Sprintf("sessions/private-%03d.jsonl", i),
			Line:         i + 1,
			Timestamp:    fmt.Sprintf("private-time-%03d", i),
			Score:        100 - i,
			MatchedTerms: []string{"validation", fmt.Sprintf("term-%03d", i)},
			Excerpt:      fmt.Sprintf("history-%03d %s", i, strings.Repeat("evidence ", 90)),
		}
	}

	first, err := buildBrainBriefAgentV1(report, brainBriefDeliveryAlways, 8)
	if err != nil {
		t.Fatalf("first build: %v", err)
	}
	second, err := buildBrainBriefAgentV1(report, brainBriefDeliveryAlways, 8)
	if err != nil {
		t.Fatalf("second build: %v", err)
	}
	if first.packet != second.packet {
		t.Fatal("agent_v1 packet is not deterministic")
	}
	if len(first.packet) > brainBriefAgentV1BudgetBytes {
		t.Fatalf("agent_v1 bytes = %d, budget = %d", len(first.packet), brainBriefAgentV1BudgetBytes)
	}

	records := parseAgentV1Records(t, first.packet)
	history := agentV1RecordsByTag(records, "history")
	if len(history) == 0 || len(history) >= len(report.History.Matches) {
		t.Fatalf("history records = %d, want a non-empty strict prefix of %d", len(history), len(report.History.Matches))
	}
	for i, record := range history {
		if got := agentV1IntField(t, record, "rank"); got != i {
			t.Fatalf("history[%d].rank = %d", i, got)
		}
		if got := agentV1StringField(t, record, "excerpt"); got != report.History.Matches[i].Excerpt {
			t.Fatalf("history[%d].excerpt changed", i)
		}
		if fields := agentV1Fields(record); fields["path"] != "" || fields["line"] != "" || fields["timestamp"] != "" {
			t.Fatalf("history[%d] retained transcript provenance: %#v", i, fields)
		}
	}
	end := agentV1RecordsByTag(records, "end")
	if len(end) != 1 || agentV1RawField(t, end[0], "truncated") != "true" {
		t.Fatalf("end record did not report truncation: %#v", end)
	}
	if got := projectionHistoryCount(first); got != len(history) {
		t.Fatalf("profile history count = %d, packet history = %d", got, len(history))
	}
	if facts := agentV1RecordsByTag(records, "fact"); len(facts) != len(report.Facts) {
		t.Fatalf("mandatory facts = %d, want %d", len(facts), len(report.Facts))
	}
	if warnings := agentV1RecordsByTag(records, "trust_warning"); len(warnings) != 1 {
		t.Fatalf("mandatory proposal-state warning count = %d", len(warnings))
	}
	profile := first.counts.profileCounts()
	if got, want := profile.Facts, agentV1IntField(t, end[0], "facts"); got != want {
		t.Fatalf("profile facts = %d, serialized = %d", got, want)
	}
	if got, want := profile.HistoryMatches, agentV1IntField(t, end[0], "history"); got != want {
		t.Fatalf("profile history = %d, serialized = %d", got, want)
	}
	if got, want := profile.Warnings, agentV1IntField(t, end[0], "trust_warnings"); got != want {
		t.Fatalf("profile warnings = %d, serialized = %d", got, want)
	}
}

func TestBrainBriefAgentV1OptionalPriorityUsesSectionPrefixes(t *testing.T) {
	report := comprehensiveCompactV1Report()
	report.Semantic = brainBriefSemantic{}
	report.History.Matches = nil
	report.ActionChecklist = nil
	report.LikelyFiles = nil
	report.LikelyEditFiles = make([]string, 100)
	for i := range report.LikelyEditFiles {
		report.LikelyEditFiles[i] = fmt.Sprintf("internal/%03d-%s.go", i, strings.Repeat("x", 550))
	}
	report.LikelyTestFiles = []string{"internal/a_test.go", "internal/b_test.go"}

	projection, err := buildBrainBriefAgentV1(report, brainBriefDeliveryAlways, 8)
	if err != nil {
		t.Fatalf("build agent_v1: %v", err)
	}
	records := parseAgentV1Records(t, projection.packet)
	edits := agentV1RecordsByTag(records, "edit_file")
	if len(edits) == 0 || len(edits) >= len(report.LikelyEditFiles) {
		t.Fatalf("edit files = %d, want non-empty strict prefix of %d", len(edits), len(report.LikelyEditFiles))
	}
	for i, record := range edits {
		if got := agentV1StringField(t, record, "path"); got != report.LikelyEditFiles[i] {
			t.Fatalf("edit_file[%d] leapfrogged prefix: %q", i, got)
		}
	}
	tests := agentV1RecordsByTag(records, "test_file")
	for i, record := range tests {
		if got := agentV1StringField(t, record, "path"); got != report.LikelyTestFiles[i] {
			t.Fatalf("test_file[%d] leapfrogged prefix: %q", i, got)
		}
	}
	end := agentV1RecordsByTag(records, "end")[0]
	if got := agentV1RawField(t, end, "truncated"); got != "true" {
		t.Fatalf("truncated = %s", got)
	}
	if got := agentV1IntField(t, end, "available_edit_files"); got != len(report.LikelyEditFiles) {
		t.Fatalf("available_edit_files = %d", got)
	}
}

func TestBrainBriefAgentV1FiltersUnsafeStructuredPathsButPreservesProse(t *testing.T) {
	for _, unsafe := range []string{
		"/Users/private/value", `C:\private\value`, `\\server\share\value`, "../escape", "a/../../escape",
		"~/private", "$HOME/private", "${HOME}/private", "file:///Users/private/value", "https://host/private",
	} {
		if agentV1SafeStructuredValue(unsafe) {
			t.Errorf("unsafe structured value accepted: %q", unsafe)
		}
	}
	for _, safe := range []string{"internal/a.go", "docs/guide.md", "pkg.Type.Method", "architecture.validation.order"} {
		if !agentV1SafeStructuredValue(safe) {
			t.Errorf("safe structured value rejected: %q", safe)
		}
	}
	report := comprehensiveCompactV1Report()
	report.Status.Live.ChangedFiles = []string{"internal/safe.go", "/Users/private/live.go", `C:\private\live.go`, `\\server\share\live.go`, "../escape.go"}
	report.Facts[0].Paths = []string{"architecture.safe", "/Users/private/fact-path"}
	report.Facts[0].Locus = []string{"pkg.Safe", `C:\private\Fact`}
	report.Facts[0].Text = "Exact prose may mention /Users/preserved-in-fact-text and must remain unchanged."
	report.FactsLocusDrift[report.Facts[0].ID] = []string{"pkg.Old", "../private/stale"}
	report.LikelyEditFiles = []string{"internal/safe.go", "/Users/private/edit.go"}
	report.LikelyTestFiles = []string{"internal/safe_test.go", `C:\private\test.go`}
	report.LikelyFiles = []string{"docs/safe.md", `\\server\share\likely.md`}
	report.ActionChecklist = []brainBriefAction{{File: "/Users/private/action.go", Symbol: "../private.Symbol", Action: "Prose /Users/preserved-in-action"}}
	report.Semantic.Context.Symbols[0].FilePath = `C:\private\symbol.go`
	report.Semantic.Tests.Suggestions[0].Symbol.FilePath = "../private_test.go"

	projection, err := buildBrainBriefAgentV1(report, brainBriefDeliveryAlways, 8)
	if err != nil {
		t.Fatalf("build agent_v1: %v", err)
	}
	for _, forbidden := range []string{
		"/Users/private/live.go", `C:\private\live.go`, `\\server\share\live.go`, "../escape.go",
		"/Users/private/fact-path", `C:\private\Fact`, "../private/stale", "/Users/private/edit.go",
		`C:\private\test.go`, `\\server\share\likely.md`, "/Users/private/action.go", "../private.Symbol",
		`C:\private\symbol.go`, "../private_test.go",
	} {
		if strings.Contains(projection.packet, forbidden) {
			t.Errorf("unsafe structured path leaked: %q", forbidden)
		}
	}
	for _, preserved := range []string{"internal/safe.go", "internal/safe_test.go", "docs/safe.md", "pkg.Safe", "pkg.Old", "/Users/preserved-in-fact-text", "/Users/preserved-in-action"} {
		if !strings.Contains(projection.packet, preserved) {
			t.Errorf("safe structured value or exact prose missing: %q", preserved)
		}
	}
}

func TestBrainBriefAgentV1PreservesAllUnsafeLocusDriftTrustState(t *testing.T) {
	report := comprehensiveCompactV1Report()
	factID := report.Facts[0].ID
	report.FactsLocusDrift[factID] = []string{"/Users/private/old.go", "../private/older.go"}

	projection, err := buildBrainBriefAgentV1(report, brainBriefDeliveryAlways, 8)
	if err != nil {
		t.Fatalf("build agent_v1: %v", err)
	}
	records := parseAgentV1Records(t, projection.packet)
	facts := agentV1RecordsByTag(records, "fact")
	if len(facts) == 0 {
		t.Fatal("agent_v1 fact missing")
	}
	fields := agentV1Fields(facts[0])
	if got := fields["locus_drift"]; got != "true" {
		t.Fatalf("fact locus_drift = %q, want true", got)
	}
	if _, exists := fields["stale_locus"]; exists {
		t.Fatalf("unsafe stale_locus was emitted: %s", fields["stale_locus"])
	}
	end := agentV1RecordsByTag(records, "end")
	if len(end) != 1 || agentV1IntField(t, end[0], "facts_with_locus_drift") != 1 {
		t.Fatalf("facts_with_locus_drift did not retain trust state: %#v", end)
	}
	for _, unsafe := range report.FactsLocusDrift[factID] {
		if strings.Contains(projection.packet, unsafe) {
			t.Fatalf("unsafe drift value leaked: %q", unsafe)
		}
	}
}

func TestBrainBriefAgentV1WithCountsMatchesEmittedFooter(t *testing.T) {
	fixtures := []struct {
		name          string
		newReport     func() brainBriefReport
		wantTruncated string
	}{
		{name: "rich", newReport: newBrainBriefPacketMeasurementReport, wantTruncated: "false"},
		{name: "near_budget_history", newReport: newBrainBriefAgentV1NearBudgetHistoryReport, wantTruncated: "true"},
	}
	for _, fixture := range fixtures {
		for _, policy := range []brainBriefDeliveryPolicy{brainBriefDeliveryAlways, brainBriefDeliveryShadow} {
			t.Run(fixture.name+"/"+string(policy), func(t *testing.T) {
				report := fixture.newReport()
				before := brainBriefPacketMeasurementFingerprint(t, report)
				projection, err := buildBrainBriefAgentV1(report, brainBriefDeliveryAlways, brainBriefPacketMeasurementLimit)
				if err != nil {
					t.Fatalf("build: %v", err)
				}

				var out bytes.Buffer
				cmd := &cobra.Command{}
				cmd.SetOut(&out)
				emittedCounts, err := emitBrainBriefAgentV1WithCounts(
					cmd, report, policy, brainBriefPacketMeasurementLimit,
				)
				if err != nil {
					t.Fatalf("emit with counts: %v", err)
				}
				if out.String() != projection.packet {
					t.Fatal("emission bytes differ from the always-bound direct projection")
				}

				footerCounts, truncated := agentV1ProfileCountsFromFooter(t, out.String())
				if truncated != fixture.wantTruncated {
					t.Fatalf("truncated = %s, want %s", truncated, fixture.wantTruncated)
				}
				if got := emittedCounts.profileCounts(); got != footerCounts {
					t.Fatalf("emitted counts = %+v, footer = %+v", got, footerCounts)
				}
				profileCounts, err := brainBriefProfilePacketCounts(report, brainBriefPacketAgentV1, &emittedCounts)
				if err != nil {
					t.Fatalf("profile counts: %v", err)
				}
				if profileCounts != footerCounts {
					t.Fatalf("profile counts = %+v, footer = %+v", profileCounts, footerCounts)
				}
				if after := brainBriefPacketMeasurementFingerprint(t, report); after != before {
					t.Fatalf("emission mutated fixture: before=%s after=%s", before, after)
				}
			})
		}
	}
}

func TestBrainBriefAgentV1WithCountsReleasesNothingOnFailure(t *testing.T) {
	tests := []struct {
		name          string
		mutate        func(*brainBriefReport)
		writerFailure bool
		wantError     string
	}{
		{name: "writer", writerFailure: true, wantError: errVitalityTestOutput.Error()},
		{
			name: "mandatory_overflow",
			mutate: func(report *brainBriefReport) {
				report.Facts[0].Text = strings.Repeat("x", brainBriefAgentV1BudgetBytes)
			},
			wantError: "mandatory packet",
		},
		{
			name: "nonfinite",
			mutate: func(report *brainBriefReport) {
				report.FactsPendingReview = map[string]factReviewNotice{
					report.Facts[0].ID: {ReviewID: "review:nan", Confidence: math.NaN(), Message: "review"},
				}
			},
			wantError: "agent_v1 cannot encode non-finite",
		},
	}
	for _, test := range tests {
		for _, policy := range []brainBriefDeliveryPolicy{brainBriefDeliveryAlways, brainBriefDeliveryShadow} {
			t.Run(test.name+"/"+string(policy), func(t *testing.T) {
				report := comprehensiveCompactV1Report()
				if test.mutate != nil {
					test.mutate(&report)
				}
				var out bytes.Buffer
				cmd := &cobra.Command{}
				if test.writerFailure {
					cmd.SetOut(failingVitalityWriter{})
				} else {
					cmd.SetOut(&out)
				}
				counts, err := emitBrainBriefAgentV1WithCounts(cmd, report, policy, 8)
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error = %v, want containing %q", err, test.wantError)
				}
				if counts != (brainBriefAgentV1Counts{}) {
					t.Fatalf("failed emission released counts: %+v", counts)
				}
				if out.Len() != 0 {
					t.Fatalf("failed emission wrote %d bytes", out.Len())
				}
			})
		}
	}
}

func TestBrainBriefAgentV1RejectsNonFiniteConfidenceBeforeWrite(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*brainBriefReport)
	}{
		{
			name: "review_nan",
			mutate: func(report *brainBriefReport) {
				report.FactsPendingReview = map[string]factReviewNotice{
					report.Facts[0].ID: {ReviewID: "review:nan", Confidence: math.NaN(), Message: "review"},
				}
			},
		},
		{
			name: "review_positive_infinity",
			mutate: func(report *brainBriefReport) {
				report.FactsPendingReview = map[string]factReviewNotice{
					report.Facts[0].ID: {ReviewID: "review:inf", Confidence: math.Inf(1), Message: "review"},
				}
			},
		},
		{
			name: "symbol_negative_infinity",
			mutate: func(report *brainBriefReport) {
				report.Semantic.Context.Symbols[0].Confidence = math.Inf(-1)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := comprehensiveCompactV1Report()
			tt.mutate(&report)
			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&out)
			err := emitBrainBriefAgentV1(cmd, report, brainBriefDeliveryAlways, 8)
			if err == nil || !strings.Contains(err.Error(), "agent_v1 cannot encode non-finite") {
				t.Fatalf("non-finite error = %v", err)
			}
			if out.Len() != 0 {
				t.Fatalf("non-finite input wrote %d bytes", out.Len())
			}
		})
	}
}

func TestBrainBriefAgentV1IsDeterministicAcrossTrustMapInsertionOrder(t *testing.T) {
	firstReport := comprehensiveCompactV1Report()
	firstReport.Facts = append(firstReport.Facts, factRecord{ID: "fact:2", Text: "second", Status: "active"})
	firstReport.FactsLocusDrift = map[string][]string{}
	firstReport.FactsLocusDrift["fact:1"] = []string{"OldOne"}
	firstReport.FactsLocusDrift["fact:2"] = []string{"OldTwo"}
	firstReport.FactsPendingReview = map[string]factReviewNotice{}
	firstReport.FactsPendingReview["fact:1"] = factReviewNotice{ReviewID: "review:1", Message: "first"}
	firstReport.FactsPendingReview["fact:2"] = factReviewNotice{ReviewID: "review:2", Message: "second"}

	secondReport := firstReport
	secondReport.FactsLocusDrift = map[string][]string{}
	secondReport.FactsLocusDrift["fact:2"] = []string{"OldTwo"}
	secondReport.FactsLocusDrift["fact:1"] = []string{"OldOne"}
	secondReport.FactsPendingReview = map[string]factReviewNotice{}
	secondReport.FactsPendingReview["fact:2"] = factReviewNotice{ReviewID: "review:2", Message: "second"}
	secondReport.FactsPendingReview["fact:1"] = factReviewNotice{ReviewID: "review:1", Message: "first"}

	first, err := buildBrainBriefAgentV1(firstReport, brainBriefDeliveryAlways, 8)
	if err != nil {
		t.Fatalf("first build: %v", err)
	}
	second, err := buildBrainBriefAgentV1(secondReport, brainBriefDeliveryAlways, 8)
	if err != nil {
		t.Fatalf("second build: %v", err)
	}
	if first.packet != second.packet {
		t.Fatal("agent_v1 depends on trust-map insertion order")
	}
}

func TestBrainBriefAgentV1MandatoryOverflowFailsBeforeWrite(t *testing.T) {
	report := comprehensiveCompactV1Report()
	report.Facts[0].Text = strings.Repeat("x", brainBriefAgentV1BudgetBytes)

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	err := emitBrainBriefAgentV1(cmd, report, brainBriefDeliveryAlways, 8)
	const wantError = "agent_v1 mandatory packet is 33868 bytes, exceeds 32768-byte budget"
	if err == nil || err.Error() != wantError {
		t.Fatalf("overflow error = %v, want %q", err, wantError)
	}
	if out.Len() != 0 {
		t.Fatalf("overflow wrote %d partial bytes", out.Len())
	}
}

func TestBrainBriefAgentV1RejectsAdaptivePolicyBeforeWrite(t *testing.T) {
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	err := emitBrainBriefAgentV1(cmd, comprehensiveCompactV1Report(), brainBriefDeliveryPolicy("adaptive"), 8)
	if err == nil || !strings.Contains(err.Error(), "must be always") {
		t.Fatalf("adaptive policy error = %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("invalid policy wrote %d bytes", out.Len())
	}
}

func TestBrainBriefAgentV1FailedEmissionWritesNoVitalityReceipt(t *testing.T) {
	fixture := newVerifyFixture(t)
	fact := vitalityTestFact("Agent packet receipts happen only after successful emission.", "main", fixture.now)
	if err := writeFacts(fixture.brainDir, "main", []factRecord{fact}); err != nil {
		t.Fatalf("write facts: %v", err)
	}
	if err := updateFactSourceManifest(fixture.brainDir, fixture.now); err != nil {
		t.Fatalf("update manifest: %v", err)
	}
	cmd := &cobra.Command{}
	cmd.SetOut(failingVitalityWriter{})
	cmd.SetErr(io.Discard)
	err := runBrainBrief(context.Background(), cmd, fixture.opts, brainBriefOptions{
		limit:          8,
		noSemantic:     true,
		surface:        "mcp:brain_brief",
		packetFormat:   brainBriefPacketAgentV1,
		deliveryPolicy: brainBriefDeliveryAlways,
	}, "successful emission receipts")
	if err == nil || !errors.Is(err, errVitalityTestOutput) {
		t.Fatalf("failed agent_v1 output returned %v", err)
	}
	if got := vitalitySidecarBytes(t, fixture.brainDir, "main"); len(got) != 0 {
		t.Fatalf("failed agent_v1 emission wrote a receipt: %s", got)
	}
}

func legacyBrainBriefAgentV1EndRecordLineForTest(
	emitted, available brainBriefAgentV1Counts,
	bodyRecords, bodyBytes int,
	bodySHA256 string,
) string {
	return agentV1RecordLine("end",
		compactV1IntAlways("facts", emitted.facts),
		compactV1IntAlways("fact_reviews", emitted.factReviews),
		compactV1IntAlways("facts_with_locus_drift", emitted.factsWithLocusDrift),
		compactV1IntAlways("trust_warnings", emitted.trustWarnings),
		compactV1IntAlways("edit_files", emitted.editFiles),
		compactV1IntAlways("test_files", emitted.testFiles),
		compactV1IntAlways("likely_files", emitted.likelyFiles),
		compactV1IntAlways("actions", emitted.actions),
		compactV1IntAlways("symbols", emitted.symbols),
		compactV1IntAlways("test_suggestions", emitted.testSuggestions),
		compactV1IntAlways("history", emitted.history),
		compactV1IntAlways("available_facts", available.facts),
		compactV1IntAlways("available_fact_reviews", available.factReviews),
		compactV1IntAlways("available_trust_warnings", available.trustWarnings),
		compactV1IntAlways("available_edit_files", available.editFiles),
		compactV1IntAlways("available_test_files", available.testFiles),
		compactV1IntAlways("available_likely_files", available.likelyFiles),
		compactV1IntAlways("available_actions", available.actions),
		compactV1IntAlways("available_symbols", available.symbols),
		compactV1IntAlways("available_test_suggestions", available.testSuggestions),
		compactV1IntAlways("available_history", available.history),
		compactV1Bool("truncated", emitted.truncated(available)),
		compactV1IntAlways("body_records", bodyRecords),
		compactV1IntAlways("body_bytes", bodyBytes),
		compactV1StringAlways("body_sha256", bodySHA256),
	)
}

func agentV1ProfileCountsFromFooter(t *testing.T, packet string) (brainBriefProfileCounts, string) {
	t.Helper()
	records := parseAgentV1Records(t, packet)
	end := agentV1RecordsByTag(records, "end")
	if len(end) != 1 {
		t.Fatalf("end records = %d, want 1", len(end))
	}
	return brainBriefProfileCounts{
		SemanticSymbols:     agentV1IntField(t, end[0], "symbols"),
		TestSuggestions:     agentV1IntField(t, end[0], "test_suggestions"),
		HistoryMatches:      agentV1IntField(t, end[0], "history"),
		Facts:               agentV1IntField(t, end[0], "facts"),
		FactsWithLocusDrift: agentV1IntField(t, end[0], "facts_with_locus_drift"),
		Warnings:            agentV1IntField(t, end[0], "trust_warnings"),
		Actions:             agentV1IntField(t, end[0], "actions"),
		LikelyEditFiles:     agentV1IntField(t, end[0], "edit_files"),
		LikelyTestFiles:     agentV1IntField(t, end[0], "test_files"),
		LikelyFiles:         agentV1IntField(t, end[0], "likely_files"),
	}, agentV1RawField(t, end[0], "truncated")
}

func projectionHistoryCount(projection brainBriefAgentV1Projection) int {
	return projection.counts.profileCounts().HistoryMatches
}

func parseAgentV1Records(t *testing.T, packet string) []compactV1RawRecord {
	t.Helper()
	if !strings.HasSuffix(packet, "\n") {
		t.Fatal("agent_v1 packet is missing final newline")
	}
	lines := strings.Split(strings.TrimSuffix(packet, "\n"), "\n")
	if len(lines) < 5 || lines[0] != brainBriefAgentV1Marker {
		t.Fatalf("agent_v1 marker = %q", lines[0])
	}
	records := make([]compactV1RawRecord, 0, len(lines)-1)
	for _, line := range lines[1:] {
		record, err := parseCompactV1RawRecord(line)
		if err != nil {
			t.Fatalf("parse agent_v1 record %q: %v", line, err)
		}
		records = append(records, record)
	}
	end := records[len(records)-1]
	if end.tag != "end" {
		t.Fatalf("last record = %q, want end", end.tag)
	}
	body := strings.Join(lines[:len(lines)-1], "\n") + "\n"
	if got, want := agentV1IntField(t, end, "body_records"), len(records)-1; got != want {
		t.Fatalf("body_records = %d, want %d", got, want)
	}
	if got, want := agentV1IntField(t, end, "body_bytes"), len(body); got != want {
		t.Fatalf("body_bytes = %d, want %d", got, want)
	}
	hash := sha256.Sum256([]byte(body))
	if got, want := agentV1StringField(t, end, "body_sha256"), fmt.Sprintf("sha256:%x", hash); got != want {
		t.Fatalf("body_sha256 = %q, want %q", got, want)
	}
	return records
}

func agentV1RecordsByTag(records []compactV1RawRecord, tag string) []compactV1RawRecord {
	var out []compactV1RawRecord
	for _, record := range records {
		if record.tag == tag {
			out = append(out, record)
		}
	}
	return out
}

func agentV1Fields(record compactV1RawRecord) map[string]string {
	out := make(map[string]string, len(record.fields))
	for _, field := range record.fields {
		out[field.key] = field.value
	}
	return out
}

func agentV1RawField(t *testing.T, record compactV1RawRecord, key string) string {
	t.Helper()
	value, ok := agentV1Fields(record)[key]
	if !ok {
		t.Fatalf("%s.%s missing", record.tag, key)
	}
	return value
}

func agentV1StringField(t *testing.T, record compactV1RawRecord, key string) string {
	t.Helper()
	raw := agentV1RawField(t, record, key)
	value, err := strconv.Unquote(raw)
	if err != nil {
		t.Fatalf("decode %s.%s: %v", record.tag, key, err)
	}
	return value
}

func agentV1IntField(t *testing.T, record compactV1RawRecord, key string) int {
	t.Helper()
	value, err := strconv.Atoi(agentV1RawField(t, record, key))
	if err != nil {
		t.Fatalf("decode %s.%s: %v", record.tag, key, err)
	}
	return value
}

func agentV1StringArrayField(t *testing.T, record compactV1RawRecord, key string) []string {
	t.Helper()
	raw := agentV1RawField(t, record, key)
	if len(raw) < 2 || raw[0] != '[' || raw[len(raw)-1] != ']' {
		t.Fatalf("decode %s.%s: %q is not an array", record.tag, key, raw)
	}
	if raw == "[]" {
		return nil
	}
	var out []string
	for cursor := 1; cursor < len(raw)-1; {
		end, err := scanCompactQuoted(raw, cursor)
		if err != nil {
			t.Fatalf("decode %s.%s: %v", record.tag, key, err)
		}
		value, err := strconv.Unquote(raw[cursor:end])
		if err != nil {
			t.Fatalf("decode %s.%s item: %v", record.tag, key, err)
		}
		out = append(out, value)
		cursor = end
		if cursor < len(raw)-1 {
			if raw[cursor] != ',' {
				t.Fatalf("decode %s.%s: missing comma", record.tag, key)
			}
			cursor++
		}
	}
	return out
}
