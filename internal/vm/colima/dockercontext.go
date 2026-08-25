package colima

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// dockerContextPrefix is what Colima prepends to an instance name when it
// registers that instance's daemon as a host docker context. Combined with
// vmPrefix, every cloister-managed context is named "colima-cloister-<profile>".
const dockerContextPrefix = "colima-"

// DockerContextName returns the host docker context that Colima registers for
// the given cloister profile. Cloister never activates it (see startArgs), but
// it remains available for explicit use: `docker --context <name> ps`.
func DockerContextName(profile string) string {
	return dockerContextPrefix + VMName(profile)
}

// ProfileFromDockerContext extracts the cloister profile from a host docker
// context name. It returns an empty string for contexts that cloister does not
// own (Docker Desktop, user-created Colima profiles, the built-in default), so
// callers never act on a context they did not create.
func ProfileFromDockerContext(name string) string {
	if !strings.HasPrefix(name, dockerContextPrefix) {
		return ""
	}
	return ProfileFromVMName(strings.TrimPrefix(name, dockerContextPrefix))
}

// DockerContext is the subset of `docker context ls --format json` that
// cloister inspects.
type DockerContext struct {
	Name    string `json:"Name"`
	Current bool   `json:"Current"`
}

// ListDockerContexts enumerates the host's docker contexts. It fails when the
// docker CLI is unavailable, which callers treat as "nothing to report".
func ListDockerContexts() ([]DockerContext, error) {
	out, err := exec.Command("docker", "context", "ls", "--format", "json").Output()
	if err != nil {
		return nil, fmt.Errorf("docker context ls: %w", err)
	}
	return parseDockerContexts(out)
}

// parseDockerContexts decodes the newline-delimited JSON objects emitted by
// `docker context ls --format json`.
func parseDockerContexts(out []byte) ([]DockerContext, error) {
	var contexts []DockerContext
	for _, line := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var c DockerContext
		if err := json.Unmarshal(line, &c); err != nil {
			return nil, fmt.Errorf("parsing docker context list: %w", err)
		}
		contexts = append(contexts, c)
	}
	return contexts, nil
}

// CurrentDockerContext reports the context the host docker CLI will use for
// its next command. `docker context show` already honours the DOCKER_CONTEXT
// environment variable, so the result reflects the caller's shell rather
// than only the persisted selection.
func CurrentDockerContext() (string, error) {
	out, err := exec.Command("docker", "context", "show").Output()
	if err != nil {
		return "", fmt.Errorf("docker context show: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// RemoveDockerContext deletes a host docker context. --force is required to
// remove a context that is currently selected; the docker CLI then falls back
// to "default".
func RemoveDockerContext(name string) error {
	if out, err := exec.Command("docker", "context", "rm", "--force", name).CombinedOutput(); err != nil {
		return fmt.Errorf("docker context rm %s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// OrphanDockerContexts returns the cloister-owned docker contexts whose Colima
// instance no longer exists. vmNames are Colima instance names as reported by
// `colima list`. Contexts cloister does not own are never reported, so a
// user's own Colima profiles and Docker Desktop are left untouched.
func OrphanDockerContexts(contexts []DockerContext, vmNames []string) []string {
	present := make(map[string]struct{}, len(vmNames))
	for _, n := range vmNames {
		present[n] = struct{}{}
	}
	var orphans []string
	for _, c := range contexts {
		profile := ProfileFromDockerContext(c.Name)
		if profile == "" {
			continue
		}
		if _, ok := present[VMName(profile)]; !ok {
			orphans = append(orphans, c.Name)
		}
	}
	return orphans
}

// PreferredHostDockerContext picks the context a user most likely wants their
// host docker CLI pointed at when it has been left on a dead cloister context:
// Docker Desktop when it is registered, otherwise the built-in default.
func PreferredHostDockerContext(contexts []DockerContext) string {
	for _, c := range contexts {
		if c.Name == "desktop-linux" {
			return c.Name
		}
	}
	return "default"
}

// DockerContextAdvice explains, in one sentence, why the host docker CLI is
// not going to work when it is pointed at a cloister VM that is stopped or
// gone, and how to fix it. It returns an empty string when the current
// context is not cloister's concern or backs a running VM. lookup reports
// whether the VM for a profile exists and whether it is running.
func DockerContextAdvice(current, fallback string, lookup func(profile string) (exists, running bool)) string {
	profile := ProfileFromDockerContext(current)
	if profile == "" {
		return ""
	}
	exists, running := lookup(profile)
	switch {
	case !exists:
		return fmt.Sprintf("host docker is pointed at %q but that cloister VM no longer exists; switch back with: docker context use %s", current, fallback)
	case !running:
		return fmt.Sprintf("host docker is pointed at %q but profile %q is stopped; switch back with: docker context use %s", current, profile, fallback)
	}
	return ""
}

// CleanupOrphanDockerContexts removes docker contexts that cloister created
// (via Colima) for VMs that no longer exist. Colima normally removes a
// context when its VM stops or is deleted, but an unclean shutdown or a VM
// rename leaves the entry behind, where it clutters `docker context ls` and
// can be selected by mistake. Returns the number removed and a human-readable
// report line per action.
func (b *Backend) CleanupOrphanDockerContexts() (removed int, report []string, err error) {
	contexts, err := ListDockerContexts()
	if err != nil {
		return 0, nil, err
	}
	statuses, err := b.List(false)
	if err != nil {
		return 0, nil, err
	}
	vmNames := make([]string, 0, len(statuses))
	for _, s := range statuses {
		vmNames = append(vmNames, s.Name)
	}
	for _, name := range OrphanDockerContexts(contexts, vmNames) {
		if rmErr := RemoveDockerContext(name); rmErr != nil {
			report = append(report, fmt.Sprintf("Could not remove orphaned docker context %q: %v", name, rmErr))
			continue
		}
		removed++
		report = append(report, fmt.Sprintf("Removed orphaned docker context %q (no Colima VM behind it)", name))
	}
	return removed, report, nil
}
