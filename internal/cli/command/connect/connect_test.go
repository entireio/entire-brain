package connect

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/clitest"
)

func TestPlacementFailureErrorWithoutStderrNamesTheCommand(t *testing.T) {
	err := placementFailureError(fmt.Errorf("entire [repo view --json]: %w", errors.New("exit status 1")), nil)
	if got, want := err.Error(), "`entire repo view --json`: exit status 1"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	bannerOnly := []byte("Using context 'auth.example.invalid'.\n")
	err = placementFailureError(errors.New("exit status 1"), bannerOnly)
	if got, want := err.Error(), "`entire repo view --json`: exit status 1"; got != want {
		t.Fatalf("banner-only stderr must fall back to the command, got %q, want %q", got, want)
	}
}

func TestResolveHostedPlacementBoundsTheShellOut(t *testing.T) {
	runner := clitest.CommandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("the placement shell-out must carry a deadline")
		}
		return []byte(`{"repoId":"r","apiUrl":"https://cell.api.example.com"}`), nil, nil
	})
	if _, err := resolveHostedPlacement(context.Background(), runner, "/repo"); err != nil {
		t.Fatal(err)
	}
}

func TestCheckHostCLIVersion(t *testing.T) {
	runnerReporting := func(version string) base.CommandRunner {
		return clitest.CommandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
			return []byte(version), nil, nil
		})
	}
	cases := []struct {
		name    string
		runner  base.CommandRunner
		min     string
		wantErr bool
	}{
		{"too old is refused", runnerReporting("Entire CLI 0.11.3\nGo version: go1.27.1\n"), "0.12.0", true},
		{"too old with commit suffix is refused", runnerReporting("Entire CLI 0.11.3 (abc1234)\n"), "0.12.0", true},
		{"new enough with commit suffix passes", runnerReporting("Entire CLI 0.12.0 (abc1234)\n"), "0.12.0", false},
		{"prerelease of the minimum passes", runnerReporting("Entire CLI 0.12.0-nightly.20261020\n"), "0.12.0", false},
		{"pseudo-version above the minimum passes", runnerReporting("Entire CLI v0.12.1-0.20261020123456-abcdef123456\n"), "0.12.0", false},
		{"prerelease of an older release is refused", runnerReporting("Entire CLI 0.11.3-nightly.1\n"), "0.12.0", true},
		{"unset minimum disables the gate", runnerReporting("Entire CLI 0.11.3\n"), "", false},
		{"minimum passes", runnerReporting("Entire CLI 0.12.0\n"), "0.12.0", false},
		{"newer passes", runnerReporting("Entire CLI 0.13.1\n"), "0.12.0", false},
		{"dev build passes", runnerReporting("Entire CLI dev\n"), "0.12.0", false},
		{"missing CLI passes", clitest.CommandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
			return nil, nil, errors.New("not found")
		}), "0.12.0", false},
	}
	for _, tc := range cases {
		err := checkHostCLIVersion(context.Background(), tc.runner, "/repo", tc.min)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: got err=%v, want error=%v", tc.name, err, tc.wantErr)
		}
	}
}

func TestResolveHostedPlacementRefusesAnOldHostCLI(t *testing.T) {
	prev := minHostCLIVersion
	minHostCLIVersion = "0.12.0"
	t.Cleanup(func() { minHostCLIVersion = prev })
	repoViewCalled := false
	runner := clitest.CommandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		if len(args) > 0 && args[0] == "version" {
			return []byte("Entire CLI 0.11.3\n"), nil, nil
		}
		repoViewCalled = true
		return []byte(`{"repoId":"r","apiUrl":"https://cell.api.example.com"}`), nil, nil
	})
	if _, err := resolveHostedPlacement(context.Background(), runner, "/repo"); err == nil {
		t.Fatal("placement on a host CLI older than minHostCLIVersion must fail with upgrade guidance")
	} else if !strings.Contains(err.Error(), "upgrade") {
		t.Fatalf("error must tell the user to upgrade, got %v", err)
	}
	if repoViewCalled {
		t.Fatal("repo view must not run once the version gate refused")
	}
}
