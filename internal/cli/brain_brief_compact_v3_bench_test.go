package cli

import (
	"strings"
	"testing"
)

var compactV3BenchmarkBytes int

func BenchmarkBrainBriefCompactV3Emitter(b *testing.B) {
	benchmarks := []struct {
		name   string
		report brainBriefReport
	}{
		{name: "realistic_multi_file", report: comprehensiveCompactV1Report()},
		{name: "exhaustive", report: comprehensiveCompactV2Report()},
		{name: "large_repeated", report: repeatedCompactV2Report(comprehensiveCompactV1Report(), 20)},
	}
	for _, benchmark := range benchmarks {
		b.Run(benchmark.name, func(b *testing.B) {
			var out strings.Builder
			cmd := (&cobraCommandForCompactV2Test{out: &out}).command()
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				out.Reset()
				if err := emitBrainBriefCompactV3(cmd, benchmark.report); err != nil {
					b.Fatal(err)
				}
			}
			compactV3BenchmarkBytes = out.Len()
		})
	}
}
