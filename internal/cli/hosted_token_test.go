package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const fakeJWT = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ1bHJpY2gifQ.c2lnbmF0dXJl"

func TestMintJurisdictionTokenTakesLastLineAndUsesContextFlag(t *testing.T) {
	var gotName string
	var gotArgs []string
	runner := commandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		gotName = name
		gotArgs = args
		return []byte("some banner chatter\n" + fakeJWT + "\n"), nil, nil
	})
	token, err := mintJurisdictionToken(context.Background(), runner, "/repo", "entire", "us")
	if err != nil {
		t.Fatal(err)
	}
	if token != fakeJWT {
		t.Fatalf("token must be the last non-empty stdout line, got %q", token)
	}
	if gotName != "entire" {
		t.Fatalf("must shell out to the host binary, got %q", gotName)
	}
	want := []string{"auth", "token", "--context", "us"}
	if strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Fatalf("args = %v, want %v (current CLI form; --jurisdiction is deprecated)", gotArgs, want)
	}
}

func TestMintJurisdictionTokenOmitsContextWhenEmpty(t *testing.T) {
	var gotArgs []string
	runner := commandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		gotArgs = args
		return []byte(fakeJWT + "\n"), nil, nil
	})
	if _, err := mintJurisdictionToken(context.Background(), runner, "/repo", "entire", ""); err != nil {
		t.Fatal(err)
	}
	if strings.Join(gotArgs, " ") != "auth token" {
		t.Fatalf("empty jurisdiction must omit --context, got %v", gotArgs)
	}
}

func TestMintJurisdictionTokenFailures(t *testing.T) {
	t.Run("runner error passes through", func(t *testing.T) {
		boom := errors.New("not logged in")
		runner := commandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
			return nil, nil, boom
		})
		if _, err := mintJurisdictionToken(context.Background(), runner, "/repo", "entire", "us"); !errors.Is(err, boom) {
			t.Fatalf("runner error must pass through, got %v", err)
		}
	})
	t.Run("empty stdout errors", func(t *testing.T) {
		runner := commandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
			return []byte("  \n"), nil, nil
		})
		if _, err := mintJurisdictionToken(context.Background(), runner, "/repo", "entire", "us"); err == nil {
			t.Fatal("empty stdout must error")
		}
	})
	t.Run("non-JWT output errors without echoing it", func(t *testing.T) {
		secretish := "definitely-not-a-jwt-but-still-private"
		runner := commandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
			return []byte(secretish + "\n"), nil, nil
		})
		_, err := mintJurisdictionToken(context.Background(), runner, "/repo", "entire", "us")
		if err == nil {
			t.Fatal("non-JWT output must error")
		}
		if strings.Contains(err.Error(), secretish) {
			t.Fatalf("error must not echo command output: %v", err)
		}
		if !strings.Contains(err.Error(), "entire auth token") {
			t.Fatalf("error must name the command, got: %v", err)
		}
	})
}
