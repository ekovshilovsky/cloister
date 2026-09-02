package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"cloister.io/internal/config"
	"cloister.io/internal/memory"
	"cloister.io/internal/vm"
	vmcolima "cloister.io/internal/vm/colima"
	vmlume "cloister.io/internal/vm/lume"
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
	Use:   "status [profile]",
	Short: "Show status of all cloister profiles",
	Long: `Display the state, resource allocation, idle time, and configured tunnels
for every profile defined in the cloister configuration, or for one named
profile.

Pass --json to receive a machine-readable JSON array suitable for scripting.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runStatus,
}

// Profile states that are not read back from a backend verbatim.
const (
	// stateStopped means a backend answered and did not list the VM.
	stateStopped = "stopped"
	// stateUnknown means no backend could be asked, so the VM's state was
	// never measured. Reporting it as stopped would be indistinguishable from
	// a measurement, and wrong whenever the VM is in fact running.
	stateUnknown = "unknown"
)

// statusBackend is the part of a VM backend that status depends on. Narrowing
// it here is what lets the reporting be exercised without a hypervisor.
type statusBackend interface {
	List(verbose bool) ([]vm.VMStatus, error)
	ProfileFromVMName(vmName string) string
}

// namedBackend pairs a backend with the configuration value that selects it
// and the name used when reporting about it.
type namedBackend struct {
	name    string
	display string
	backend statusBackend
}

// vmInventory is what the backends were able to report.
//
// unreachable records the backends that could not be enumerated, because a
// profile's absence from byProfile carries information only when the backend
// it belongs to actually answered.
type vmInventory struct {
	byProfile   map[string]vm.VMStatus
	unreachable map[string]bool
	warnings    []string
}

// queryVMs enumerates each backend, collecting both the VMs found and the
// backends that could not be asked. A backend that fails yields one warning
// naming it and the cause, not one per profile configured against it.
func queryVMs(backends []namedBackend) vmInventory {
	inventory := vmInventory{
		byProfile:   make(map[string]vm.VMStatus),
		unreachable: make(map[string]bool),
	}
	for _, b := range backends {
		vms, err := b.backend.List(false)
		if err != nil {
			inventory.unreachable[b.name] = true
			inventory.warnings = append(inventory.warnings,
				fmt.Sprintf("Could not query %s status: %v", b.display, err))
			continue
		}
		for _, s := range vms {
			if profile := b.backend.ProfileFromVMName(s.Name); profile != "" {
				inventory.byProfile[profile] = s
			}
		}
	}
	return inventory
}

// stateOf reports the state to display for a profile on the named backend.
func (inv vmInventory) stateOf(profile, backend string) string {
	if s, ok := inv.byProfile[profile]; ok {
		return strings.ToLower(s.Status)
	}
	if inv.unreachable[canonicalBackendName(backend)] {
		return stateUnknown
	}
	return stateStopped
}

// canonicalBackendName resolves the configured backend value, which is empty
// for profiles written before the field existed.
func canonicalBackendName(backend string) string {
	if backend == "" {
		return "colima"
	}
	return strings.ToLower(backend)
}

// selectProfiles returns the profiles to report, sorted by name so that two
// runs produce the same row order and the output can be diffed. An empty
// filter selects every profile.
func selectProfiles(profiles map[string]*config.Profile, filter string) ([]string, error) {
	if filter != "" {
		if _, ok := profiles[filter]; !ok {
			return nil, fmt.Errorf("profile %q not found", filter)
		}
		return []string{filter}, nil
	}
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
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

	var filter string
	if len(args) == 1 {
		filter = args[0]
	}
	names, err := selectProfiles(cfg.Profiles, filter)
	if err != nil {
		return err
	}

	inventory := queryVMs([]namedBackend{
		{name: "colima", display: "Colima", backend: &vmcolima.Backend{}},
		{name: "lume", display: "Lume", backend: &vmlume.Backend{}},
	})

	// Warnings go to stderr so that --json stays a valid document on stdout.
	for _, warning := range inventory.warnings {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", warning)
	}

	// Determine the effective memory budget.
	budgetGB := cfg.MemoryBudget
	if budgetGB == 0 {
		budgetGB = config.CalculateBudget(getSystemRAM())
	}

	// Calculate total memory allocated to running VMs. The budget covers every
	// profile, not just the ones being displayed, so a filtered view still
	// reports the true consumption.
	var usedGB int
	for name, p := range cfg.Profiles {
		s, listed := inventory.byProfile[name]
		if listed && strings.EqualFold(s.Status, "running") {
			mem := p.Memory
			if mem == 0 {
				mem = config.DefaultMemory
			}
			usedGB += mem
		}
	}

	if sf.jsonOutput {
		return printStatusJSON(cmd, cfg, names, inventory)
	}

	return printStatusTable(cmd, cfg, names, inventory, usedGB, budgetGB)
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
func printStatusTable(cmd *cobra.Command, cfg *config.Config, names []string, inventory vmInventory, usedGB, budgetGB int) error {
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)

	fmt.Fprintln(w, "PROFILE\tBACKEND\tSTATE\tMEMORY\tIDLE\tHOST\tSTACKS")

	for _, name := range names {
		p := cfg.Profiles[name]
		state := inventory.stateOf(name, p.Backend)

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

	if summary := tunnelSummary(cfg.Tunnels); summary != "" {
		fmt.Fprintln(cmd.OutOrStdout(), summary)
	}

	printDockerContextSummary(cmd, inventory.byProfile)

	return nil
}

// printDockerContextSummary shows which engine the host's docker CLI is
// pointed at. Cloister never changes that selection itself, but a user who
// selected a cloister VM's context by hand, or who is carrying one over from
// an older cloister release, is left with a docker CLI that cannot connect
// once the VM stops. The warning names the cause and the one-line fix.
// Output is skipped entirely when the docker CLI is unavailable.
func printDockerContextSummary(cmd *cobra.Command, vmByProfile map[string]vm.VMStatus) {
	out := cmd.OutOrStdout()
	if host := os.Getenv("DOCKER_HOST"); host != "" {
		fmt.Fprintf(out, "Host docker: DOCKER_HOST=%s (docker contexts are bypassed while it is set)\n", host)
		return
	}
	current, err := vmcolima.CurrentDockerContext()
	if err != nil {
		return
	}
	fmt.Fprintf(out, "Host docker context: %s\n", current)

	contexts, err := vmcolima.ListDockerContexts()
	if err != nil {
		return
	}
	lookup := func(profile string) (exists, running bool) {
		s, ok := vmByProfile[profile]
		return ok, ok && strings.EqualFold(s.Status, "running")
	}
	if advice := vmcolima.DockerContextAdvice(current, vmcolima.PreferredHostDockerContext(contexts), lookup); advice != "" {
		fmt.Fprintf(out, "  warning: %s\n", advice)
	}
}

// printStatusJSON serialises the profile status list to a JSON array.
func printStatusJSON(cmd *cobra.Command, cfg *config.Config, names []string, inventory vmInventory) error {
	statuses := make([]profileStatus, 0, len(names))

	for _, name := range names {
		p := cfg.Profiles[name]
		state := inventory.stateOf(name, p.Backend)

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

// tunnelSummary renders the one-line digest of the tunnels the configuration
// defines, or "" when it defines none.
//
// The line names the configured tunnels and says their health was not
// measured. Status does no network I/O, so it has no verdict to report: a pass
// or fail marker here would be a claim about a check that never ran.
func tunnelSummary(tunnels []config.TunnelConfig) string {
	if len(tunnels) == 0 {
		return ""
	}
	names := make([]string, 0, len(tunnels))
	for _, t := range tunnels {
		names = append(names, t.Name)
	}
	return fmt.Sprintf("Tunnels configured (health not checked): %s", strings.Join(names, ", "))
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
