//go:build !brain_cgo

package cli

// newVectorStore on the default (pure-Go) build is the 1a stack: the
// vectors.bin flat file with brute-force cosine ranking. The brain_cgo build
// swaps in the sqlite-vec vec0 store (embed_vec_cgo.go).
func newVectorStore(brainDir, branch, modelID string, dim int) vectorStore {
	return newEmbedStore(brainDir, branch, modelID, dim)
}
