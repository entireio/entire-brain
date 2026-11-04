package releasecheck

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Keep fragments separate so the checker does not match its own rule definitions.
var privateIdentifierFragments = [][]string{
	{"/Users/", "thomi"}, {"/Users/", "suhaan"}, {"/Users/", "georgf"},
	{"Users-", "thomi"}, {"suhaan", "thayyil"}, {"Stefan ", "Haubold"},
	{"[redacted-name] ", "Dohmke"}, {"peyton", "montei"}, {"dvy", "dra"},
	{"/Users/", "Victor"}, {`\Users\`, "Victor"}, {"/private/var/", "folders/"},
	{"eu-staging", ".api.entire.io"}, {"aws-us-", "east-2", ".api.entire.io"},
	{"aws-eu-", "central-1", ".api.entire.io"}, {"aws-eu-", "west-1", ".api.entire.io"},
	{"claude/diff-less-", "brain"}, {"review-", "cutover"},
}

var privateIdentifierPatterns = []*regexp.Regexp{
	regexp.MustCompile(strings.Join([]string{`\baws-(?:us|eu|ap)-`, `[a-z0-9-]+\b`}, "")),
	regexp.MustCompile(strings.Join([]string{`\bcell-(?:us|eu|ap)-`, `[a-z0-9-]+\.entire\.io\b`}, "")),
	regexp.MustCompile(strings.Join([]string{`\bcore[.-](?:us|eu)`, `\.entire\.io\b`}, "")),
	regexp.MustCompile(strings.Join([]string{`\b(?:us|eu)\.auth`, `\.entire\.io\b`}, "")),
}

func containsPrivateIdentifier(data []byte) bool {
	data = bytes.ToLower(data)
	for _, parts := range privateIdentifierFragments {
		if bytes.Contains(data, []byte(strings.ToLower(strings.Join(parts, "")))) {
			return true
		}
	}
	for _, pattern := range privateIdentifierPatterns {
		if pattern.Match(data) {
			return true
		}
	}
	return false
}

func TestRepositoryPublicationSafety(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	cmd.Dir = root
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range strings.Split(string(output), "\x00") {
		if name == "" {
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(name))
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue // Unstaged deletion in a development checkout.
		}
		if err != nil {
			t.Fatal(err)
		}
		var data []byte
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				t.Fatal(err)
			}
			data = []byte(target)
		} else if info.Mode().IsRegular() {
			data, err = os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
		} else {
			continue
		}
		if containsPrivateIdentifier(data) {
			t.Errorf("%s contains an audited private identifier", name)
		}
	}
}

func TestPublicationRulesRejectIdentifiersAndAllowPublicExamples(t *testing.T) {
	for _, parts := range privateIdentifierFragments {
		if !containsPrivateIdentifier([]byte(strings.ToUpper(strings.Join(parts, "")))) {
			t.Error("missed literal identifier rule")
		}
	}
	for _, parts := range [][]string{
		{"aws-ap-", "southeast-99"}, {"cell-us-", "west-99.entire.io"},
		{"core.", "eu.entire.io"}, {"core-", "us.entire.io"}, {"us.auth", ".entire.io"},
	} {
		if !containsPrivateIdentifier([]byte(strings.Join(parts, ""))) {
			t.Error("missed endpoint pattern")
		}
	}
	if containsPrivateIdentifier([]byte("/home/example/project https://example.com https://entire.io")) {
		t.Fatal("rejected public examples")
	}
}
