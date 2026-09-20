package cli

import "testing"

func TestSetupDisabledAgentRemainsDisabledInDaemon(t *testing.T) {
	opts := defaultSetupOptions()
	opts.agent = "none"
	w := daemonWatchOptions(t, opts)
	if w.distillAgent != "none" || w.distill {
		t.Fatalf("setup disabled agent became daemon distill=%v agent=%q", w.distill, w.distillAgent)
	}
}
