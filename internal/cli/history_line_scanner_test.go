package cli

import (
	"bufio"
	"io"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
)

type scannedLine struct {
	Text      string
	Oversized bool
}

func scanAllHistoryLines(t *testing.T, r io.Reader, maxLine int) ([]scannedLine, int) {
	t.Helper()
	scanner := newBoundedHistoryLineScanner(r, maxLine)
	var lines []scannedLine
	for scanner.Scan() {
		lines = append(lines, scannedLine{Text: scanner.Text(), Oversized: scanner.Oversized()})
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan error: %v", err)
	}
	return lines, scanner.Skipped()
}

// referenceHistoryLines is the contract in plain code: lines split on "\n"
// like bufio.ScanLines (a trailing "\r" dropped, no empty line after a final
// newline), and a line of maxLine bytes or more, the ones bufio.Scanner
// rejects with ErrTooLong, comes back as an empty, oversized placeholder.
func referenceHistoryLines(input string, maxLine int) []scannedLine {
	var lines []scannedLine
	for rest := input; rest != ""; {
		line := rest
		if i := strings.IndexByte(rest, '\n'); i >= 0 {
			line, rest = rest[:i], rest[i+1:]
		} else {
			rest = ""
		}
		if len(line) >= maxLine {
			lines = append(lines, scannedLine{Oversized: true})
			continue
		}
		lines = append(lines, scannedLine{Text: strings.TrimSuffix(line, "\r")})
	}
	return lines
}

func TestHistoryLineScannerSkipsOnlyLinesTooLongToRead(t *testing.T) {
	t.Parallel()
	const maxLine = 16
	long := strings.Repeat("x", maxLine)
	cases := []struct {
		name  string
		input string
		want  []scannedLine
	}{
		{name: "empty", input: ""},
		{name: "longest readable line", input: strings.Repeat("a", maxLine-1) + "\nb\n", want: []scannedLine{{Text: strings.Repeat("a", maxLine-1)}, {Text: "b"}}},
		{name: "shortest oversized line", input: "a\n" + long + "\nb\n", want: []scannedLine{{Text: "a"}, {Oversized: true}, {Text: "b"}}},
		{name: "oversized across many buffers", input: "a\n" + strings.Repeat("y", maxLine*40+3) + "\nb", want: []scannedLine{{Text: "a"}, {Oversized: true}, {Text: "b"}}},
		{name: "oversized first line", input: long + "z\na\n", want: []scannedLine{{Oversized: true}, {Text: "a"}}},
		{name: "oversized last line without newline", input: "a\n" + long + "zz", want: []scannedLine{{Text: "a"}, {Oversized: true}}},
		{name: "consecutive oversized lines", input: long + "\n" + long + long + "\n\na\n", want: []scannedLine{{Oversized: true}, {Oversized: true}, {Text: ""}, {Text: "a"}}},
		{name: "crlf", input: "a\r\n" + long + "\r\nb\r\n", want: []scannedLine{{Text: "a"}, {Oversized: true}, {Text: "b"}}},
	}
	readers := map[string]func(string) io.Reader{
		"whole":    func(s string) io.Reader { return strings.NewReader(s) },
		"one byte": func(s string) io.Reader { return iotest.OneByteReader(strings.NewReader(s)) },
		"half":     func(s string) io.Reader { return iotest.HalfReader(strings.NewReader(s)) },
	}
	for _, tc := range cases {
		for readerName, reader := range readers {
			t.Run(tc.name+"/"+readerName, func(t *testing.T) {
				t.Parallel()
				if ref := referenceHistoryLines(tc.input, maxLine); !reflect.DeepEqual(ref, tc.want) {
					t.Fatalf("reference = %+v, want %+v", ref, tc.want)
				}
				got, skipped := scanAllHistoryLines(t, reader(tc.input), maxLine)
				if !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("lines = %+v, want %+v", got, tc.want)
				}
				wantSkipped := 0
				for _, line := range tc.want {
					if line.Oversized {
						wantSkipped++
					}
				}
				if skipped != wantSkipped {
					t.Fatalf("skipped = %d, want %d", skipped, wantSkipped)
				}
			})
		}
	}
}

// Where no line is too long, the scanner must be bufio.Scanner exactly: the
// history, conversation and expansion readers all switched to it, and any
// difference on ordinary input would move line numbers that stored records
// and exchange ranges point at.
func TestHistoryLineScannerMatchesBufioScannerOnReadableInput(t *testing.T) {
	t.Parallel()
	const maxLine = 24
	rng := rand.New(rand.NewSource(641))
	pieces := []string{"a", "{", "}", " ", "\r", "\n", "\r\n", "\n\n"}
	for i := range 2000 {
		var b strings.Builder
		for range rng.Intn(12) {
			for range rng.Intn(maxLine * 3) {
				b.WriteString(pieces[rng.Intn(len(pieces)-3)])
			}
			b.WriteString(pieces[5+rng.Intn(3)])
		}
		if rng.Intn(2) == 0 {
			b.WriteString(strings.Repeat("t", rng.Intn(maxLine*2)))
		}
		input := b.String()
		got, _ := scanAllHistoryLines(t, iotest.HalfReader(strings.NewReader(input)), maxLine)
		if want := referenceHistoryLines(input, maxLine); !reflect.DeepEqual(got, want) {
			t.Fatalf("case %d %q: lines = %+v, want %+v", i, input, got, want)
		}
		plain := bufio.NewScanner(strings.NewReader(input))
		plain.Buffer(make([]byte, 0, maxLine), maxLine)
		var plainLines []scannedLine
		for plain.Scan() {
			plainLines = append(plainLines, scannedLine{Text: plain.Text()})
		}
		if plain.Err() != nil {
			continue // a line too long for bufio: covered by the reference above
		}
		if !reflect.DeepEqual(got, plainLines) {
			t.Fatalf("case %d %q: lines = %+v, bufio.Scanner = %+v", i, input, got, plainLines)
		}
	}
}

func TestHistoryLineScannerReportsReadErrors(t *testing.T) {
	t.Parallel()
	failing := io.MultiReader(strings.NewReader("a\n"+strings.Repeat("x", 64)), iotest.ErrReader(io.ErrUnexpectedEOF))
	scanner := newBoundedHistoryLineScanner(failing, 16)
	for scanner.Scan() {
	}
	if err := scanner.Err(); err != io.ErrUnexpectedEOF {
		t.Fatalf("err = %v, want the read error", err)
	}
}
