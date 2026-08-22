// Proprietary and confidential. All rights reserved.

package lifecycle

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"cloister.io/internal/vm"
)

// SysctlReader supplies numeric macOS kernel values to the FD guard.
type SysctlReader interface {
	ReadUint64(name string) (uint64, error)
}

// CommandSysctlReader reads a sysctl value without invoking a shell.
type CommandSysctlReader struct{}

func (CommandSysctlReader) ReadUint64(name string) (uint64, error) {
	out, err := exec.Command("sysctl", "-n", name).Output()
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", name, err)
	}
	value, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing %s: %w", name, err)
	}
	return value, nil
}

// FDPolicy controls warning and refusal thresholds for global host descriptors.
type FDPolicy struct {
	WarnRatio      float64
	RefuseRatio    float64
	WarnHeadroom   uint64
	RefuseHeadroom uint64
}

func DefaultFDPolicy() FDPolicy {
	return FDPolicy{WarnRatio: 0.70, RefuseRatio: 0.85, WarnHeadroom: 100_000, RefuseHeadroom: 50_000}
}

// FDResult records the sampled state and policy decision.
type FDResult struct {
	Used, Limit, Headroom uint64
	Ratio                 float64
	Warning               string
	Refuse                bool
}

func (r FDResult) Detail() string {
	return fmt.Sprintf("kern.num_files=%d, kern.maxfiles=%d, headroom=%d (%.1f%% used)", r.Used, r.Limit, r.Headroom, r.Ratio*100)
}

// CheckFDHeadroom evaluates the system-wide macOS descriptor table.
func CheckFDHeadroom(reader SysctlReader, policy FDPolicy) (FDResult, error) {
	if reader == nil {
		return FDResult{}, fmt.Errorf("sysctl reader is required")
	}
	used, err := reader.ReadUint64("kern.num_files")
	if err != nil {
		return FDResult{}, err
	}
	limit, err := reader.ReadUint64("kern.maxfiles")
	if err != nil {
		return FDResult{}, err
	}
	if limit == 0 || used > limit {
		return FDResult{}, fmt.Errorf("invalid descriptor sample: used=%d limit=%d", used, limit)
	}
	result := FDResult{Used: used, Limit: limit, Headroom: limit - used, Ratio: float64(used) / float64(limit)}
	// The absolute-headroom floors are capped at half the table so a host whose
	// kern.maxfiles is small (old macOS defaults such as 12288/24576/49152 still
	// linger on upgraded machines) is never refused while idle. On those hosts
	// the fixed 50k/100k floors exceed the whole table and would otherwise refuse
	// every start at 0% utilization with no reachable safe state. Medium and
	// large hosts keep the fixed reserve unchanged, since limit/2 is far above it.
	refuseHeadroom := capHeadroom(policy.RefuseHeadroom, limit)
	warnHeadroom := capHeadroom(policy.WarnHeadroom, limit)
	result.Refuse = result.Ratio >= policy.RefuseRatio || result.Headroom < refuseHeadroom
	if result.Refuse || result.Ratio >= policy.WarnRatio || result.Headroom < warnHeadroom {
		result.Warning = "Warning: low host file descriptor headroom: " + result.Detail()
	}
	return result, nil
}

// capHeadroom bounds an absolute descriptor floor at half the table so refusal
// always requires more than 50% utilization. This keeps the fixed reserve on
// hosts with a large kern.maxfiles while guaranteeing a small-table host is
// never refused at low usage.
func capHeadroom(headroom, limit uint64) uint64 {
	if half := limit / 2; headroom > half {
		return half
	}
	return headroom
}

// WorkspacePolicy bounds directory traversal and broad-root detection.
type WorkspacePolicy struct {
	WarnEntries       uint64
	RefuseEntries     uint64
	ProjectChildLimit int
}

func DefaultWorkspacePolicy() WorkspacePolicy {
	return WorkspacePolicy{WarnEntries: 25_000, RefuseEntries: 100_000, ProjectChildLimit: 2}
}

// WorkspaceAssessment describes why a start directory is considered broad.
type WorkspaceAssessment struct {
	Entries         uint64
	ProjectChildren int
	Warning         string
}

// CheckWorkspace performs a capped traversal. Broad roots are refused only
// when virtiofs would expose them, while broker and disabled modes may retain a
// broad start_dir as a routing hint without mounting it.
func CheckWorkspace(root string, provider vm.WorkspaceProvider, policy WorkspacePolicy) (WorkspaceAssessment, error) {
	if root == "" {
		if provider == vm.VirtiofsWorkspace {
			return WorkspaceAssessment{}, fmt.Errorf("virtiofs workspace path is empty")
		}
		return WorkspaceAssessment{}, nil
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return WorkspaceAssessment{}, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return WorkspaceAssessment{}, fmt.Errorf("resolving start_dir %q: %w", root, err)
	}
	if provider == vm.WorkspaceBroker {
		info, statErr := os.Stat(root)
		if statErr != nil {
			return WorkspaceAssessment{}, fmt.Errorf("reading workspace routing root %q: %w", root, statErr)
		}
		if !info.IsDir() {
			return WorkspaceAssessment{}, fmt.Errorf("workspace routing root %q is not a directory", root)
		}
		return WorkspaceAssessment{}, nil
	}

	assessment := WorkspaceAssessment{}
	home, _ := os.UserHomeDir()
	broadRoot := root == filepath.Clean(string(filepath.Separator)) || (home != "" && root == filepath.Clean(home))
	rootIsProject := pathExists(filepath.Join(root, ".git"))
	entries, err := os.ReadDir(root)
	if err != nil {
		return assessment, fmt.Errorf("reading start_dir %q: %w", root, err)
	}
	if !rootIsProject {
		for _, entry := range entries {
			if entry.IsDir() && pathExists(filepath.Join(root, entry.Name(), ".git")) {
				assessment.ProjectChildren++
			}
		}
	}

	stopWalk := fmt.Errorf("workspace entry cap reached")
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path != root {
			assessment.Entries++
		}
		if assessment.Entries > policy.RefuseEntries {
			return stopWalk
		}
		return nil
	})
	if err != nil && err != stopWalk {
		return assessment, fmt.Errorf("counting start_dir %q: %w", root, err)
	}

	broadProjects := assessment.ProjectChildren >= policy.ProjectChildLimit
	broadEntries := assessment.Entries > policy.RefuseEntries
	var reasons []string
	if broadRoot {
		reasons = append(reasons, "root or home directory")
	}
	if broadProjects {
		reasons = append(reasons, fmt.Sprintf("%d child Git projects", assessment.ProjectChildren))
	}
	if broadEntries {
		reasons = append(reasons, fmt.Sprintf("more than %d entries", policy.RefuseEntries))
	}
	if provider == vm.VirtiofsWorkspace && (broadRoot || broadProjects || broadEntries) {
		return assessment, fmt.Errorf("refusing broad virtiofs start_dir %q (%s); select a project root", root, strings.Join(reasons, ", "))
	}
	if provider == vm.BrokerWorkspace && (broadRoot || (!rootIsProject && assessment.ProjectChildren > 0)) {
		return assessment, fmt.Errorf("refusing broad broker start_dir %q; this Tier 2 slice requires one explicit project root", root)
	}
	if broadRoot || broadProjects || broadEntries || assessment.Entries >= policy.WarnEntries {
		assessment.Warning = fmt.Sprintf("Warning: broad start_dir %q has %d child Git projects and at least %d entries", root, assessment.ProjectChildren, assessment.Entries)
	}
	return assessment, nil
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
