//go:build !brain_cgo

package cli

// newVectorStore on the default (pure-Go) build is the 1a stack: the
// vectors.bin flat file with brute-force cosine ranking. The brain_cgo build
// swaps in the sqlite-vec vec0 store (embed_vec_cgo.go).
func newVectorStore(brainDir, branch, modelID string, dim int) vectorStore {
	return newEmbedStore(brainDir, branch, modelID, dim)
}

// newHistoryVectorStore has no pure-Go implementation, deliberately: history
// vectors number in the hundreds of thousands on large repos, and the flat
// vectors.bin design loads the whole file on every query. ok=false keeps the
// history semantic arm gated to BM25-only on this build (the validated
// fallback), rather than shipping a path that melts at exactly the corpus
// size the capstone validated fusion on.
func newHistoryVectorStore(brainDir, modelID string, dim int) (historyVectorStore, bool) {
	return nil, false
}

// newConversationVectorStore mirrors newHistoryVectorStore: the conversation
// semantic arm is vec0-only, so on this build the conversation source stays
// lexical (BM25) with an explicit vector-unavailable state.
func newConversationVectorStore(brainDir, modelID string, dim int) (historyVectorStore, bool) {
	return nil, false
}
