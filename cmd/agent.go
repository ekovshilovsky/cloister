package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/ekovshilovsky/cloister/internal/agent"
	"github.com/ekovshilovsky/cloister/internal/config"
	"github.com/ekovshilovsky/cloister/internal/memory"
	"github.com/ekovshilovsky/cloister/internal/tunnel"
	"github.com/ekovshilovsky/cloister/internal/vm"
	"github.com/ekovshilovsky/cloister/internal/vm/colima"
	"github.com/spf13/cobra"
)

// agentCmd is the parent command for all agent lifecycle operations. Each
// subcommand targets a specific headless profile by name and delegates to the
// internal agent package for Docker and SSH management.
var agentCmd = &cobra.Command{
	Use:   "agent <profile>",
	Short: "Manage headless agent profiles",
	Long: `Start, stop, and inspect headless agent containers running inside cloister VMs.

Agent profiles are created with --headless or --openclaw and run Docker containers
instead of interactive terminal sessions. Use these subcommands to manage the
full agent lifecycle without attaching a shell.`,
}

func init() {
	rootCmd.AddCommand(agentCmd)

	// start
	agentCmd.AddCommand(agentStartCmd)

	// stop
	agentCmd.AddCommand(agentStopCmd)

	// restart
	agentCmd.AddCommand(agentRestartCmd)

	// status
	agentStatusCmd.Flags().BoolVar(&agentStatusJSON, "json", false, "Emit status as JSON instead of a human-readable table")
	agentCmd.AddCommand(agentStatusCmd)

	// logs
	agentLogsCmd.Flags().BoolVarP(&agentLogsFollow, "follow", "f", false, "Stream logs in real-time instead of printing the last 100 lines")
	agentCmd.AddCommand(agentLogsCmd)

	// forward
	agentCmd.AddCommand(agentForwardCmd)

	// close
	agentCloseCmd.Flags().BoolVar(&agentCloseAll, "all", false, "Close all active port forwards for the profile")
	agentCmd.AddCommand(agentCloseCmd)
}

// Flag variables scoped to agent subcommands.
var (
	agentStatusJSON bool
	agentLogsFollow bool
	agentCloseAll   bool
)

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// loadAgentProfile loads the configuration, locates the named profile, and
// verifies that it is a headless agent profile with a non-nil Agent block.
// Returns the full config, the profile, and its name for use in subcommands.
func loadAgentProfile(name string) (*config.Config, *config.Profile, error) {
	cfgPath, err := config.ConfigPath()
	if err != nil {
		return nil, nil, fmt.Errorf("resolving config path: %w", err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, fmt.Errorf("loading config: %w", err)
	}
	p, ok := cfg.Profiles[name]
	if !ok {
		return nil, nil, fmt.Errorf("profile %q not found", name)
	}
	if !p.Headless || p.Agent == nil {
		return nil, nil, fmt.Errorf("profile %q is not a headless agent profile", name)
	}
	return cfg, p, nil
}

// ---------------------------------------------------------------------------
// agent start
// ---------------------------------------------------------------------------

var agentStartCmd = &cobra.Command{
	Use:   "start <profile>",
	Short: "Start the agent container for a headless profile",
	Long: `Start the VM (if not already running), verify Docker availability, pull and
run the agent's Docker container, and persist the container ID to state. The
profile's auto_start flag is set to true so that future VM boots automatically
launch the agent.`,
	Args: cobra.ExactArgs(1),
	RunE: runAgentStart,
}

func runAgentStart(cmd *cobra.Command, args []string) error {
	name := args[0]
	cfg, p, err := loadAgentProfile(name)
	if err != nil {
		return err
	}
	p.ApplyDefaults()

	// Ensure the VM is running, starting it if necessary with a memory budget
	// check to avoid exceeding the host's memory allocation.
	if !vm.IsRunning(name) {
		vms, _ := vm.List(false)
		running := make(map[string]bool)
		for _, v := range vms {
			if strings.EqualFold(v.Status, "Running") {
				pName := vm.ProfileFromVMName(v.Name)
				if pName != "" {
					running[pName] = true
				}
			}
		}

		result := memory.CheckDefault(cfg, name, running)
		if result.Exceeded {
			return fmt.Errorf("%s", result.FormatNonInteractive())
		}

		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolving home directory: %w", err)
		}
		workspaceDir, err := config.ResolveWorkspaceDir(p.StartDir, home)
		if err != nil {
			return fmt.Errorf("invalid workspace directory: %w", err)
		}
		mounts := vm.BuildMounts(home, workspaceDir, p.Stacks, p.MountPolicy, p.Headless)

		// Mount the agent data directory (writable) and compose directory (read-only).
		agentDir, err := agentDataDir(name, p.Agent.Type)
		if err != nil {
			return fmt.Errorf("resolving agent data directory: %w", err)
		}
		os.MkdirAll(agentDir, 0o700) //nolint:errcheck
		mounts = append(mounts, vm.Mount{Location: agentDir, Writable: true})

		// Write/update the compose file on the host and mount read-only
		if err := agent.WriteComposeFile(name, p.Agent, agentDir, workspaceDir); err != nil {
			return fmt.Errorf("writing compose file: %w", err)
		}
		composeDir := agent.ComposeDir(name, p.Agent.Type)
		mounts = append(mounts, vm.Mount{Location: composeDir, Writable: false})

		fmt.Printf("Starting VM for %q...\n", name)
		if err := vm.Start(name, p.CPU, p.Memory, p.Disk, mounts, false); err != nil {
			return fmt.Errorf("starting VM: %w", err)
		}

		// Establish tunnels for any host services the profile is configured to
		// consume (e.g., op-forward for credential injection).
		// TODO(task-10): resolve backend from profile config instead of hard-coding Colima.
		backend := &colima.Backend{}
		results := tunnel.Discover()
		resolvedPolicy := p.TunnelPolicy.ResolveForTunnels(p.Headless)
		results = tunnel.FilterByPolicy(results, resolvedPolicy)
		tunnel.PrintDiscovery(results)
		if err := tunnel.StartAll(name, backend, results, cfg.Tunnels); err != nil {
			fmt.Fprintf(os.Stderr, "warning: tunnel setup incomplete: %v\n", err)
		}
		if err := tunnel.DeployShims(name, backend, results); err != nil {
			fmt.Fprintf(os.Stderr, "warning: shim deployment incomplete: %v\n", err)
		}
	}

	// Verify Docker is operational inside the VM before attempting to start
	// the agent container.
	if err := agent.CheckDocker(name); err != nil {
		return err
	}

	// Resolve the workspace directory for use as the container's workspace bind mount.
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving home directory: %w", err)
	}
	workspaceDir, err := config.ResolveWorkspaceDir(p.StartDir, home)
	if err != nil {
		return fmt.Errorf("invalid workspace directory: %w", err)
	}

	agentDir, err := agentDataDir(name, p.Agent.Type)
	if err != nil {
		return fmt.Errorf("resolving agent data directory: %w", err)
	}

	fmt.Printf("Starting agent container for %q...\n", name)
	containerID, err := agent.StartContainer(name, p.Agent, agentDir, workspaceDir)
	if err != nil {
		return err
	}

	// Persist the container ID so that stop, status, and logs can locate it.
	stateDir, err := agent.StateDir()
	if err != nil {
		return fmt.Errorf("resolving state directory: %w", err)
	}
	os.MkdirAll(stateDir, 0o700) //nolint:errcheck
	if err := agent.WriteContainerID(stateDir, name, containerID); err != nil {
		return fmt.Errorf("writing container state: %w", err)
	}

	// Enable auto-start so that the agent is launched on subsequent VM boots.
	p.Agent.AutoStart = true
	cfgPath, _ := config.ConfigPath()
	if err := config.Save(cfgPath, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not persist auto_start flag: %v\n", err)
	}

	fmt.Printf("Agent %q started (container %s)\n", name, containerID[:12])
	ports := make([]string, len(p.Agent.Ports))
	for i, port := range p.Agent.Ports {
		ports[i] = strconv.Itoa(port)
	}
	fmt.Printf("Published ports: %s\n", strings.Join(ports, ", "))
	fmt.Printf("Forward with: cloister agent forward %s <port>\n", name)
	return nil
}

// ---------------------------------------------------------------------------
// agent stop
// ---------------------------------------------------------------------------

var agentStopCmd = &cobra.Command{
	Use:   "stop <profile>",
	Short: "Stop the running agent container",
	Long: `Stop and remove the agent's Docker container, clean up state files and port
forwards, and set auto_start to false so the agent is not relaunched on the
next VM boot.`,
	Args: cobra.ExactArgs(1),
	RunE: runAgentStop,
}

func runAgentStop(cmd *cobra.Command, args []string) error {
	name := args[0]
	cfg, p, err := loadAgentProfile(name)
	if err != nil {
		return err
	}

	stateDir, err := agent.StateDir()
	if err != nil {
		return fmt.Errorf("resolving state directory: %w", err)
	}

	containerID, err := agent.ReadContainerID(stateDir, name)
	if err != nil {
		return fmt.Errorf("no running agent for profile %q", name)
	}

	fmt.Printf("Stopping agent container for %q...\n", name)
	if err := agent.StopContainerWithType(name, containerID, p.Agent.Type); err != nil {
		return err
	}

	agent.RemoveContainerID(stateDir, name)

	// Tear down any active port forwards for this profile.
	agent.CloseAllForwards(name)

	// Disable auto-start so the container is not relaunched on VM boot.
	p.Agent.AutoStart = false
	cfgPath, _ := config.ConfigPath()
	if err := config.Save(cfgPath, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not persist auto_start flag: %v\n", err)
	}

	fmt.Printf("Agent %q stopped.\n", name)
	return nil
}

// ---------------------------------------------------------------------------
// agent restart
// ---------------------------------------------------------------------------

var agentRestartCmd = &cobra.Command{
	Use:   "restart <profile>",
	Short: "Restart the agent container (stop then start)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		// Stop errors are intentionally ignored so that a fresh start can
		// proceed even when the previous container is already gone.
		_ = runAgentStop(cmd, args)
		return runAgentStart(cmd, args)
	},
}

// ---------------------------------------------------------------------------
// agent status
// ---------------------------------------------------------------------------

var agentStatusCmd = &cobra.Command{
	Use:   "status [profile]",
	Short: "Show status of agent profiles",
	Long: `Without arguments, display a summary table of all headless agent profiles.
With a profile name, show detailed status including container state, uptime,
published ports, and active port forwards.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runAgentStatus,
}

func runAgentStatus(cmd *cobra.Command, args []string) error {
	if len(args) == 1 {
		return showSingleAgentStatus(cmd, args[0])
	}
	return showAllAgentStatus(cmd)
}

// showAllAgentStatus renders a table of every headless agent profile with its
// current container state, or JSON when --json is set.
func showAllAgentStatus(cmd *cobra.Command) error {
	cfgPath, err := config.ConfigPath()
	if err != nil {
		return fmt.Errorf("resolving config path: %w", err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	stateDir, err := agent.StateDir()
	if err != nil {
		return fmt.Errorf("resolving state directory: %w", err)
	}

	type agentRow struct {
		Profile   string `json:"profile"`
		State     string `json:"state"`
		Image     string `json:"image"`
		Uptime    string `json:"uptime"`
		AutoStart bool   `json:"auto_start"`
	}

	var rows []agentRow
	for name, p := range cfg.Profiles {
		if !p.Headless || p.Agent == nil {
			continue
		}

		row := agentRow{
			Profile:   name,
			State:     "stopped",
			Image:     p.Agent.Image,
			AutoStart: p.Agent.AutoStart,
		}

		containerID, err := agent.ReadContainerID(stateDir, name)
		if err == nil && containerID != "" && vm.IsRunning(name) {
			status, err := agent.InspectContainer(name, containerID)
			if err == nil {
				row.State = status.State
				row.Uptime = status.Uptime
			}
		}

		rows = append(rows, row)
	}

	if len(rows) == 0 {
		cmd.Println("No agent profiles defined. Create one with: cloister create --openclaw <name>")
		return nil
	}

	if agentStatusJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PROFILE\tSTATE\tIMAGE\tUPTIME\tAUTO_START")
	for _, r := range rows {
		uptime := r.Uptime
		if uptime == "" {
			uptime = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%v\n", r.Profile, r.State, r.Image, uptime, r.AutoStart)
	}
	return w.Flush()
}

// showSingleAgentStatus renders detailed information for a single agent profile
// including container state, published ports, and active SSH forwards.
func showSingleAgentStatus(cmd *cobra.Command, name string) error {
	_, p, err := loadAgentProfile(name)
	if err != nil {
		return err
	}

	stateDir, err := agent.StateDir()
	if err != nil {
		return fmt.Errorf("resolving state directory: %w", err)
	}

	state := "stopped"
	uptime := "-"
	image := p.Agent.Image

	containerID, err := agent.ReadContainerID(stateDir, name)
	if err == nil && containerID != "" && vm.IsRunning(name) {
		status, err := agent.InspectContainer(name, containerID)
		if err == nil {
			state = status.State
			uptime = status.Uptime
			image = status.Image
		}
	}

	ports := make([]string, len(p.Agent.Ports))
	for i, port := range p.Agent.Ports {
		ports[i] = strconv.Itoa(port)
	}

	forwards := agent.ListForwardPorts(stateDir, name)
	fwdStrs := make([]string, len(forwards))
	for i, port := range forwards {
		fwdStrs[i] = strconv.Itoa(port)
	}

	if agentStatusJSON {
		info := map[string]interface{}{
			"profile":    name,
			"state":      state,
			"uptime":     uptime,
			"image":      image,
			"ports":      p.Agent.Ports,
			"auto_start": p.Agent.AutoStart,
			"forwards":   forwards,
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(info)
	}

	cmd.Printf("Profile:    %s\n", name)
	cmd.Printf("State:      %s\n", state)
	cmd.Printf("Uptime:     %s\n", uptime)
	cmd.Printf("Image:      %s\n", image)
	cmd.Printf("Ports:      %s\n", strings.Join(ports, ", "))
	cmd.Printf("Auto-start: %v\n", p.Agent.AutoStart)
	if len(fwdStrs) > 0 {
		cmd.Printf("Forwards:   %s\n", strings.Join(fwdStrs, ", "))
	} else {
		cmd.Printf("Forwards:   none\n")
	}
	return nil
}

// ---------------------------------------------------------------------------
// agent logs
// ---------------------------------------------------------------------------

var agentLogsCmd = &cobra.Command{
	Use:   "logs <profile>",
	Short: "Tail or stream the agent container logs",
	Long: `Print the last 100 lines of the agent container's log output, or stream logs
in real-time with --follow.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		_, _, err := loadAgentProfile(name)
		if err != nil {
			return err
		}

		stateDir, err := agent.StateDir()
		if err != nil {
			return fmt.Errorf("resolving state directory: %w", err)
		}

		containerID, err := agent.ReadContainerID(stateDir, name)
		if err != nil {
			return fmt.Errorf("no running agent for profile %q", name)
		}

		return agent.ContainerLogs(name, containerID, agentLogsFollow)
	},
}

// ---------------------------------------------------------------------------
// agent forward
// ---------------------------------------------------------------------------

var agentForwardCmd = &cobra.Command{
	Use:   "forward <profile> <port>",
	Short: "Forward a container port to the host via SSH",
	Long: `Create an SSH local port forward from the macOS host to the VM, exposing the
specified container port on localhost. The port must be listed in the agent's
published ports configuration.

The forward persists until explicitly closed with 'cloister agent close'.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		port, err := strconv.Atoi(args[1])
		if err != nil {
			return fmt.Errorf("invalid port number %q: %w", args[1], err)
		}

		_, p, err := loadAgentProfile(name)
		if err != nil {
			return err
		}

		fmt.Printf("Forwarding port %d for %q...\n", port, name)
		if err := agent.StartForward(name, port, p.Agent); err != nil {
			return err
		}

		fmt.Printf("Port %d forwarded. Access at: http://localhost:%d\n", port, port)
		fmt.Println("Warning: this forward exposes the service on your local machine. Close it when done:")
		fmt.Printf("  cloister agent close %s %d\n", name, port)
		return nil
	},
}

// ---------------------------------------------------------------------------
// agent close
// ---------------------------------------------------------------------------

var agentCloseCmd = &cobra.Command{
	Use:   "close <profile> [port]",
	Short: "Close an active port forward",
	Long: `Close an SSH port forward for the given profile. Specify a port number to close
a single forward, or use --all to tear down every active forward for the profile.`,
	Args: cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		_, _, err := loadAgentProfile(name)
		if err != nil {
			return err
		}

		if agentCloseAll {
			fmt.Printf("Closing all forwards for %q...\n", name)
			agent.CloseAllForwards(name)
			fmt.Println("All forwards closed.")
			return nil
		}

		if len(args) < 2 {
			return fmt.Errorf("specify a port number or use --all to close all forwards")
		}

		port, err := strconv.Atoi(args[1])
		if err != nil {
			return fmt.Errorf("invalid port number %q: %w", args[1], err)
		}

		if err := agent.CloseForward(name, port); err != nil {
			return err
		}
		fmt.Printf("Forward for port %d closed.\n", port)
		return nil
	},
}
