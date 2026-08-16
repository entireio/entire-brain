package cli

import (
	"bytes"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// runPrivacyLinearizedPatternMutation is the commit/egress boundary for the
// explicit pattern operations which may send transcript-derived material to a
// provider or persist a derived result. Unlike an ordinary retrieval, provider
// egress cannot be revoked after it starts. The sorted Brain locks therefore
// remain held from the final derived-state check through provider execution,
// persistence, and emission of the completely buffered command response.
//
// This deliberately trades bounded exclusion/purge latency (the provider
// timeout) for a real privacy linearization point. Holding only an epoch token
// would be insufficient: a tombstone could commit after the request bytes had
// reached the provider but before a later epoch check noticed it.
func runPrivacyLinearizedPatternMutation(cmd *cobra.Command, policies []retrievalPrivacyPolicy, mutate func() error) error {
	for i := range policies {
		policies[i].RequireDerivedClean = true
	}
	originalOut := cmd.OutOrStdout()
	originalErr := cmd.ErrOrStderr()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	defer func() {
		cmd.SetOut(originalOut)
		cmd.SetErr(originalErr)
	}()

	return withLockedRetrievalPrivacyPolicies(policies, func() error {
		// Policy identity alone is not enough: a cleanup transaction may still
		// mark derived projections dirty under that same tombstone generation.
		for _, policy := range policies {
			if policy.BrainDir == "" {
				continue
			}
			if err := requirePrivacyDerivedRead(policy.BrainDir); err != nil {
				return err
			}
		}
		if err := mutate(); err != nil {
			return err
		}
		cmd.SetOut(originalOut)
		cmd.SetErr(originalErr)
		if err := writeAllCommandBytes(originalOut, stdout.Bytes()); err != nil {
			return err
		}
		return writeAllCommandBytes(originalErr, stderr.Bytes())
	})
}

func writeAllCommandBytes(out io.Writer, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	n, err := out.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	if err != nil {
		return fmt.Errorf("write command output: %w", err)
	}
	return nil
}
