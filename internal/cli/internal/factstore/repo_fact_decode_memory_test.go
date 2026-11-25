package factstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"reflect"
	"strings"
	"testing"
)

// Freeze the original strict streaming decoder as the compatibility oracle.
func referenceStreamingDecodeV3(ctx context.Context, data []byte) (map[string]factmerge.Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var s repoFactStore
	if _, err := brainstore.DecodeStrictVersionedJSON(data, &s, repoFactsVersion, "repository memory store"); err != nil {
		return nil, err
	}
	if s.SchemaVersion != repoFactsVersion || s.Records == nil {
		return nil, fmt.Errorf("invalid repository memory store header")
	}
	out := make(map[string]factmerge.Record, len(s.Records))
	for id, raw := range s.Records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var rec factmerge.Record
		if err := json.Unmarshal(raw, &rec); err != nil {
			return nil, fmt.Errorf("invalid repository memory record %s: %w", id, err)
		}
		if rec.ID != id {
			return nil, fmt.Errorf("repository memory record %s carries id %q; the record the key names is gone — %s", id, rec.ID, RepairHint())
		}
		out[id] = rec
	}
	return out, nil
}

func TestDirectStoreDecodeMatchesStrictStreaming(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"canonical":         `{"schema_version":3,"records":{"one":{"id":"one","text":"café 数据库","paths":["architecture.retry"],"extra_record_field":1},"two":{"id":"two"}}}`,
		"empty":             `{"schema_version":3,"records":{}}`,
		"missing":           `{"schema_version":3}`,
		"null records":      `{"schema_version":3,"records":null}`,
		"null document":     `null`,
		"malformed":         `{"schema_version":3,`,
		"trailing":          `{"schema_version":3,"records":{}} {}`,
		"future version":    `{"schema_version":4,"records":{}}`,
		"old version":       `{"schema_version":2,"records":{}}`,
		"unknown top-level": `{"schema_version":3,"records":{},"future_field":1}`,
		"field aliases":     `{"SCHEMA_VERSION":3,"Records":{"one":{"id":"one"}}}`,
		"duplicate objects": `{"schema_version":3,"records":{"one":{"id":"wrong"}},"records":{"one":{"id":"one"}}}`,
		"alias merge":       `{"schema_version":3,"records":{"one":{"id":"one"}},"Records":{"two":{"id":"two"}}}`,
		"bad records type":  `{"schema_version":3,"records":[]}`,
		"bad record type":   `{"schema_version":3,"records":{"one":123}}`,
		"bad record id":     `{"schema_version":3,"records":{"one":{"id":"two"}}}`,
		"null record":       `{"schema_version":3,"records":{"one":null}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			data := []byte(body)
			before := bytes.Clone(data)
			want, wantErr := referenceStreamingDecodeV3(context.Background(), data)
			got, gotErr := decodeV3Context(context.Background(), data)
			if fmt.Sprint(gotErr) != fmt.Sprint(wantErr) || !reflect.DeepEqual(got, want) {
				t.Fatalf("decode changed: got=%+v err=%v; want=%+v err=%v", got, gotErr, want, wantErr)
			}
			if !bytes.Equal(data, before) {
				t.Fatal("decoding modified the input buffer")
			}
		})
	}
}

func largeDecodeFixture(t testing.TB) []byte {
	t.Helper()
	raw, err := json.Marshal(factmerge.Record{ID: "large", Text: strings.Repeat("x", 4<<20)})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(repoFactStore{SchemaVersion: repoFactsVersion, Records: map[string]json.RawMessage{"large": raw}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// Benchmark allocation accounting is process-wide, so this test is serial.
func TestDirectStoreDecodeAvoidsStreamingBuffer(t *testing.T) {
	data := largeDecodeFixture(t)
	result := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			records, err := decodeV3Context(context.Background(), data)
			if err != nil || len(records) != 1 {
				b.Fatalf("records=%d err=%v", len(records), err)
			}
		}
	})
	if allocated := result.AllocedBytesPerOp(); allocated > int64(len(data))*4 {
		t.Fatalf("decoding %d already-loaded bytes allocated %d bytes; must not grow another streaming buffer", len(data), allocated)
	}
}

func BenchmarkDecodeV3Buffering(b *testing.B) {
	data := largeDecodeFixture(b)
	for _, tc := range []struct {
		name   string
		decode func(context.Context, []byte) (map[string]factmerge.Record, error)
	}{
		{"streaming_reference", referenceStreamingDecodeV3}, {"direct", decodeV3Context},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				if _, err := tc.decode(context.Background(), data); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
