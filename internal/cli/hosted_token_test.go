package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/clitest"
)

const fakeJWT = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ1bHJpY2gifQ.c2lnbmF0dXJl"

func TestMintHostedTokenUsesTheCLIsOwnAuth(t *testing.T) {
	var gotName string
	var gotArgs []string
	runner := clitest.CommandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
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
		runner := clitest.CommandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
			return nil, nil, boom
		})
		if _, err := mintHostedToken(context.Background(), runner, "/repo", "entire"); !errors.Is(err, boom) {
			t.Fatalf("runner error must pass through, got %v", err)
		}
	})
	t.Run("empty stdout errors", func(t *testing.T) {
		runner := clitest.CommandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
			return []byte("  \n"), nil, nil
		})
		if _, err := mintHostedToken(context.Background(), runner, "/repo", "entire"); err == nil {
			t.Fatal("empty stdout must error")
		}
	})
	t.Run("non-JWT output errors without echoing it", func(t *testing.T) {
		secretish := "definitely-not-a-jwt-but-still-private"
		runner := clitest.CommandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
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

// jwtWithPayload builds an unsigned JWT-shaped token with the given payload
// JSON, matching the base64url (unpadded) encoding real tokens use.
func jwtWithPayload(payload string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"RS256"}`)) + "." + enc([]byte(payload)) + ".c2lnbmF0dXJl"
}

func TestMintHostedTokenRejectsExpiredTokens(t *testing.T) {
	mint := func(t *testing.T, token string) (string, error) {
		t.Helper()
		runner := clitest.CommandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
			return []byte(token + "\n"), nil, nil
		})
		return mintHostedToken(context.Background(), runner, "/repo", "entire")
	}

	t.Run("expired exp errors with the re-login hint, without echoing the token", func(t *testing.T) {
		token := jwtWithPayload(fmt.Sprintf(`{"sub":"ulrich","exp":%d}`, time.Now().Add(-time.Hour).Unix()))
		_, err := mint(t, token)
		if err == nil {
			t.Fatal("an expired token must error instead of being handed to brainstore")
		}
		if !strings.Contains(err.Error(), "entire auth login") {
			t.Fatalf("error must point at re-login, got: %v", err)
		}
		if strings.Contains(err.Error(), token[strings.Index(token, ".")+1:]) {
			t.Fatalf("error must not echo the token: %v", err)
		}
	})
	t.Run("exp just past is within clock-skew leeway", func(t *testing.T) {
		token := jwtWithPayload(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(-10*time.Second).Unix()))
		if _, err := mint(t, token); err != nil {
			t.Fatalf("a token within the skew leeway must pass through: %v", err)
		}
	})
	t.Run("future exp passes through", func(t *testing.T) {
		token := jwtWithPayload(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(time.Hour).Unix()))
		if got, err := mint(t, token); err != nil || got != token {
			t.Fatalf("got %q, %v", got, err)
		}
	})
	t.Run("missing exp fails open", func(t *testing.T) {
		token := jwtWithPayload(`{"sub":"ulrich"}`)
		if _, err := mint(t, token); err != nil {
			t.Fatalf("a token without exp must pass through (server stays the authority): %v", err)
		}
	})
	t.Run("undecodable payload fails open", func(t *testing.T) {
		token := "eyJhbGciOiJSUzI1NiJ9.%%%notbase64%%%.c2lnbmF0dXJl"
		if _, err := mint(t, token); err != nil {
			t.Fatalf("an undecodable payload must pass through unjudged: %v", err)
		}
	})
}

func TestMintHostedTokenBoundsTheShellOut(t *testing.T) {
	// A watch tick must stay a tick and a hook must return: the shell-out to
	// the host CLI carries a hard deadline, not just signal cancellation.
	runner := clitest.CommandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("the auth token shell-out must carry a deadline")
		}
		return []byte(fakeJWT + "\n"), nil, nil
	})
	if _, err := mintHostedToken(context.Background(), runner, "/repo", "entire"); err != nil {
		t.Fatal(err)
	}
}
