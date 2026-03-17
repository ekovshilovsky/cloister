package cmd

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(configCmd)
}

// configCmd opens the cloister configuration file in the user's preferred
// editor and validates the resulting YAML after the editor process exits.
var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Edit configuration",
	Long: `Open the cloister configuration file (~/.cloister/config.yaml) in
$EDITOR (falling back to vim) and validate the YAML after the editor exits.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		path, err := config.ConfigPath()
		if err != nil {
			return err
		}

		// Bootstrap an empty config file when none exists so the editor opens
		// a valid, parseable document rather than a blank file.
		if _, err := os.Stat(path); os.IsNotExist(err) {
			cfg := &config.Config{Profiles: make(map[string]*config.Profile)}
			if err := config.Save(path, cfg); err != nil {
				return fmt.Errorf("initialising config file: %w", err)
			}
		}

		editor := os.Getenv("EDITOR")
		if editor == "" {
			editor = "vim"
		}

		// Attach the editor process to the current terminal so interactive
		// editing works correctly.
		c := exec.Command(editor, path)
		c.Stdin = os.Stdin
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		if err := c.Run(); err != nil {
			return err
		}

		// Validate the YAML after the editor exits so the user is informed of
		// any syntax errors introduced during editing.
		if _, err := config.Load(path); err != nil {
			fmt.Printf("Warning: config has YAML errors: %v\n", err)
		} else {
			fmt.Println("Config saved and validated.")
		}
		return nil
	},
}
