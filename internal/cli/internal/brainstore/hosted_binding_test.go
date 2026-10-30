package brainstore

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHostedRepoBindingRoundTrip(t *testing.T) {
	brainDir := t.TempDir()
	in := HostedRepoBinding{RepoID: "01M2Q9WATHXVM46CD8Y4D3AX8W", BaseURL: "https://cell.api.example.com/api/v1/"}
	if err := WriteHostedRepoBinding(brainDir, in); err != nil {
		t.Fatal(err)
	}
	got, present, err := ReadHostedRepoBinding(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if !present {
		t.Fatal("binding must be present after write")
	}
	if got.RepoID != in.RepoID {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.BaseURL != "https://cell.api.example.com/api/v1" {
		t.Fatalf("base URL must be stored without trailing slash, got %q", got.BaseURL)
	}
}

func TestHostedRepoBindingOptOutRoundTrip(t *testing.T) {
	brainDir := t.TempDir()
	in := HostedRepoBinding{RepoID: "repo1", BaseURL: "https://cell.api.example.com", OptedOut: true}
	if err := WriteHostedRepoBinding(brainDir, in); err != nil {
		t.Fatal(err)
	}
	got, present, err := ReadHostedRepoBinding(brainDir)
	if err != nil || !present {
		t.Fatalf("opted-out binding must still be present: present=%v err=%v", present, err)
	}
	if !got.OptedOut {
		t.Fatal("opt-out must survive the round trip")
	}
	if got.RepoID != in.RepoID || got.BaseURL != in.BaseURL {
		t.Fatalf("opt-out must keep the coordinates for easy re-connect, got %+v", got)
	}
}

func TestHostedRepoBindingAbsent(t *testing.T) {
	_, present, err := ReadHostedRepoBinding(t.TempDir())
	if err != nil {
		t.Fatalf("absent binding must not be an error, got %v", err)
	}
	if present {
		t.Fatal("absent binding must report present=false")
	}
}

func TestHostedRepoBindingCorrupt(t *testing.T) {
	brainDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(brainDir, "hosted.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadHostedRepoBinding(brainDir); err == nil {
		t.Fatal("corrupt binding must return an error")
	}
}

func TestMutateHostedRepoBinding(t *testing.T) {
	t.Run("applies the update under the lock", func(t *testing.T) {
		brainDir := t.TempDir()
		until := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
		if err := WriteHostedRepoBinding(brainDir, HostedRepoBinding{RepoID: "repo1", BaseURL: "https://cell.example"}); err != nil {
			t.Fatal(err)
		}
		if err := MutateHostedRepoBinding(brainDir, func(b HostedRepoBinding) (HostedRepoBinding, bool) {
			b.NoAccessUntil = until
			return b, true
		}); err != nil {
			t.Fatal(err)
		}
		got, _, err := ReadHostedRepoBinding(brainDir)
		if err != nil {
			t.Fatal(err)
		}
		if !got.NoAccessUntil.Equal(until) || got.RepoID != "repo1" {
			t.Fatalf("mutate round trip = %+v", got)
		}
	})
	t.Run("declining the write leaves the file byte-identical", func(t *testing.T) {
		brainDir := t.TempDir()
		if err := WriteHostedRepoBinding(brainDir, HostedRepoBinding{RepoID: "repo1", BaseURL: "https://cell.example"}); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(filepath.Join(brainDir, HostedRepoBindingFile))
		if err != nil {
			t.Fatal(err)
		}
		if err := MutateHostedRepoBinding(brainDir, func(b HostedRepoBinding) (HostedRepoBinding, bool) {
			return HostedRepoBinding{}, false
		}); err != nil {
			t.Fatal(err)
		}
		after, err := os.ReadFile(filepath.Join(brainDir, HostedRepoBindingFile))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatal("a declined mutate must not rewrite the binding")
		}
	})
	t.Run("never creates an absent binding", func(t *testing.T) {
		brainDir := t.TempDir()
		if err := MutateHostedRepoBinding(brainDir, func(b HostedRepoBinding) (HostedRepoBinding, bool) {
			t.Error("update must not run without a binding")
			return b, true
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(brainDir, HostedRepoBindingFile)); !os.IsNotExist(err) {
			t.Fatalf("mutate must not create a binding, stat err = %v", err)
		}
	})
	t.Run("never repairs an unreadable binding", func(t *testing.T) {
		brainDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(brainDir, HostedRepoBindingFile), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := MutateHostedRepoBinding(brainDir, func(b HostedRepoBinding) (HostedRepoBinding, bool) {
			t.Error("update must not run on a corrupt binding")
			return b, true
		}); err == nil {
			t.Fatal("a corrupt binding must surface its read error")
		}
		got, err := os.ReadFile(filepath.Join(brainDir, HostedRepoBindingFile))
		if err != nil || string(got) != "{not json" {
			t.Fatalf("mutate must leave a corrupt binding untouched: %q err=%v", got, err)
		}
	})
}
