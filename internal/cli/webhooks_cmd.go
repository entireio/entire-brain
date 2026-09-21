package cli

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// `entire brain webhook` — the surface for setting one up and proving it works.
//
// The failure people hit with webhooks is silence: nothing arrives and there is
// no way to tell whether the endpoint is wrong, the variable is unset, the
// scheme is unsupported, or nothing has happened yet. `status` answers the
// first three without waiting for an event, and `test` answers the fourth by
// sending one on demand — which is also how you check a signature verifier
// before trusting it with real events.

func newWebhookCommand(opts Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "webhook",
		Short: "Notify another service when this brain changes",
		Long: strings.TrimSpace(`
Post a small JSON event to an HTTP endpoint when a fact is recorded or retracted
or the brain is refreshed, so a CI job, a chat channel or an index can react.

Set ` + webhookURLEnv + ` to switch it on; unset, nothing is sent and no network
call is made. ` + webhookSecretEnv + ` adds an HMAC-SHA256 signature in the
X-Entire-Signature-256 header so a receiver can verify the POST came from this
brain.

Fact text is not sent. Events carry the fact id, kind and taxonomy paths; set
` + webhookIncludeTextEnv + `=1 to include the text as well.

ENTIRE_BRAIN_NO_EGRESS and ENTIRE_BRAIN_LOCAL_ONLY disable webhooks outright,
whatever else is configured.`),
	}
	cmd.AddCommand(newWebhookStatusCommand(opts), newWebhookTestCommand(opts), newWebhookEventsCommand())
	return cmd
}

func newWebhookStatusCommand(opts Options) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show whether webhooks are on, and why not when they are off",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			endpoint, enabled, reason := webhookEndpoint()
			signed := strings.TrimSpace(os.Getenv(webhookSecretEnv)) != ""
			includeText := envBool(webhookIncludeTextEnv)
			if jsonOut {
				return writeJSON(cmd, map[string]any{
					"enabled":      enabled,
					"endpoint":     endpoint,
					"reason":       reason,
					"signed":       signed,
					"include_text": includeText,
					"events":       webhookEventNames(),
				})
			}
			out := cmd.OutOrStdout()
			if !enabled {
				fmt.Fprintf(out, "webhooks off: %s\n", reason)
				// The hint has to match the reason. Telling somebody to set a
				// variable they have already set — because an egress toggle is
				// what actually silenced this — sends them to fix the wrong thing.
				switch {
				case brainNoEgressMode():
					fmt.Fprintf(out, "  unset ENTIRE_BRAIN_NO_EGRESS/ENTIRE_BRAIN_LOCAL_ONLY to allow them\n")
				case strings.TrimSpace(os.Getenv(webhookURLEnv)) != "":
					fmt.Fprintf(out, "  %s is set to %q; correct it and try again\n",
						webhookURLEnv, strings.TrimSpace(os.Getenv(webhookURLEnv)))
				default:
					fmt.Fprintf(out, "  set %s=https://... to switch them on\n", webhookURLEnv)
				}
				return nil
			}
			fmt.Fprintf(out, "webhooks on: %s\n", endpoint)
			if signed {
				fmt.Fprintf(out, "  signed: HMAC-SHA256 in X-Entire-Signature-256\n")
			} else {
				fmt.Fprintf(out, "  unsigned: set %s so the receiver can verify the sender\n", webhookSecretEnv)
			}
			if includeText {
				fmt.Fprintf(out, "  fact text IS included (%s is set)\n", webhookIncludeTextEnv)
			} else {
				fmt.Fprintf(out, "  fact text is not sent; ids, kinds and paths only\n")
			}
			fmt.Fprintf(out, "  events: %s\n", strings.Join(webhookEventNames(), ", "))
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit the status as JSON")
	return cmd
}

func newWebhookTestCommand(opts Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "test",
		Short: "Send a webhook.test event to the configured endpoint",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			endpoint, enabled, reason := webhookEndpoint()
			if !enabled {
				// An error rather than a no-op: somebody who typed `webhook test`
				// wants a delivery, and reporting success for a send that never
				// happened is the worst possible answer.
				return fmt.Errorf("webhooks are off: %s", reason)
			}
			event := webhookEvent{
				Event:     WebhookTest,
				Timestamp: opts.Now().UTC().Format(time.RFC3339),
				Repo:      webhookRepoName(opts.Env.RepoRoot),
			}
			if err := deliverWebhook(cmd.Context(), endpoint, event); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "delivered %s to %s\n", WebhookTest, endpoint)
			return nil
		},
	}
	return cmd
}

func newWebhookEventsCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "events",
		Short: "List the events this brain sends",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			for _, event := range webhookCatalogue() {
				fmt.Fprintf(cmd.OutOrStdout(), "%-18s %s\n", event.Name, event.Description)
			}
			return nil
		},
	}
}
