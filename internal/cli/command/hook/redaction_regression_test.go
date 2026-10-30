package hook

import (
	"strings"
	"testing"
)

func TestHookReadsActualFailureTail(t *testing.T) {
	const marker = "ACTUAL FAILURE: final compile error"
	input := strings.Repeat("x", 2<<20) + "\n" + marker + "\n"
	got := hookReadFailureTail(strings.NewReader(input))
	if len(got) != hookFailureTailBytes || !strings.HasSuffix(got, marker+"\n") {
		t.Fatalf("got %d bytes without expected final diagnostic", len(got))
	}
}
