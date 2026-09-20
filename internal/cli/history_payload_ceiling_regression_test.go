package cli

import "testing"

func TestFilteredPayloadCeilingFallsBackWithoutDroppingEligibleHits(t *testing.T) {
	old := historyFTSFilteredScanCeiling
	historyFTSFilteredScanCeiling = 2
	t.Cleanup(func() { historyFTSFilteredScanCeiling = old })
	index := historyIndex{Records: []historyRecord{
		{ID: "first", Kind: "decision", Path: "first", Summary: "bounded candidate needle"},
		{ID: "second", Kind: "decision", Path: "second", Summary: "bounded candidate needle"},
		{ID: "last", Kind: "decision", Path: "last", Summary: "bounded candidate needle"},
	}}
	dir, source := writeDirectHistoryFTSFixture(t, index)
	// Determine actual BM25 tie order instead of relying on an unspecified tie.
	var order []string
	_, used, err := rankHistoryViaFreshFTSCutoffDetailed(dir, source, "history", "bounded candidate needle", 1, 0, func(r historyRecord) bool { order = append(order, r.ID); return false })
	if err != nil {
		t.Fatal(err)
	}
	if used {
		t.Fatalf("payload claimed complete success after predicate ceiling: examined=%v", order)
	}
	if len(order) != 2 {
		t.Fatalf("hydrated %d records, want ceiling 2", len(order))
	}
	blocked := map[string]bool{}
	for _, id := range order {
		blocked[id] = true
	}
	pred := func(r historyRecord) bool { return !blocked[r.ID] }
	got, used, err := rankHistoryViaFreshFTSCutoffDetailed(dir, source, "history", "bounded candidate needle", 1, 0, pred)
	if err != nil || used || len(got) != 0 {
		t.Fatalf("partial payload result escaped: %+v used=%v err=%v", got, used, err)
	}
	ranked, access, _, err := rankFreshHistoryLexicalFromSource(dir, source, "history", "bounded candidate needle", 1, pred)
	if err != nil || len(ranked) != 1 || blocked[ranked[0].Record.ID] || access != historyIndexAccessJSON {
		t.Fatalf("eligible tail lost: %+v access=%s err=%v", ranked, access, err)
	}
	got, used, err = rankHistoryViaFreshFTSCutoffDetailed(dir, source, "history", "bounded candidate needle", 1, 0, func(historyRecord) bool { return true })
	if err != nil || !used || len(got) != 1 {
		t.Fatalf("complete bounded payload lost: %+v used=%v err=%v", got, used, err)
	}
}
