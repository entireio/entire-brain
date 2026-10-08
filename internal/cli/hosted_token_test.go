package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const fakeJWT = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ1bHJpY2gifQ.c2lnbmF0dXJl"

func TestMintHostedTokenUsesTheCLIsOwnAuth(t *testing.T) {
	var gotName string
	var gotArgs []string
	runner := commandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		gotName = name
		gotArgs = args
		return []byte("some banner chatter\n" + fakeJWT + "\n"), nil, nil
	})
	token, err := mintHostedToken(context.Background(), runner, "/repo", "entire")
	if err != nil {
		t.Fatal(err)
	}
	if token != fakeJWT {
		t.Fatalf("token must be the last non-empty stdout line, got %q", token)
	}
	if gotName != "entire" {
		t.Fatalf("must shell out to the host binary, got %q", gotName)
	}
	// The CLI owns auth entirely: its stored active-context login (or
	// ENTIRE_TOKEN, which it prints verbatim). No context/jurisdiction flags.
	if strings.Join(gotArgs, " ") != "auth token" {
		t.Fatalf("args = %v, want [auth token]", gotArgs)
	}
}

func TestMintHostedTokenFailures(t *testing.T) {
	t.Run("runner error passes through", func(t *testing.T) {
		boom := errors.New("not logged in")
		runner := commandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
			return nil, nil, boom
		})
		if _, err := mintHostedToken(context.Background(), runner, "/repo", "entire"); !errors.Is(err, boom) {
			t.Fatalf("runner error must pass through, got %v", err)
		}
	})
	t.Run("empty stdout errors", func(t *testing.T) {
		runner := commandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
			return []byte("  \n"), nil, nil
		})
		if _, err := mintHostedToken(context.Background(), runner, "/repo", "entire"); err == nil {
			t.Fatal("empty stdout must error")
		}
	})
	t.Run("non-JWT output errors without echoing it", func(t *testing.T) {
		secretish := "definitely-not-a-jwt-but-still-private"
		runner := commandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
			return []byte(secretish + "\n"), nil, nil
		})
		_, err := mintHostedToken(context.Background(), runner, "/repo", "entire")
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

func TestMintHostedTokenBoundsTheShellOut(t *testing.T) {
	// A watch tick must stay a tick and a hook must return: the shell-out to
	// the host CLI carries a hard deadline, not just signal cancellation.
	runner := commandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("the auth token shell-out must carry a deadline")
		}
		return []byte(fakeJWT + "\n"), nil, nil
	})
	if _, err := mintHostedToken(context.Background(), runner, "/repo", "entire"); err != nil {
		t.Fatal(err)
	}
}
