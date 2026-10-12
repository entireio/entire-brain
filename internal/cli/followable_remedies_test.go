package cli

import (
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// A remedy that names a flag the command does not accept, a script the reader
// does not have, or a deletion that destroys recoverable state, is worse than
// no remedy: it costs the reader an attempt and their trust.

// --graph-binary exists only on bench and entities. Neither `setup` nor
// `refresh` accepts it, and those are the commands that reach these errors --
// verified against the built binary's own --help, which reports zero
// occurrences for both.
func TestProviderRemediesNameAnInstallThatExists(t *testing.T) {
	t.Parallel()
	// A CUSTOM binary on purpose. With the default binary and a not-found
	// detail, semanticDoctorFailureError returns early through the
	// provider-missing branch and never reaches the lines under test -- which
	// is exactly how the first version of this guard passed while broken.
	const custom = "/opt/tools/entire-graph"
	cases := []struct{ name, detail string }{
		{"not found", "exec: \"/opt/tools/entire-graph\": executable file not found in $PATH"},
		{"not executable", "fork/exec /opt/tools/entire-graph: permission denied"},
	}
	for _, c := range cases {
		err := semanticDoctorFailureError(custom, []semanticWarning{{Code: "provider_doctor_failed", Detail: c.detail}})
		if err == nil {
			t.Fatalf("%s: expected an error to inspect", c.name)
		}
		msg := err.Error()
		if !strings.Contains(msg, custom) {
			t.Fatalf("%s: not the branch under test, so this guard proves nothing: %s", c.name, msg)
		}
		if strings.Contains(msg, "--graph-binary") {
			t.Errorf("%s: names a flag neither setup nor refresh accepts: %s", c.name, msg)
		}
		if !strings.Contains(msg, "entire plugin install graph") {
			t.Errorf("%s: names no way to install the provider: %s", c.name, msg)
		}
	}
}

// A reader who installed the plugin has no entire-brain checkout and no
// sibling clone, so scripts/install.sh is not a thing they can run.
func TestTheMissingProviderMessageDoesNotSendYouToACheckout(t *testing.T) {
	t.Parallel()
	err := semanticProviderUnverifiedError("entire", []semanticWarning{{Code: "provider_missing", Detail: `unknown command "graph" for "entire"`}})
	if err == nil {
		t.Fatal("expected an error to inspect")
	}
	msg := err.Error()
	// Non-vacuity: if the provider-missing branch did not fire, none of the
	// strings below can appear and every assertion would pass on anything.
	if !strings.Contains(msg, "not installed") {
		t.Fatalf("the provider-missing branch did not fire, so this guard proves nothing: %s", msg)
	}
	for _, forbidden := range []string{"scripts/install.sh", "sibling clone", "entire-brain checkout"} {
		if strings.Contains(msg, forbidden) {
			t.Errorf("remedy assumes a source checkout the reader may not have (%q): %s", forbidden, msg)
		}
	}
	if !strings.Contains(msg, "entire plugin install graph") {
		t.Errorf("remedy names no installable provider: %s", msg)
	}
}

// Deleting the setup record loses this repo's daemon identity, and the call
// site's own comment says why that is dangerous: every path below acts on a
// PREVIOUS daemon identity, so a lost record can retire a watcher belonging to
// another repo. That advice belongs only to a record that cannot be PARSED.
func TestOnlyACorruptRecordIsWorthDeleting(t *testing.T) {
	t.Parallel()

	transient := []error{
		fs.ErrPermission,
		errors.New("input/output error"),
	}
	for _, err := range transient {
		if setupRecordIsCorrupt(err) {
			t.Errorf("a transient read was classed corrupt, so the reader is told to delete recoverable state: %v", err)
		}
		msg := setupRecordRemedy(err, "/state", "entire brain")
		// Joined, not a literal slash. As a NEGATIVE assertion a hardcoded "/"
		// would not fail on Windows — it would simply stop matching, and the
		// guard would quietly pass whatever the code did.
		if strings.Contains(msg, "delete "+filepath.Join("/state", setupRecordFile)) {
			t.Errorf("transient failure advises deletion: %s", msg)
		}
		if !strings.Contains(msg, "re-run") {
			t.Errorf("transient failure offers no next step: %s", msg)
		}
	}

	corrupt := []error{
		&json.SyntaxError{},
		errors.New(`json: unknown field "whatever"`),
		errors.New("invalid character 'x' looking for beginning of value"),
	}
	for _, err := range corrupt {
		if !setupRecordIsCorrupt(err) {
			t.Errorf("an unparseable record was classed transient, so the reader is told to retry for ever: %v", err)
		}
		if msg := setupRecordRemedy(err, "/state", "entire brain"); !strings.Contains(msg, "delete") {
			t.Errorf("an unparseable record must say it can be deleted: %s", msg)
		}
	}
}

// Both branches must name the file. "the setup record" is not a path.
func TestTheRecordRemedyNamesTheFile(t *testing.T) {
	t.Parallel()
	for _, err := range []error{fs.ErrPermission, errors.New(`json: unknown field "x"`)} {
		// filepath.Join, not a literal "/": the production remedy joins the path,
		// so a hardcoded forward slash passes on Unix and fails on Windows.
		want := filepath.Join("/state", setupRecordFile)
		if msg := setupRecordRemedy(err, "/state", "entire brain"); !strings.Contains(msg, want) {
			t.Errorf("remedy does not name %s: %s", want, msg)
		}
	}
}
