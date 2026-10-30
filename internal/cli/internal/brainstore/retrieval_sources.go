package brainstore

// Retrieval source selectors. Phase 1 of the conversational-memory plan:
// "all" retains the pre-Phase-1 source set (facts + classified history + docs);
// conversation exchanges are returned only when explicitly selected.
const (
	RetrievalSourceAll          = "all"
	RetrievalSourceFact         = "fact"
	RetrievalSourceHistory      = "history"
	RetrievalSourceConversation = "conversation"
	RetrievalSourceDoc          = "doc"
)
