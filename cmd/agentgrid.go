package cmd

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"cloister.io/internal/agentgrid"
	"cloister.io/internal/config"
	"cloister.io/internal/vm"
	"github.com/spf13/cobra"
)

// workspaceSelectorToken is the reserved selector that shares the whole
// workspace tree (the parent of every synchronized project) as one entry.
const workspaceSelectorToken = "workspace"

var (
	agentgridShareAll        bool
	agentgridShareWorkspace  bool
	agentgridListJSON        bool
	agentgridUnshareAll      bool
	agentgridUnshareWorkspce bool
)

var agentgridCmd = &cobra.Command{
	Use:   "agentgrid",
	Short: "Manage which workspace projects the Agent Grid daemon serves to paired clients",
	Long: `Manage which workspace projects the Agent Grid daemon in a profile's VM
will serve to a paired client.

Cloister already discovers the profile's projects. These commands pick which of
them the in-VM Agent Grid daemon exposes to remote devices, without touching the
user's desktop Agent Grid app. Projects are named by their workspace selector
(for example apps/AWSCrossReference). The reserved name "workspace" shares the
whole synchronized tree as a single project that contains every synced project.`,
}

var agentgridListCmd = &cobra.Command{
	Use:   "list <profile>",
	Short: "List shareable projects and whether each is currently shared",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runAgentgridList(cmd, args[0])
	},
}

var agentgridShareCmd = &cobra.Command{
	Use:   "share <profile> [selector...]",
	Short: "Share selected projects (or --all, or --workspace) with the Agent Grid daemon",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runAgentgridShare(cmd, args[0], args[1:])
	},
}

var agentgridUnshareCmd = &cobra.Command{
	Use:   "unshare <profile> [selector...]",
	Short: "Stop sharing selected projects (or --all, or --workspace)",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runAgentgridUnshare(cmd, args[0], args[1:])
	},
}

func init() {
	agentgridListCmd.Flags().BoolVar(&agentgridListJSON, "json", false, "Print the shareable projects as JSON")
	agentgridShareCmd.Flags().BoolVar(&agentgridShareAll, "all", false, "Share every discovered project")
	agentgridShareCmd.Flags().BoolVar(&agentgridShareWorkspace, "workspace", false, "Share the whole workspace tree as one project")
	agentgridUnshareCmd.Flags().BoolVar(&agentgridUnshareAll, "all", false, "Stop sharing every discovered project")
	agentgridUnshareCmd.Flags().BoolVar(&agentgridUnshareWorkspce, "workspace", false, "Stop sharing the whole-workspace entry")
	agentgridCmd.AddCommand(agentgridListCmd, agentgridShareCmd, agentgridUnshareCmd)
	rootCmd.AddCommand(agentgridCmd)
}

// shareable is one project cloister can expose to the daemon: its workspace
// selector (or the reserved "workspace" token), the absolute guest path the
// daemon must serve, and a display name.
type shareable struct {
	Selector  string
	GuestPath string
	Name      string
	Workspace bool
}

func runAgentgridList(cmd *cobra.Command, profileName string) error {
	backend, p, err := agentgridProfile(profileName)
	if err != nil {
		return err
	}
	shareables, err := agentgridShareables(backend, profileName, p)
	if err != nil {
		return err
	}
	entries, err := agentgridReadEntries(backend, profileName)
	if err != nil {
		return err
	}
	shared := make(map[string]bool, len(entries))
	for _, entry := range entries {
		shared[entry.Path] = true
	}
	if agentgridListJSON {
		type row struct {
			Selector string `json:"selector"`
			Path     string `json:"path"`
			Name     string `json:"name"`
			Shared   bool   `json:"shared"`
		}
		rows := make([]row, 0, len(shareables))
		for _, s := range shareables {
			rows = append(rows, row{Selector: s.Selector, Path: s.GuestPath, Name: s.Name, Shared: shared[s.GuestPath]})
		}
		data, marshalErr := json.MarshalIndent(rows, "", "  ")
		if marshalErr != nil {
			return marshalErr
		}
		cmd.Println(string(data))
		return nil
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "%-8s  %-40s  %s\n", "SHARED", "SELECTOR", "GUEST PATH")
	for _, s := range shareables {
		mark := "no"
		if shared[s.GuestPath] {
			mark = "yes"
		}
		fmt.Fprintf(out, "%-8s  %-40s  %s\n", mark, s.Selector, s.GuestPath)
	}
	return nil
}

func runAgentgridShare(cmd *cobra.Command, profileName string, selectors []string) error {
	backend, p, err := agentgridProfile(profileName)
	if err != nil {
		return err
	}
	shareables, err := agentgridShareables(backend, profileName, p)
	if err != nil {
		return err
	}
	chosen, err := selectShareables(shareables, selectors, agentgridShareAll, agentgridShareWorkspace)
	if err != nil {
		return err
	}
	existing, err := agentgridReadEntries(backend, profileName)
	if err != nil {
		return err
	}
	now := time.Now()
	desired := make([]agentgrid.Entry, 0, len(chosen))
	for _, s := range chosen {
		desired = append(desired, agentgrid.NewEntry(s.GuestPath, s.Name, now))
	}
	merged := agentgrid.Upsert(existing, desired)
	if err := agentgridWriteEntries(backend, profileName, merged); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	for _, s := range chosen {
		fmt.Fprintf(out, "Shared %s (%s)\n", s.Selector, s.GuestPath)
	}
	fmt.Fprintln(out, "Reopen the Agent Grid remote project picker to see the change.")
	return nil
}

func runAgentgridUnshare(cmd *cobra.Command, profileName string, selectors []string) error {
	backend, p, err := agentgridProfile(profileName)
	if err != nil {
		return err
	}
	shareables, err := agentgridShareables(backend, profileName, p)
	if err != nil {
		return err
	}
	chosen, err := selectShareables(shareables, selectors, agentgridUnshareAll, agentgridUnshareWorkspce)
	if err != nil {
		return err
	}
	existing, err := agentgridReadEntries(backend, profileName)
	if err != nil {
		return err
	}
	paths := make([]string, 0, len(chosen))
	for _, s := range chosen {
		paths = append(paths, s.GuestPath)
	}
	kept := agentgrid.RemovePaths(existing, paths)
	if err := agentgridWriteEntries(backend, profileName, kept); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	for _, s := range chosen {
		fmt.Fprintf(out, "Unshared %s (%s)\n", s.Selector, s.GuestPath)
	}
	return nil
}

// selectShareables resolves the caller's selectors, --all, and --workspace into
// a concrete set of shareables. It fails closed when nothing is selected or an
// unknown selector is named.
func selectShareables(shareables []shareable, selectors []string, all, workspace bool) ([]shareable, error) {
	var projects, workspaceEntry []shareable
	for _, s := range shareables {
		if s.Workspace {
			workspaceEntry = append(workspaceEntry, s)
			continue
		}
		projects = append(projects, s)
	}
	bySelector := make(map[string]shareable, len(projects))
	for _, s := range projects {
		bySelector[s.Selector] = s
	}
	chosen := make([]shareable, 0)
	seen := make(map[string]bool)
	add := func(s shareable) {
		if seen[s.GuestPath] {
			return
		}
		seen[s.GuestPath] = true
		chosen = append(chosen, s)
	}
	if all {
		for _, s := range projects {
			add(s)
		}
	}
	if workspace {
		for _, s := range workspaceEntry {
			add(s)
		}
	}
	for _, selector := range selectors {
		if selector == workspaceSelectorToken {
			for _, s := range workspaceEntry {
				add(s)
			}
			continue
		}
		s, ok := bySelector[selector]
		if !ok {
			return nil, fmt.Errorf("unknown project selector %q; run 'cloister agentgrid list' to see valid selectors", selector)
		}
		add(s)
	}
	if len(chosen) == 0 {
		return nil, fmt.Errorf("nothing selected; name one or more selectors, or pass --all or --workspace")
	}
	return chosen, nil
}

// agentgridProfile loads the profile and its backend, requiring a broker or
// workspace profile with a running VM. A profile without the agentgrid stack is
// allowed but warned about, because the daemon may be added later.
func agentgridProfile(profileName string) (vm.Backend, *config.Profile, error) {
	cfgPath, err := config.ConfigPath()
	if err != nil {
		return nil, nil, err
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, fmt.Errorf("loading config: %w", err)
	}
	p, ok := cfg.Profiles[profileName]
	if !ok {
		return nil, nil, fmt.Errorf("profile %q is not configured", profileName)
	}
	if !workspaceProvider(p).IsBroker() {
		return nil, nil, fmt.Errorf("profile %q is not a broker or workspace profile; Agent Grid sharing needs synchronized guest projects", profileName)
	}
	backend, err := resolveBackend(p.Backend)
	if err != nil {
		return nil, nil, err
	}
	if !backend.IsRunning(profileName) {
		return nil, nil, fmt.Errorf("profile %q VM is not running; start it before sharing", profileName)
	}
	if !hasStack(p, "agentgrid") {
		fmt.Fprintf(os.Stderr, "Warning: profile %q does not have the agentgrid stack; the daemon must be provisioned before a client can open these projects.\n", profileName)
	}
	return backend, p, nil
}

func hasStack(p *config.Profile, name string) bool {
	for _, s := range p.Stacks {
		if s == name {
			return true
		}
	}
	return false
}

// agentgridShareables enumerates the profile's project sessions and turns each
// into a shareable, then appends the whole-workspace entry.
func agentgridShareables(backend vm.Backend, profile string, p *config.Profile) ([]shareable, error) {
	specs, err := brokerSessionSpecs(backend, profile, p)
	if err != nil {
		return nil, err
	}
	root, err := agentgridWorkspaceRoot(p)
	if err != nil {
		return nil, err
	}
	guestHome, err := agentgridGuestHome(backend, profile)
	if err != nil {
		return nil, err
	}
	out := make([]shareable, 0, len(specs)+1)
	for _, spec := range specs {
		selector, err := agentgridSelector(root, spec.HostRoot)
		if err != nil {
			return nil, err
		}
		guestPath, err := agentgridGuestPath(guestHome, spec.GuestRoot)
		if err != nil {
			return nil, err
		}
		out = append(out, shareable{Selector: selector, GuestPath: guestPath, Name: selector})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Selector < out[j].Selector })
	out = append(out, shareable{
		Selector:  workspaceSelectorToken,
		GuestPath: path.Join(guestHome, "workspaces"),
		Name:      profile + " workspace",
		Workspace: true,
	})
	return out, nil
}

// agentgridSelector is the project's workspace-relative portable path.
func agentgridSelector(root, hostRoot string) (string, error) {
	rel, err := filepath.Rel(root, hostRoot)
	if err != nil {
		return "", fmt.Errorf("deriving project selector: %w", err)
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("project %q is outside the workspace root", hostRoot)
	}
	return rel, nil
}

// agentgridGuestPath turns a broker guest root (~/workspaces/<name>) into an
// absolute guest path under the resolved guest home.
func agentgridGuestPath(guestHome, guestRoot string) (string, error) {
	const prefix = "~/workspaces/"
	if !strings.HasPrefix(guestRoot, prefix) {
		return "", fmt.Errorf("unexpected broker guest root %q", guestRoot)
	}
	rel := strings.TrimPrefix(guestRoot, "~/")
	if strings.Contains(rel, "..") {
		return "", fmt.Errorf("unexpected broker guest root %q", guestRoot)
	}
	return path.Join(guestHome, rel), nil
}

func agentgridWorkspaceRoot(p *config.Profile) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	base := p.Workspace.Root
	if base == "" {
		base = p.StartDir
	}
	resolved, err := config.ResolveWorkspaceDir(base, home)
	if err != nil {
		return "", fmt.Errorf("resolving workspace root: %w", err)
	}
	root, err := filepath.EvalSymlinks(resolved)
	if err != nil {
		return "", fmt.Errorf("resolving workspace root: %w", err)
	}
	return root, nil
}

// agentgridGuestHome resolves the guest $HOME behind sentinels so a login
// shell's banner output cannot corrupt the value.
func agentgridGuestHome(backend vm.Backend, profile string) (string, error) {
	out, err := backend.SSHCapture(profile, `printf '__CLH[%s]CLH__' "$HOME"`)
	if err != nil {
		return "", fmt.Errorf("resolving guest home: %w", err)
	}
	value, ok := extractSentinel(out)
	if !ok || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("resolving guest home: unexpected output %q", out)
	}
	return strings.TrimSpace(value), nil
}

// agentgridReadEntries reads and parses the guest share list, treating a
// missing file as an empty list.
func agentgridReadEntries(backend vm.Backend, profile string) ([]agentgrid.Entry, error) {
	script := `printf '__CLH['; cat "$HOME/` + agentgrid.SharedListRelPath + `" 2>/dev/null; printf ']CLH__'`
	out, err := backend.SSHCapture(profile, script)
	if err != nil {
		return nil, fmt.Errorf("reading Agent Grid share list: %w", err)
	}
	body, ok := extractSentinel(out)
	if !ok {
		return nil, fmt.Errorf("reading Agent Grid share list: unexpected output")
	}
	return agentgrid.Parse([]byte(body))
}

// agentgridWriteEntries writes the share list atomically inside the VM. The
// JSON is base64-encoded and decoded in the guest so the payload is never
// embedded in the shell text: the base64 alphabet contains no shell
// metacharacters or quotes, so there is nothing for the data to break out of.
func agentgridWriteEntries(backend vm.Backend, profile string, entries []agentgrid.Entry) error {
	data, err := agentgrid.Marshal(entries)
	if err != nil {
		return err
	}
	encoded := base64.StdEncoding.EncodeToString(data)
	script := fmt.Sprintf(`set -eu
target="$HOME/%s"
mkdir -p "$(dirname "$target")"
tmp="$target.tmp-$$"
printf '%%s' %s | base64 -d > "$tmp"
chmod 600 "$tmp"
mv -f "$tmp" "$target"
`, agentgrid.SharedListRelPath, encoded)
	if _, err := backend.SSHScript(profile, script); err != nil {
		return fmt.Errorf("writing Agent Grid share list: %w", err)
	}
	return nil
}

func extractSentinel(out string) (string, bool) {
	const open, close = "__CLH[", "]CLH__"
	start := strings.Index(out, open)
	end := strings.LastIndex(out, close)
	if start < 0 || end < 0 || end < start+len(open) {
		return "", false
	}
	return out[start+len(open) : end], true
}
