// cmd/cloister-vm/main.go
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Version is set at build time via -ldflags.
var Version = "dev"

var rootCmd = &cobra.Command{
	Use:   "cloister-vm",
	Short: "Toolkit for cloister VM environments",
	Long: `cloister-vm provides status, diagnostics, and configuration tools
for use inside cloister-managed virtual machines.

Run 'cloister-vm status' for a quick overview of your VM environment.`,
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print cloister-vm version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("cloister-vm %s\n", Version)
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
