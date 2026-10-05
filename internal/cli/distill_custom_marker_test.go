package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestCustomDistillMarkerHelper(t *testing.T) {
	if os.Getenv("ENTIRE_BRAIN_TEST_CUSTOM_MARKER") != "1" {
		return
	}
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(2)
	}
	data, _ := json.Marshal(struct {
		Args  []string
		Input string
	}{os.Args[len(os.Args)-2:], string(input)})
	fmt.Print(string(data))
	os.Exit(0)
}

func TestCustomDistillCommandPreservesPromptMarkerArgument(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_TEST_CUSTOM_MARKER", "1")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := []string{self, "-test.run=^TestCustomDistillMarkerHelper$", "--", distillAgentPromptMarker, "custom-value"}
	args, err := distillAgentCommandArgs("command", command, "private system prompt")
	if err != nil {
		t.Fatal(err)
	}
	out, err := defaultDistillAgentRunner("command")(context.Background(), t.TempDir(), args, []byte("transcript"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Args  []string
		Input string
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Args, command[len(command)-2:]) || got.Input != "transcript" {
		t.Fatalf("custom command arguments or stdin changed: %+v", got)
	}
}
