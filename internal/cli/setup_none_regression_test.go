package cli

import "testing"

func TestSetupDisabledAgentRemainsDisabledInDaemon(t *testing.T) {
	opts := defaultSetupOptions()
	opts.agent = "none"
	w := daemonWatchOptions(t, opts)
	if w.DistillAgent != "none" || w.Distill {
		t.Fatalf("setup disabled agent became daemon distill=%v agent=%q", w.Distill, w.DistillAgent)
	}
}
