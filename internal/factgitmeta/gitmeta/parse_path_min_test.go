package gitmeta

import "testing"

func TestParseTargetPreservesPathMinimum(t *testing.T) {
	for _, value := range []string{"", "a", "ab"} {
		if _, err := ParseTarget("path:" + value); err == nil {
			t.Errorf("accepted short path %q", value)
		}
	}
	for _, value := range []string{"abc", "a/b"} {
		got, err := ParseTarget("path:" + value)
		if err != nil || got.Value != value {
			t.Errorf("path %q: %#v, %v", value, got, err)
		}
	}
}
