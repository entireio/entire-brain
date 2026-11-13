package base

import (
	"os"
	"slices"
	"strings"
	"testing"
)

func TestPathOnlyExeLookupEnvLeavesNonWindowsUnchanged(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "freebsd"} {
		if got := PathOnlyExeLookupEnv(goos, nil); got != nil {
			t.Fatalf("%s: nil env must stay nil so the child inherits as before, got %q", goos, got)
		}
		env := []string{"A=1", EnvNoDefaultCurrentDirectoryInExePath + "=0"}
		got := PathOnlyExeLookupEnv(goos, env)
		if !slices.Equal(got, env) {
			t.Fatalf("%s: env changed: %q", goos, got)
		}
	}
}

func TestPathOnlyExeLookupEnvOnWindowsAppendsTheEntry(t *testing.T) {
	env := []string{"A=1", "PATH=C:\\bin"}
	got := PathOnlyExeLookupEnv("windows", env)
	want := []string{"A=1", "PATH=C:\\bin", EnvNoDefaultCurrentDirectoryInExePath + "=1"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	if len(env) != 2 {
		t.Fatalf("caller's slice was modified: %q", env)
	}
}

func TestPathOnlyExeLookupEnvOnWindowsOverridesAnInheritedValue(t *testing.T) {
	env := []string{
		EnvNoDefaultCurrentDirectoryInExePath + "=",
		"A=1",
		strings.ToUpper(EnvNoDefaultCurrentDirectoryInExePath) + "=0",
		strings.ToLower(EnvNoDefaultCurrentDirectoryInExePath) + "=yes",
	}
	original := slices.Clone(env)
	got := PathOnlyExeLookupEnv("windows", env)
	want := []string{"A=1", EnvNoDefaultCurrentDirectoryInExePath + "=1"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	if !slices.Equal(env, original) {
		t.Fatalf("caller's slice was modified: %q", env)
	}
}

func TestPathOnlyExeLookupEnvOnWindowsInheritsWhenNil(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_EXE_LOOKUP_TEST", "inherited")
	t.Setenv(EnvNoDefaultCurrentDirectoryInExePath, "0")
	got := PathOnlyExeLookupEnv("windows", nil)
	if !slices.Contains(got, "ENTIRE_BRAIN_EXE_LOOKUP_TEST=inherited") {
		t.Fatalf("parent environment not inherited: %q", got)
	}
	var values []string
	for _, entry := range got {
		key, value, _ := strings.Cut(entry, "=")
		if strings.EqualFold(key, EnvNoDefaultCurrentDirectoryInExePath) {
			values = append(values, value)
		}
	}
	if !slices.Equal(values, []string{"1"}) {
		t.Fatalf("want exactly one %s=1, got values %q", EnvNoDefaultCurrentDirectoryInExePath, values)
	}
	if len(got) != len(os.Environ()) {
		t.Fatalf("want the inherited environment with the entry replaced, got %d entries for %d inherited", len(got), len(os.Environ()))
	}
}
