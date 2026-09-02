package main

import (
	"fmt"
	"os"

	"cloister.io/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		if cmd.ShouldPrintError(err) {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
}
