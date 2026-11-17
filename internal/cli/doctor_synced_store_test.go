package cli

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

func env(pairs map[string]string) func(string) string {
	return func(name string) string { return pairs[name] }
}

func TestSyncedStoreDirDetailFlagsOneDriveOnWindows(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		dataDir string
		vars    map[string]string
	}{
		{
			name:    "personal account",
			dataDir: `C:\Users\sam\OneDrive\AppData\Local\entire\plugins\data\brain`,
			vars:    map[string]string{"OneDrive": `C:\Users\sam\OneDrive`},
		},
		{
			name:    "work account",
			dataDir: `C:\Users\sam\OneDrive - Contoso\AppData\Local\entire`,
			vars:    map[string]string{"OneDriveCommercial": `C:\Users\sam\OneDrive - Contoso`},
		},
		{
			// The resolved store path and the exported variable need not agree on
			// casing, and NTFS does not care either.
			name:    "casing differs from the exported variable",
			dataDir: `c:\users\sam\onedrive\appdata\local\entire`,
			vars:    map[string]string{"OneDrive": `C:\Users\sam\OneDrive`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			detail := syncedStoreDirDetail(windowsDoctorGOOS, tc.dataDir, env(tc.vars))
			if detail == "" {
				t.Fatal("want a warning, got none")
			}
			if !strings.Contains(detail, "OneDrive") {
				t.Fatalf("warning should name OneDrive, got: %s", detail)
			}
		})
	}
}

// The reason pathIsUnder compares components instead of using HasPrefix: a
// sibling whose name merely starts with the OneDrive root is not inside it, and
// warning about it would be a false alarm on a perfectly good store.
func TestSyncedStoreDirDetailIgnoresSiblingsOfTheOneDriveRoot(t *testing.T) {
	t.Parallel()

	detail := syncedStoreDirDetail(
		windowsDoctorGOOS,
		`C:\Users\sam\OneDrive-Archive\entire\plugins\data\brain`,
		env(map[string]string{"OneDrive": `C:\Users\sam\OneDrive`}),
	)
	if detail != "" {
		t.Fatalf("OneDrive-Archive is a sibling, not a child; got warning: %s", detail)
	}
}

func TestSyncedStoreDirDetailIgnoresAHealthyLocalStore(t *testing.T) {
	t.Parallel()

	detail := syncedStoreDirDetail(
		windowsDoctorGOOS,
		`C:\Users\sam\AppData\Local\entire\plugins\data\brain`,
		env(map[string]string{"OneDrive": `C:\Users\sam\OneDrive`}),
	)
	if detail != "" {
		t.Fatalf("a plain LOCALAPPDATA store is fine; got warning: %s", detail)
	}
}

func TestSyncedStoreDirDetailIgnoresOneDriveVarsOffWindows(t *testing.T) {
	t.Parallel()

	// The variable can exist on a Mac with OneDrive installed; the Windows
	// profile-redirection hazard this check describes does not apply there, and
	// the rest of the message would be wrong.
	detail := syncedStoreDirDetail(
		"darwin",
		"/Users/sam/OneDrive/entire/plugins/data/brain",
		env(map[string]string{"OneDrive": "/Users/sam/OneDrive"}),
	)
	if detail != "" {
		t.Fatalf("OneDrive detection is Windows-scoped; got warning: %s", detail)
	}
}

// UNC is checked before the OS branch: a network store is unsafe for WAL
// whoever mounted it.
func TestSyncedStoreDirDetailFlagsUNCOnAnyOS(t *testing.T) {
	t.Parallel()

	for _, goos := range []string{windowsDoctorGOOS, "linux", "darwin"} {
		detail := syncedStoreDirDetail(goos, `\\fileserver\profiles\sam\entire`, env(nil))
		if detail == "" {
			t.Fatalf("goos %s: want a UNC warning, got none", goos)
		}
		if !strings.Contains(detail, "network share") {
			t.Fatalf("goos %s: warning should name the network share, got: %s", goos, detail)
		}
	}
}

func TestSyncedStoreDirDetailHandlesAnEmptyDataDir(t *testing.T) {
	t.Parallel()

	if detail := syncedStoreDirDetail(windowsDoctorGOOS, "", env(map[string]string{"OneDrive": `C:\x`})); detail != "" {
		t.Fatalf("empty data dir should produce no warning, got: %s", detail)
	}
}

// Every case above is written with Windows separators, and every one of them
// passed on darwin while the subject could not warn on Windows at all. That is
// not a gap in the cases -- it is the comparison itself having been
// OS-dependent. These two tests pin the property that makes the cases above
// mean what they say.

func TestSyncedStoreComparisonIsOSIndependent(t *testing.T) {
	t.Parallel()

	// Mixed separators, because a path can reach doctor from an environment
	// variable in either form, and the two must compare equal on every OS.
	cases := []struct {
		name      string
		candidate string
		root      string
		want      bool
	}{
		{"both backslash", `C:\Users\sam\OneDrive\brain`, `C:\Users\sam\OneDrive`, true},
		{"both slash", "C:/Users/sam/OneDrive/brain", "C:/Users/sam/OneDrive", true},
		{"slash candidate, backslash root", "C:/Users/sam/OneDrive/brain", `C:\Users\sam\OneDrive`, true},
		{"backslash candidate, slash root", `C:\Users\sam\OneDrive\brain`, "C:/Users/sam/OneDrive", true},
		{"trailing separator on the root", `C:\Users\sam\OneDrive\brain`, `C:\Users\sam\OneDrive\`, true},
		{"exactly the root", `C:\Users\sam\OneDrive`, `C:\Users\sam\OneDrive`, true},
		{"sibling sharing a prefix", `C:\Users\sam\OneDrive-Archive\brain`, `C:\Users\sam\OneDrive`, false},
		{"unrelated path", `D:\brain`, `C:\Users\sam\OneDrive`, false},
		{"empty root", `C:\Users\sam\OneDrive\brain`, "", false},
		{"empty candidate", "", `C:\Users\sam\OneDrive`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := pathIsUnder(tc.candidate, tc.root); got != tc.want {
				t.Fatalf("pathIsUnder(%q, %q) = %v, want %v", tc.candidate, tc.root, got, tc.want)
			}
		})
	}

	// The mechanism, stated directly. filepath.Clean would reintroduce "\" here
	// on Windows and leave it alone on darwin, which is exactly how the original
	// defect passed CI on three platforms and failed on the fourth.
	for _, in := range []string{`C:\Users\sam\OneDrive`, "C:/Users/sam/OneDrive", `C:\Users\sam\OneDrive\`} {
		if got := normalizePathForCompare(in); strings.Contains(got, `\`) {
			t.Fatalf("normalizePathForCompare(%q) = %q, want a slash-separated form on every OS", in, got)
		}
	}
}

// The comment on pathIsUnder says this file must not use path/filepath. A
// comment does not survive the next edit; this does. It is the cheapest
// available guard against re-introducing an OS-dependent comparison in a file
// whose whole purpose is a Windows-only code path that darwin CI cannot reach.
func TestSyncedStoreSourceAvoidsFilepath(t *testing.T) {
	t.Parallel()

	const file = "doctor_synced_store.go"
	parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	for _, spec := range parsed.Imports {
		if spec.Path.Value == `"path/filepath"` {
			t.Fatalf("%s imports path/filepath; its separator handling is OS-dependent, "+
				"which made the OneDrive warning unreachable on Windows. Use package path.", file)
		}
	}
}
