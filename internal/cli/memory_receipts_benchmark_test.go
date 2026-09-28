package cli

import (
	"fmt"
	"testing"
)

var receiptBenchmarkResult projectionReceipt

func BenchmarkProjectionReceiptLookup(b *testing.B) {
	for _, n := range []int{1000, 10000, 20000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			s := projectionState{Sessions: make([]projectionReceipt, n)}
			for i := range s.Sessions {
				s.Sessions[i].SessionRef = fmt.Sprintf("conversation-session:%08d", i)
			}
			b.ResetTimer()
			for k := 0; k < b.N; k++ {
				for _, r := range s.Sessions {
					receiptBenchmarkResult, _ = s.receiptFor(r.SessionRef)
				}
			}
		})
	}
}

func TestProjectionReceiptLookupBoundaries(t *testing.T) {
	s := projectionState{Sessions: []projectionReceipt{{SessionRef: "b"}, {SessionRef: "d"}, {SessionRef: "f"}}}
	for _, ref := range []string{"", "a", "b", "c", "d", "e", "f", "g"} {
		got, ok := s.receiptFor(ref)
		want := ref == "b" || ref == "d" || ref == "f"
		if ok != want || ok && got.SessionRef != ref {
			t.Fatalf("lookup %q: %+v %v", ref, got, ok)
		}
	}
	if _, ok := (projectionState{}).receiptFor("b"); ok {
		t.Fatal("empty state returned a receipt")
	}
}
