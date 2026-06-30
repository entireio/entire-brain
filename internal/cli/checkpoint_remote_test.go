package cli

import (
	"strings"
	"testing"
)

func settingsWithRepo(repo string) entireSettingsFile {
	return entireSettingsFile{
		StrategyOptions: entireStrategyOptions{
			CheckpointRemote: &checkpointRemoteSettings{Provider: "github", Repo: repo},
		},
	}
}

func TestCheckpointRemoteFetchURLValidatesRepo(t *testing.T) {
	good := map[string]string{
		"owner/name":     "https://github.com/owner/name.git",
		"/owner/name/":   "https://github.com/owner/name.git",
		"My-Org/repo.js": "https://github.com/My-Org/repo.js.git",
		"a_b/c.d-e":      "https://github.com/a_b/c.d-e.git",
	}
	for repo, want := range good {
		got, err := settingsWithRepo(repo).CheckpointRemoteFetchURL()
		if err != nil || got != want {
			t.Errorf("repo %q => (%q, %v), want %q", repo, got, err, want)
		}
	}

	bad := []string{
		"../evil",
		"owner/../../etc",
		"owner/name/extra",
		"owner",
		"owner/na me",
		"owner/name?query=1",
		"owner/name#frag",
		"@host/owner/name",
		"owner/..",
		"owner/.",
		"https://evil.com/owner/name",
	}
	for _, repo := range bad {
		if got, err := settingsWithRepo(repo).CheckpointRemoteFetchURL(); err == nil {
			t.Errorf("repo %q should be rejected, got url %q", repo, got)
		} else if !strings.Contains(err.Error(), "invalid checkpoint_remote repo") {
			t.Errorf("repo %q error = %v, want invalid-repo error", repo, err)
		}
	}
}
