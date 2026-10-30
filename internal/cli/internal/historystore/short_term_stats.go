package historystore

// ShortTermStats summarizes one short-term memory delta pass: how many
// transcripts changed, what the overlay now carries, and whether the pass was
// truncated or incomplete (the signals the watch tick escalates on).
type ShortTermStats struct {
	Files               int  `json:"files"`
	Records             int  `json:"records"`
	Exchanges           int  `json:"exchanges"`
	IncompleteExchanges int  `json:"incomplete_exchanges"`
	Reused              int  `json:"reused_files"`
	Scanned             int  `json:"scanned_files"`
	Truncated           bool `json:"truncated,omitempty"`
	// Failed counts transcripts this delta could not scan; the overlay
	// records their identities durably.
	Failed int `json:"failed_files,omitempty"`
}
