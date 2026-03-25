package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/memory"
	"github.com/ekovshilovsky/cloister/internal/vm"
	vmcolima "github.com/ekovshilovsky/cloister/internal/vm/colima"
	vmlume "github.com/ekovshilovsky/cloister/internal/vm/lume"
	"github.com/spf13/cobra"
)

// statusFlags holds flag state for the status subcommand.
type statusFlags struct {
	jsonOutput bool
}

var sf statusFlags

func init() {
	rootCmd.AddCommand(statusCmd)
	statusCmd.Flags().BoolVar(&sf.jsonOutput, "json", false, "Emit profile status as a JSON array instead of a human-readable table")
}

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show status of all cloister profiles",
	Long: `Display the state, resource allocation, idle time, and tunnel health for
every profile defined in the cloister configuration.

Pass --json to receive a machine-readable JSON array suitable for scripting.`,
	Args: cobra.NoArgs,
	RunE: runStatus,
}

// profileStatus is the machine-readable representation of a single profile's
// runtime state, emitted when --json is set.
type profileStatus struct {
	Name     string   `json:"name"`
	Backend  string   `json:"backend"`
	State    string   `json:"state"`
	MemoryGB int      `json:"memory_gb"`
	Idle     string   `json:"idle"`
	Host     string   `json:"host"`
	Stacks   []string `json:"stacks"`
}

// runStatus is the handler for the status subcommand.
func runStatus(cmd *cobra.Command, args []string) error {
	cfgPath, err := config.ConfigPath()
	if err != nil {
		return fmt.Errorf("resolving config path: %w", err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	if len(cfg.Profiles) == 0 {
		cmd.Println("No profiles defined. Create one with: cloister create <name>")
		return nil
	}

	// Use colima backend directly to list all colima VMs in a single call.
	// In the future, both backends would be queried when listing all managed VMs.
	backend := &vmcolima.Backend{}
	vmList, err := backend.List(false)
	if err != nil {
		// Non-fatal: we can still display config-only information with an
		// unknown state rather than refusing to run entirely.
		fmt.Fprintf(os.Stderr, "warning: could not query VM state: %v\n", err)
	}

	// Build a lookup map from profile name to VM status for O(1) access below.
	vmByProfile := make(map[string]vm.VMStatus, len(vmList))
	for _, s := range vmList {
		pName := backend.ProfileFromVMName(s.Name)
		if pName != "" {
			vmByProfile[pName] = s
		}
	}

	// Determine the effective memory budget.
	budgetGB := cfg.MemoryBudget
	if budgetGB == 0 {
		budgetGB = config.CalculateBudget(getSystemRAM())
	}

	// Calculate total memory allocated to running VMs.
	var usedGB int
	for name, p := range cfg.Profiles {
		s, running := vmByProfile[name]
		if running && strings.EqualFold(s.Status, "running") {
			mem := p.Memory
			if mem == 0 {
				mem = config.DefaultMemory
			}
			usedGB += mem
		}
	}

	if sf.jsonOutput {
		return printStatusJSON(cmd, cfg, vmByProfile)
	}

	return printStatusTable(cmd, cfg, vmByProfile, usedGB, budgetGB)
}

// profileHost returns the network address used to reach the given profile.
// For Colima profiles the service is only reachable via SSH tunnel on loopback.
// For Lume profiles the VM advertises its mDNS name on the local network.
func profileHost(name string, backend string) string {
	if strings.EqualFold(backend, "lume") {
		return vmlume.MDNSName(name)
	}
	return "localhost (ssh tunnel)"
}

// printStatusTable renders the profile status as an aligned table using
// text/tabwriter for column alignment.
func printStatusTable(cmd *cobra.Command, cfg *config.Config, vmByProfile map[string]vm.VMStatus, usedGB, budgetGB int) error {
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)

	fmt.Fprintln(w, "PROFILE\tBACKEND\tSTATE\tMEMORY\tIDLE\tHOST\tSTACKS")

	for name, p := range cfg.Profiles {
		state := "stopped"
		if s, ok := vmByProfile[name]; ok {
			state = strings.ToLower(s.Status)
		}

		mem := p.Memory
		if mem == 0 {
			mem = config.DefaultMemory
		}
		memStr := fmt.Sprintf("%dGB", mem)

		idle := readIdleTime(name)

		backend := p.Backend
		if backend == "" {
			backend = "colima"
		}

		host := profileHost(name, backend)

		stacks := strings.Join(p.Stacks, ",")
		if stacks == "" {
			stacks = "-"
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", name, backend, state, memStr, idle, host, stacks)
	}

	if err := w.Flush(); err != nil {
		return fmt.Errorf("flushing table output: %w", err)
	}

	// Budget and tunnel summary lines below the table.
	fmt.Fprintf(cmd.OutOrStdout(), "\nBudget: %dGB / %dGB used\n", usedGB, budgetGB)

	// Tunnel health summary derived from the configuration.
	printTunnelSummary(cmd, cfg)

	return nil
}

// printStatusJSON serialises the profile status list to a JSON array.
func printStatusJSON(cmd *cobra.Command, cfg *config.Config, vmByProfile map[string]vm.VMStatus) error {
	statuses := make([]profileStatus, 0, len(cfg.Profiles))

	for name, p := range cfg.Profiles {
		state := "stopped"
		if s, ok := vmByProfile[name]; ok {
			state = strings.ToLower(s.Status)
		}

		mem := p.Memory
		if mem == 0 {
			mem = config.DefaultMemory
		}

		backend := p.Backend
		if backend == "" {
			backend = "colima"
		}

		stacks := p.Stacks
		if stacks == nil {
			stacks = []string{}
		}

		statuses = append(statuses, profileStatus{
			Name:     name,
			Backend:  backend,
			State:    state,
			MemoryGB: mem,
			Idle:     readIdleTime(name),
			Host:     profileHost(name, backend),
			Stacks:   stacks,
		})
	}

	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(statuses)
}

// printTunnelSummary writes a one-line tunnel health digest to the command
// output. Each tunnel defined in the configuration is listed with a check mark
// when its health-check URL is reachable, or a cross when it is not. Tunnels
// without a health-check URL are shown as unknown.
func printTunnelSummary(cmd *cobra.Command, cfg *config.Config) {
	if len(cfg.Tunnels) == 0 {
		return
	}

	var parts []string
	for _, t := range cfg.Tunnels {
		// Health checking requires network I/O; for now mark all tunnels as
		// unknown (cross) until the tunnel manager is implemented in Task 8.
		parts = append(parts, fmt.Sprintf("%s \u2717", t.Name))
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Tunnels: %s\n", strings.Join(parts, "  "))
}

// readIdleTime returns a human-readable string representing how long ago the
// named profile was last entered. The timestamp is read from the state file
// written by enterProfile.
//
// Format:
//   - "active"  — less than one minute ago
//   - "Xm"      — X minutes ago (< 1 hour)
//   - "Xh"      — X hours ago (>= 1 hour)
//   - "never"   — no recorded entry (state file absent or unreadable)
func readIdleTime(profile string) string {
	dir, err := config.ConfigDir()
	if err != nil {
		return "never"
	}

	path := filepath.Join(dir, "state", profile+".last_entry")
	data, err := os.ReadFile(path)
	if err != nil {
		return "never"
	}

	ts, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return "never"
	}

	since := time.Since(time.Unix(ts, 0))

	switch {
	case since < time.Minute:
		return "active"
	case since < time.Hour:
		return fmt.Sprintf("%dm", int(since.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(since.Hours()))
	}
}

// getSystemRAM returns the total installed RAM of the host in gigabytes.
func getSystemRAM() int {
	return memory.GetSystemRAM()
}
