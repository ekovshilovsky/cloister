package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(cleanupCmd)
}

var cleanupCmd = &cobra.Command{
	Use:   "cleanup",
	Short: "Kill stale VM processes that hold macOS Virtualization.framework slots",
	Long: `macOS limits concurrent VMs to 2. Stale lume processes from failed runs
can hold VM slots even after the VM is stopped. This command finds and
kills those processes to free slots.

Also reports other Virtualization.framework consumers (e.g. Docker Desktop)
that may be using VM slots.`,
	RunE: runCleanup,
}

func runCleanup(cmd *cobra.Command, args []string) error {
	out, _ := exec.Command("pgrep", "-f", "lume.*run").Output()
	pids := strings.Fields(strings.TrimSpace(string(out)))

	killed := 0
	for _, pidStr := range pids {
		pid := 0
		fmt.Sscanf(pidStr, "%d", &pid)
		if pid == 0 || pid == os.Getpid() {
			continue
		}

		cmdline, err := exec.Command("ps", "-p", pidStr, "-o", "command=").Output()
		if err != nil {
			continue
		}
		cmdStr := strings.TrimSpace(string(cmdline))

		parts := strings.Fields(cmdStr)
		vmName := ""
		for i, p := range parts {
			if p == "run" && i+1 < len(parts) {
				vmName = parts[i+1]
				break
			}
		}
		if vmName == "" {
			continue
		}

		status := "unknown"
		statusOut, err := exec.Command("lume", "get", vmName, "--format", "json").CombinedOutput()
		if err == nil {
			if strings.Contains(string(statusOut), `"running"`) {
				status = "running"
			} else {
				status = "stopped"
			}
		}

		if status != "running" {
			fmt.Printf("Killing stale lume process (pid %d, VM %q, status: %s)\n", pid, vmName, status)
			proc, err := os.FindProcess(pid)
			if err == nil {
				if err := proc.Kill(); err != nil {
					fmt.Fprintf(os.Stderr, "  Failed to kill pid %d: %v\n", pid, err)
				} else {
					killed++
				}
			}
		} else {
			fmt.Printf("Skipping pid %d — VM %q is running\n", pid, vmName)
		}
	}

	vzOut, _ := exec.Command("pgrep", "-f", "com.apple.Virtualization.VirtualMachine").Output()
	vzPids := strings.Fields(strings.TrimSpace(string(vzOut)))
	fmt.Printf("\nVirtualization.framework slots in use: %d (macOS limit: 2)\n", len(vzPids))

	if len(vzPids) > 0 {
		dockerOut, _ := exec.Command("pgrep", "-f", "com.docker.virtualization").Output()
		if len(strings.TrimSpace(string(dockerOut))) > 0 {
			fmt.Println("  Docker Desktop is using 1 VM slot.")
		}
	}

	if killed > 0 {
		fmt.Printf("\nKilled %d stale process(es). VM slots should be freed.\n", killed)
	} else if len(pids) == 0 {
		fmt.Println("No stale lume processes found.")
	}

	return nil
}
