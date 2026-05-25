// entire-plugin-template is a sample Entire CLI external command.
//
// Once built as an executable named `entire-plugin-template`, the parent
// Entire CLI dispatches it when a user runs `entire plugin-template`.
package main

import (
	"fmt"
	"os"

	"github.com/entireio/entire-plugin-template/internal/cli"
)

var version = "dev"

func main() {
	if err := cli.Execute(version); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
