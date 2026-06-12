//go:build brain_cgo

package cli

import (
	"fmt"
	"testing"
)

// TestHistoryVecStoreKnnAtScale guards the k = row-count KNN query at history
// corpus sizes: sqlite-vec's vec0 MATCH has internal limits the facts store
// never hits at its few-hundred-row scale.
func TestHistoryVecStoreKnnAtScale(t *testing.T) {
	store, ok := newHistoryVectorStore(t.TempDir(), "scale-model", 4)
	if !ok {
		t.Fatal("store unavailable")
	}
	const n = 20000
	batch := map[string][]float32{}
	for i := 0; i < n; i++ {
		batch[fmt.Sprintf("r%05d", i)] = []float32{float32(i%7 + 1), float32(i%11 + 1), float32(i%13 + 1), 1}
		if len(batch) == 2048 {
			if err := store.upsert(batch, nil); err != nil {
				t.Fatal(err)
			}
			batch = map[string][]float32{}
		}
	}
	if err := store.upsert(batch, nil); err != nil {
		t.Fatal(err)
	}
	scores, ok := store.knnCos([]float32{1, 1, 1, 1}, 200)
	if !ok {
		t.Fatal("knnCos failed at 20k rows")
	}
	if len(scores) != 200 {
		t.Fatalf("knnCos returned %d of the requested 200", len(scores))
	}
	// And a request far above vec0's MATCH k ceiling must clamp, not error.
	scores, ok = store.knnCos([]float32{1, 1, 1, 1}, n)
	if !ok {
		t.Fatal("knnCos must clamp an over-ceiling k, not fail")
	}
	if len(scores) != vec0KnnMaxK {
		t.Fatalf("over-ceiling k must clamp to %d, got %d", vec0KnnMaxK, len(scores))
	}
}
