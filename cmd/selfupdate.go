package cmd

import (
	"github.com/ekovshilovsky/cloister/internal/selfupdate"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(selfUpdateCmd)
}

var selfUpdateCmd = &cobra.Command{
	Use:   "self-update",
	Short: "Update cloister to the latest release",
	Long: `Check for a newer cloister release on GitHub and, if one is found,
download the platform-appropriate binary and atomically replace the
running executable.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return selfupdate.Run(Version)
	},
}
