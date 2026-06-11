package cli

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
)

// embedStore persists fact vectors under facts/<branch>/embeddings/ so an
// interactive recall/brief does not re-embed the whole branch on every CLI
// invocation (each process otherwise starts with a cold in-memory cache). The
// file is a regenerable derived artifact, like facts.ndjson itself.
//
// Keying is by content-derived fact id, and the header records the embedder's
// model id + dim: a model swap (or any dim change) fails the load and triggers
// a clean rebuild, so stale vectors from a different backend are never mixed in.
type embedStore struct {
	path     string // absolute path to vectors.bin
	brainDir string
	relPath  string
	modelID  string
	dim      int
}

const (
	embedStoreFileName = "vectors.bin"
	embedStoreDirName  = "embeddings"
	embedStoreMagic    = "EBV1"
)

func newEmbedStore(brainDir, branch, modelID string, dim int) *embedStore {
	rel := filepath.ToSlash(filepath.Join(factsBranchRelDir(branch), embedStoreDirName, embedStoreFileName))
	return &embedStore{path: filepath.Join(brainDir, filepath.FromSlash(rel)), brainDir: brainDir, relPath: rel, modelID: modelID, dim: dim}
}

// load reads persisted vectors, returning an empty map (not an error) whenever
// the cache is absent, unreadable, or built for a different model/dim — every
// such case is a cache miss that the caller refills by embedding. A cache is
// never load-bearing, so corruption degrades to a rebuild rather than failing.
func (s *embedStore) load() map[string][]float32 {
	return s.loadUnlocked()
}

func (s *embedStore) loadUnlocked() map[string][]float32 {
	out := map[string][]float32{}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return out
	}
	r := &byteReader{b: raw}
	if string(r.take(4)) != embedStoreMagic {
		return out
	}
	modelID := string(r.take(int(r.u16())))
	dim := int(r.u32())
	count := int(r.u32())
	if r.err != nil || modelID != s.modelID || dim != s.dim || dim <= 0 {
		return out // model/dim mismatch -> full rebuild
	}
	for i := 0; i < count; i++ {
		id := string(r.take(int(r.u16())))
		vec := make([]float32, dim)
		for d := 0; d < dim; d++ {
			vec[d] = math.Float32frombits(r.u32())
		}
		if r.err != nil {
			return map[string][]float32{} // truncated/corrupt -> rebuild
		}
		out[id] = vec
	}
	return out
}

// save atomically writes the given vectors. The caller passes only the vectors
// for facts present this run, so removed/superseded facts are pruned on rewrite.
func (s *embedStore) save(vecs map[string][]float32) error {
	return s.savePresent(vecs, nil)
}

func (s *embedStore) savePresent(vecs map[string][]float32, present map[string]struct{}) error {
	if s.brainDir != "" && s.relPath != "" {
		return withBrainWriteLock(s.brainDir, func() error {
			merged := s.loadUnlocked()
			if present != nil {
				for id := range merged {
					if _, ok := present[id]; !ok {
						delete(merged, id)
					}
				}
			}
			for id, vec := range vecs {
				if present != nil {
					if _, ok := present[id]; !ok {
						continue
					}
				}
				merged[id] = vec
			}
			return s.saveUnlocked(merged)
		})
	}
	return s.saveUnlocked(vecs)
}

func (s *embedStore) saveUnlocked(vecs map[string][]float32) error {
	// Count the entries we will actually write (skipping any wrong-dim vector)
	// so the header count matches the body exactly — a mismatch would make the
	// next load() see a truncated file and force an unnecessary rebuild.
	count := 0
	for _, vec := range vecs {
		if len(vec) == s.dim {
			count++
		}
	}

	var buf bytes.Buffer
	buf.WriteString(embedStoreMagic)
	id := []byte(s.modelID)
	_ = binary.Write(&buf, binary.LittleEndian, uint16(len(id)))
	buf.Write(id)
	_ = binary.Write(&buf, binary.LittleEndian, uint32(s.dim))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(count))
	for factID, vec := range vecs {
		if len(vec) != s.dim {
			continue
		}
		_ = binary.Write(&buf, binary.LittleEndian, uint16(len(factID)))
		buf.WriteString(factID)
		for _, v := range vec {
			_ = binary.Write(&buf, binary.LittleEndian, math.Float32bits(v))
		}
	}
	if s.brainDir != "" && s.relPath != "" {
		return writeBrainRelativeFileAtomic(s.brainDir, s.relPath, buf.Bytes(), 0o600)
	}
	return writeFileAtomic(s.path, buf.Bytes(), 0o600)
}
