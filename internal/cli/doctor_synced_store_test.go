package cli

import (
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
