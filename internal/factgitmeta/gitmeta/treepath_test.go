// Copied verbatim from github.com/entirehq/git-meta-service/internal/gitmeta @
// feat/git-meta-service-integration (f3ec153); do not edit; de-internalize
// upstream to dedupe (follow-up).

package gitmeta

import "testing"

func mustTarget(t *testing.T, s string) Target {
	t.Helper()
	tg, err := ParseTarget(s)
	if err != nil {
		t.Fatalf("ParseTarget(%q): %v", s, err)
	}
	return tg
}

func TestTreeBasePathCommit(t *testing.T) {
	got := TreeBasePath(mustTarget(t, "commit:13a7d29cde8f8557b54fd6474f547a56822180ae"))
	want := "commit/13/13a7d29cde8f8557b54fd6474f547a56822180ae"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestTreeBasePathProject(t *testing.T) {
	if got := TreeBasePath(mustTarget(t, "project")); got != "project" {
		t.Fatalf("got %q want project", got)
	}
}

func TestTreePathString(t *testing.T) {
	got, err := TreePath(mustTarget(t, "commit:13a7d29cde8f8557b54fd6474f547a56822180ae"), "agent:model")
	if err != nil {
		t.Fatal(err)
	}
	want := "commit/13/13a7d29cde8f8557b54fd6474f547a56822180ae/agent/model/__value"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestKeyToPathSegments(t *testing.T) {
	segs := KeyToPathSegments("agent:model:version")
	if len(segs) != 3 || segs[0] != "agent" || segs[1] != "model" || segs[2] != "version" {
		t.Fatalf("got %v", segs)
	}
}

func TestTreeBasePathBranchShards(t *testing.T) {
	// branch shards on first 2 hex of sha1(value).
	got := TreeBasePath(mustTarget(t, "branch:sc-branch-1-deadbeef"))
	want := "branch/" + sha1Hex("sc-branch-1-deadbeef")[:2] + "/sc-branch-1-deadbeef"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestTreeBasePathPathRawSegments(t *testing.T) {
	got := TreeBasePath(mustTarget(t, "path:src/main.rs"))
	if got != "path/src/main.rs/__target__" {
		t.Fatalf("got %q", got)
	}
}

func TestTreeBasePathPathEscapesReserved(t *testing.T) {
	got := TreeBasePath(mustTarget(t, "path:src/__generated/file.rs"))
	if got != "path/src/~__generated/file.rs/__target__" {
		t.Fatalf("got %q", got)
	}
}

func TestListDirPath(t *testing.T) {
	got, err := ListDirPath(mustTarget(t, "commit:13a7d29cde8f8557b54fd6474f547a56822180ae"), "agent:chat")
	if err != nil {
		t.Fatal(err)
	}
	want := "commit/13/13a7d29cde8f8557b54fd6474f547a56822180ae/agent/chat/__list"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestTombstonePath(t *testing.T) {
	got, err := TombstonePath(mustTarget(t, "commit:13a7d29cde8f8557b54fd6474f547a56822180ae"), "agent:chat")
	if err != nil {
		t.Fatal(err)
	}
	want := "commit/13/13a7d29cde8f8557b54fd6474f547a56822180ae/__tombstones/agent/chat/__deleted"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestListEntryTombstonePath(t *testing.T) {
	got, err := ListEntryTombstonePath(mustTarget(t, "commit:13a7d29cde8f8557b54fd6474f547a56822180ae"), "agent:chat", "1771232450203-23c0f")
	if err != nil {
		t.Fatal(err)
	}
	want := "commit/13/13a7d29cde8f8557b54fd6474f547a56822180ae/agent/chat/__list/__tombstones/1771232450203-23c0f"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestValidateKey(t *testing.T) {
	ok := []string{"agent:model:version", "owner", "review:status"}
	bad := []string{"", "agent:__value", "__list:chat", "agent:/model", "agent::model", "agent:.", "agent:.."}
	for _, k := range ok {
		if err := ValidateKey(k); err != nil {
			t.Errorf("ValidateKey(%q) unexpected err: %v", k, err)
		}
	}
	for _, k := range bad {
		if err := ValidateKey(k); err == nil {
			t.Errorf("ValidateKey(%q) expected err", k)
		}
	}
}

func TestParseTargetShortValueRejected(t *testing.T) {
	if _, err := ParseTarget("commit:ab"); err == nil {
		t.Fatal("expected error for short value")
	}
	if _, err := ParseTarget("unknown:abc123"); err == nil {
		t.Fatal("expected error for unknown type")
	}
}

func TestDecodePathTargetSegments(t *testing.T) {
	if got := decodePathTargetSegments([]string{"src", "~__generated", "file.rs"}); got != "src/__generated/file.rs" {
		t.Fatalf("got %q", got)
	}
}
