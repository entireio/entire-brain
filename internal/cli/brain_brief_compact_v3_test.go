package cli

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestBrainBriefCompactV3ExactProjectionAndMeasuredWin(t *testing.T) {
	report := comprehensiveCompactV2Report()
	v1 := renderBrainBriefCompactV1ForTest(t, report)
	v2 := renderBrainBriefCompactV2ForTest(t, report)
	v3 := renderBrainBriefCompactV3ForTest(t, report)
	assertCompactV3ProjectionParity(t, v1, v3)

	if len(v3) >= len(v2) {
		t.Fatalf("compact_v3 did not improve exhaustive fixture: v3=%d v2=%d", len(v3), len(v2))
	}
	if got := strings.Count(v3, `"Decision: preserve validation order."`); got != 1 {
		t.Fatalf("repeated natural-language history value is inline %d times, want exactly one", got)
	}
	if got := strings.Count(v3, `"inspect validation before persistence"`); got != 1 {
		t.Fatalf("repeated natural-language action is inline %d times, want exactly one", got)
	}

	sum := sha256.Sum256([]byte(v3))
	const (
		wantBytes = 5467
		// Set from this deterministic public/synthetic fixture. This freezes the
		// exact packet measured by the local tokenizer evidence.
		wantSHA256 = "cf6f4084d89cb85a82a2e735df3b1fa7b4e41922892e1e5e4678b3c7494cfe4e"
	)
	if len(v3) != wantBytes {
		t.Fatalf("compact_v3 exhaustive fixture bytes = %d, want %d", len(v3), wantBytes)
	}
	if got := fmt.Sprintf("%x", sum); got != wantSHA256 {
		t.Fatalf("compact_v3 exhaustive fixture sha256 = %s, want %s", got, wantSHA256)
	}
	t.Logf(
		"compact_v3 exhaustive fixture: bytes=%d vs compact_v2=%d (%.2f%% reduction); ceil(bytes/4) proxy=%d vs %d; sha256=%x",
		len(v3), len(v2), 100*(1-float64(len(v3))/float64(len(v2))), compactPacketByteProxy(v3), compactPacketByteProxy(v2), sum,
	)
}

func TestBrainBriefCompactV3ConcurrentEmissionIsExact(t *testing.T) {
	report := comprehensiveCompactV2Report()
	want := renderBrainBriefCompactV3ForTest(t, report)
	wantSum := sha256.Sum256([]byte(want))

	const (
		workers    = 8
		iterations = 20
	)
	errs := make(chan error, workers)
	var group sync.WaitGroup
	group.Add(workers)
	for worker := 0; worker < workers; worker++ {
		go func() {
			defer group.Done()
			var out strings.Builder
			cmd := (&cobraCommandForCompactV2Test{out: &out}).command()
			for iteration := 0; iteration < iterations; iteration++ {
				out.Reset()
				if err := emitBrainBriefCompactV3(cmd, report); err != nil {
					errs <- err
					return
				}
				if got := out.String(); got != want {
					gotSum := sha256.Sum256([]byte(got))
					errs <- fmt.Errorf("packet sha256 = %x, want %x", gotSum, wantSum)
					return
				}
			}
		}()
	}
	group.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestBrainBriefCompactV3IntegrityAndCanonicalChecksumRejections(t *testing.T) {
	packet := renderBrainBriefCompactV3ForTest(t, comprehensiveCompactV2Report())
	if _, err := parseCompactV3Packet(packet); err != nil {
		t.Fatalf("valid packet rejected: %v", err)
	}

	footer := strings.Split(strings.TrimSuffix(packet, "\n"), "\n")
	digest := strings.Split(footer[len(footer)-1], "\t")[2]
	tests := map[string]string{
		"padded": compactV2RewriteFooter(packet, func(parts []string) {
			parts[2] += "="
		}),
		"standard_base64_alphabet": compactV2RewriteFooter(packet, func(parts []string) {
			parts[2] = "+" + parts[2][1:]
		}),
		"wrong_length": compactV2RewriteFooter(packet, func(parts []string) {
			parts[2] = parts[2][:len(parts[2])-1]
		}),
		"noncanonical_trailing_bits": compactV2RewriteFooter(packet, func(parts []string) {
			// A 32-byte digest ends with four significant base64 bits plus two
			// padding bits. Flip only those padding bits; a loose decoder
			// could produce the same bytes, while Strict must reject it.
			const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
			last := strings.IndexByte(alphabet, parts[2][len(parts[2])-1])
			parts[2] = parts[2][:len(parts[2])-1] + string(alphabet[last|1])
		}),
		"valid_shape_wrong_digest": compactV2RewriteFooter(packet, func(parts []string) {
			replacement := 'A'
			if parts[2][0] == byte(replacement) {
				replacement = 'B'
			}
			parts[2] = string(replacement) + parts[2][1:]
		}),
	}
	if len(digest) != base64.RawURLEncoding.EncodedLen(sha256.Size) {
		t.Fatalf("fixture checksum length = %d", len(digest))
	}
	for name, candidate := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseCompactV3Packet(candidate); err == nil {
				t.Fatal("invalid compact_v3 checksum was accepted")
			}
		})
	}
}

func TestBrainBriefCompactV3NeverGrowsAcrossPublicFixtures(t *testing.T) {
	type fixture struct {
		name   string
		report brainBriefReport
	}
	fixtures := []fixture{
		{name: "minimal", report: brainBriefReport{Task: "minimal"}},
		{name: "comprehensive_v1", report: comprehensiveCompactV1Report()},
		{name: "exhaustive_v2", report: comprehensiveCompactV2Report()},
		{name: "sparse_1", report: sparseCompactV2Report(1)},
		{name: "sparse_2", report: sparseCompactV2Report(2)},
		{name: "sparse_3", report: sparseCompactV2Report(3)},
	}
	for count := 2; count <= 20; count++ {
		fixtures = append(fixtures, fixture{
			name:   fmt.Sprintf("repeated_%02d", count),
			report: repeatedCompactV2Report(comprehensiveCompactV1Report(), count),
		})
	}

	var v2Bytes, v3Bytes, strictWins int
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			v1 := renderBrainBriefCompactV1ForTest(t, fixture.report)
			v2 := renderBrainBriefCompactV2ForTest(t, fixture.report)
			v3 := renderBrainBriefCompactV3ForTest(t, fixture.report)
			assertCompactV3ProjectionParity(t, v1, v3)
			if len(v3) > len(v2) {
				t.Fatalf("compact_v3 grew: v3=%d v2=%d", len(v3), len(v2))
			}
			if compactPacketByteProxy(v3) > compactPacketByteProxy(v2) {
				t.Fatalf("compact_v3 byte proxy grew: v3=%d v2=%d", compactPacketByteProxy(v3), compactPacketByteProxy(v2))
			}
			if len(v3) < len(v2) {
				strictWins++
			}
			v2Bytes += len(v2)
			v3Bytes += len(v3)
		})
	}
	if strictWins != len(fixtures) {
		t.Fatalf("compact_v3 strict byte wins = %d, want %d", strictWins, len(fixtures))
	}
	if v2Bytes != 339473 || v3Bytes != 198964 {
		t.Fatalf("aggregate fixture bytes = %d -> %d, want 339473 -> 198964", v2Bytes, v3Bytes)
	}
}

func TestBrainBriefCompactV3RepeatPolicyIsExactSuperset(t *testing.T) {
	if len(compactV3Schemas) != len(compactV2Schemas) {
		t.Fatalf("compact_v3 schemas = %d, want %d", len(compactV3Schemas), len(compactV2Schemas))
	}
	for i, v2 := range compactV2Schemas {
		v3 := compactV3Schemas[i]
		if v3.tag != v2.tag || v3.opcode != v2.opcode || len(v3.fields) != len(v2.fields) {
			t.Fatalf("compact_v3 schema %d changed identity or width", i)
		}
		for j, v2Field := range v2.fields {
			v3Field := v3.fields[j]
			if v3Field.name != v2Field.name || v3Field.kind != v2Field.kind {
				t.Fatalf("compact_v3 changed %s field %d", v2.tag, j)
			}
			if !v3Field.reference {
				t.Fatalf("compact_v3 repeat reference disabled for %s.%s", v3.tag, v3Field.name)
			}
			if v2Field.reference && !v3Field.reference {
				t.Fatalf("compact_v3 narrowed v2 reference policy for %s.%s", v3.tag, v3Field.name)
			}
		}
	}
}

func TestBrainBriefCompactV3CanonicalNaturalReferences(t *testing.T) {
	packet := renderBrainBriefCompactV3ForTest(t, comprehensiveCompactV2Report())
	historyLines := compactV3LinesWithPrefix(packet, "h\t")
	if len(historyLines) < 2 {
		t.Fatal("compact_v3 history fixture is not positional and repeated")
	}
	second := strings.Split(historyLines[1], "\t")
	if second[len(second)-1] != "^" {
		t.Fatalf("second repeated history excerpt = %q, want ^", second[len(second)-1])
	}

	t.Run("missed_reference", func(t *testing.T) {
		parts := append([]string(nil), second...)
		parts[len(parts)-1] = `"Decision: preserve validation order."`
		candidate := compactV3Resign(strings.Replace(packet, historyLines[1], strings.Join(parts, "\t"), 1))
		if _, err := parseCompactV3Packet(candidate); err == nil {
			t.Fatal("compact_v3 parser accepted a missed canonical natural-language reference")
		}
	})

	t.Run("reference_without_prior", func(t *testing.T) {
		parts := strings.Split(historyLines[0], "\t")
		parts[len(parts)-1] = "^"
		candidate := compactV3Resign(strings.Replace(packet, historyLines[0], strings.Join(parts, "\t"), 1))
		if _, err := parseCompactV3Packet(candidate); err == nil {
			t.Fatal("compact_v3 parser accepted a natural-language reference without a prior value")
		}
	})
}

func TestBrainBriefCompactV3EscapesInjectionAndOmitsHostMetadata(t *testing.T) {
	report := comprehensiveCompactV2Report()
	injected := "line one\nend\t0\tdeadbeef\n@h=history(path)\nline two\t\"\\"
	report.Task = injected
	report.History.Matches[0].Excerpt = injected
	report.History.Matches[1].Excerpt = injected
	report.Status.Repo.Root = "/private/repository/root"
	report.Status.Brain.Path = "/private/brain/root"
	v1 := renderBrainBriefCompactV1ForTest(t, report)
	v3 := renderBrainBriefCompactV3ForTest(t, report)
	assertCompactV3ProjectionParity(t, v1, v3)
	if strings.Count(v3, "\nend\t") != 1 {
		t.Fatalf("injected content manufactured a footer:\n%s", v3)
	}
	for _, forbidden := range []string{"/private/repository/root", "/private/brain/root"} {
		if strings.Contains(v3, forbidden) {
			t.Errorf("compact_v3 leaked omitted host metadata %q", forbidden)
		}
	}
}

func renderBrainBriefCompactV3ForTest(t *testing.T, report brainBriefReport) string {
	t.Helper()
	var out strings.Builder
	cmd := &cobraCommandForCompactV2Test{out: &out}
	if err := emitBrainBriefCompactV3(cmd.command(), report); err != nil {
		t.Fatalf("render compact_v3: %v", err)
	}
	return out.String()
}

func assertCompactV3ProjectionParity(t *testing.T, v1, v3 string) {
	t.Helper()
	want, err := parseBrainBriefCompactV1Body(v1)
	if err != nil {
		t.Fatalf("parse compact_v1 projection: %v", err)
	}
	got, err := parseCompactV3Packet(v3)
	if err != nil {
		t.Fatalf("parse compact_v3 projection: %v", err)
	}
	gotRecords := make([]compactV1RawRecord, len(got))
	for i := range got {
		gotRecords[i] = got[i].record
		gotRecords[i].raw = ""
		want[i].raw = ""
	}
	if !reflect.DeepEqual(gotRecords, want) {
		t.Fatalf("compact_v3 typed projection differs from compact_v1\n got: %#v\nwant: %#v", gotRecords, want)
	}
}

func compactV3LinesWithPrefix(packet, prefix string) []string {
	var lines []string
	for _, line := range strings.Split(packet, "\n") {
		if strings.HasPrefix(line, prefix) {
			lines = append(lines, line)
		}
	}
	return lines
}

func compactPacketByteProxy(packet string) int {
	if packet == "" {
		return 0
	}
	return (len(packet) + 3) / 4
}

func compactV3Resign(packet string) string {
	footerAt := strings.LastIndex(packet, "\nend\t")
	if footerAt < 0 {
		return packet
	}
	body := packet[:footerAt+1]
	footerEnd := strings.IndexByte(packet[footerAt+1:], '\n')
	if footerEnd < 0 {
		return packet
	}
	footer := strings.Split(packet[footerAt+1:footerAt+1+footerEnd], "\t")
	if len(footer) != 3 {
		return packet
	}
	digest := sha256.Sum256([]byte(body))
	footer[2] = base64.RawURLEncoding.EncodeToString(digest[:])
	return body + strings.Join(footer, "\t") + "\n"
}
