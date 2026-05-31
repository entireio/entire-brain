// entire-brain is an Entire CLI external command.
//
// Once built as an executable named `entire-brain`, the parent Entire CLI
// dispatches it when a user runs `entire brain`.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/ashtom/entire-brain/internal/cli"
)

var version = "dev"

func main() {
	if err := cli.Execute(version); err != nil {
		if !errors.Is(err, cli.RenderedError()) {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
}
