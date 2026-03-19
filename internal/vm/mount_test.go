package vm

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ekovshilovsky/cloister/internal/config"
)

// autoPolicy returns a ResourcePolicy equivalent to "auto" (allow everything).
func autoPolicy() config.ResourcePolicy {
	return config.ResourcePolicy{IsSet: true, Mode: "auto"}
}

// nonePolicy returns a ResourcePolicy equivalent to "none" (deny everything).
func nonePolicy() config.ResourcePolicy {
	return config.ResourcePolicy{IsSet: true, Mode: "none"}
}

// listPolicy returns an explicit-list ResourcePolicy containing the given names.
func listPolicy(names ...string) config.ResourcePolicy {
	return config.ResourcePolicy{IsSet: true, Names: names}
}

// unsetPolicy returns a ResourcePolicy that has not been explicitly configured,
// causing BuildMounts to apply environment-aware defaults via ResolveForMounts.
func unsetPolicy() config.ResourcePolicy {
	return config.ResourcePolicy{}
}

// findMount returns the Mount with the given subpath under homeDir, or a
// zero-valued Mount and false if no such mount exists in the slice.
func findMount(mounts []Mount, homeDir, subpath string) (Mount, bool) {
	target := filepath.Join(homeDir, subpath)
	for _, m := range mounts {
		if m.Location == target {
			return m, true
		}
	}
	return Mount{}, false
}

// TestBuildMounts_InteractiveAutoPolicy verifies that an interactive profile
// with an explicit "auto" mount policy receives all standard mounts.
func TestBuildMounts_InteractiveAutoPolicy(t *testing.T) {
	home := t.TempDir()
	workspace := filepath.Join(home, "code")

	mounts := BuildMounts(home, workspace, nil, autoPolicy(), false)

	expectedSubpaths := []string{
		"code", ".ssh", ".gnupg", "Downloads",
		".claude/plugins", ".claude/skills", ".claude/agents",
	}

	for _, sub := range expectedSubpaths {
		if _, ok := findMount(mounts, home, sub); !ok {
			t.Errorf("expected mount for %q to be present, but it was not", sub)
		}
	}
}

// TestBuildMounts_HeadlessNonePolicy verifies that a headless profile with an
// explicit "none" mount policy receives only the mandatory workspace mount.
func TestBuildMounts_HeadlessNonePolicy(t *testing.T) {
	home := t.TempDir()
	workspace := filepath.Join(home, "code")

	mounts := BuildMounts(home, workspace, nil, nonePolicy(), true)

	if len(mounts) != 1 {
		t.Fatalf("expected exactly 1 mount (workspace), got %d: %v", len(mounts), mounts)
	}

	if _, ok := findMount(mounts, home, "code"); !ok {
		t.Error("expected mandatory workspace mount to be present under none policy")
	}
}

// TestBuildMounts_HeadlessUnsetPolicy verifies that a headless profile with no
// mount policy configured receives the workspace mount plus the curated default
// set (Claude extension directories), mirroring defaultHeadlessMounts in policy.go.
func TestBuildMounts_HeadlessUnsetPolicy(t *testing.T) {
	home := t.TempDir()
	workspace := filepath.Join(home, "code")

	mounts := BuildMounts(home, workspace, nil, unsetPolicy(), true)

	expected := []string{"code", ".claude/plugins", ".claude/skills", ".claude/agents"}
	for _, sub := range expected {
		if _, ok := findMount(mounts, home, sub); !ok {
			t.Errorf("expected default headless mount for %q, but it was absent", sub)
		}
	}

	// SSH, GPG, and Downloads must be excluded from the headless default set.
	excluded := []string{".ssh", ".gnupg", "Downloads"}
	for _, sub := range excluded {
		if _, ok := findMount(mounts, home, sub); ok {
			t.Errorf("mount %q should be absent under headless unset policy, but was present", sub)
		}
	}
}

// TestBuildMounts_ExplicitPolicy verifies that an explicit allowlist policy
// admits exactly the named supplemental mounts and excludes all others; the
// workspace mount is always present regardless of the policy.
func TestBuildMounts_ExplicitPolicy(t *testing.T) {
	home := t.TempDir()
	workspace := filepath.Join(home, "code")

	mounts := BuildMounts(home, workspace, nil, listPolicy("ssh"), false)

	// Workspace mount must always be present as the first entry.
	if _, ok := findMount(mounts, home, "code"); !ok {
		t.Error("expected workspace mount to be present regardless of policy")
	}
	if _, ok := findMount(mounts, home, ".ssh"); !ok {
		t.Error("expected ssh mount to be present under explicit [ssh] policy")
	}

	// All other standard mounts must be excluded.
	excluded := []string{".gnupg", "Downloads", ".claude/plugins", ".claude/skills", ".claude/agents"}
	for _, sub := range excluded {
		if _, ok := findMount(mounts, home, sub); ok {
			t.Errorf("mount %q should be absent under explicit [ssh] policy, but was present", sub)
		}
	}
}

// TestBuildMounts_WorkspaceAlwaysIncluded verifies that the workspace mount is
// always the first entry in the mount list regardless of the supplemental
// mount policy configuration.
func TestBuildMounts_WorkspaceAlwaysIncluded(t *testing.T) {
	home := t.TempDir()
	workspace := filepath.Join(home, "code")

	// Use a policy that names no supplemental mounts.
	mounts := BuildMounts(home, workspace, nil, nonePolicy(), false)

	if len(mounts) == 0 {
		t.Fatal("expected at least one mount (workspace), got none")
	}
	if mounts[0].Location != workspace {
		t.Errorf("first mount should be workspace %q, got %q", workspace, mounts[0].Location)
	}
	if !mounts[0].Writable {
		t.Error("workspace mount must be writable")
	}
}

// TestBuildMounts_OllamaStackWithDirectory verifies that the ollama-models mount
// is appended when the ollama stack is present and the models directory exists.
func TestBuildMounts_OllamaStackWithDirectory(t *testing.T) {
	home := t.TempDir()
	workspace := filepath.Join(home, "code")

	// Create the ~/.ollama/models directory so the filesystem check succeeds.
	ollamaModels := filepath.Join(home, ".ollama", "models")
	if err := os.MkdirAll(ollamaModels, 0o755); err != nil {
		t.Fatalf("creating ollama models dir: %v", err)
	}

	mounts := BuildMounts(home, workspace, []string{"ollama"}, autoPolicy(), false)

	if _, ok := findMount(mounts, home, ".ollama/models"); !ok {
		t.Error("expected ollama-models mount to be present when stack is active and directory exists")
	}
}

// TestBuildMounts_OllamaStackDirectoryAbsent verifies that the ollama-models
// mount is NOT added when the ollama stack is present but the directory does
// not exist on disk.
func TestBuildMounts_OllamaStackDirectoryAbsent(t *testing.T) {
	home := t.TempDir()
	workspace := filepath.Join(home, "code")
	// Do not create ~/.ollama/models — it must remain absent.

	mounts := BuildMounts(home, workspace, []string{"ollama"}, autoPolicy(), false)

	if _, ok := findMount(mounts, home, ".ollama/models"); ok {
		t.Error("ollama-models mount must not be added when the directory does not exist")
	}
}

// TestBuildMounts_NoOllamaStack verifies that the ollama-models mount is NOT
// added when the models directory exists but the ollama stack is not requested.
func TestBuildMounts_NoOllamaStack(t *testing.T) {
	home := t.TempDir()
	workspace := filepath.Join(home, "code")

	// Create the directory so the only discriminator is the stack list.
	ollamaModels := filepath.Join(home, ".ollama", "models")
	if err := os.MkdirAll(ollamaModels, 0o755); err != nil {
		t.Fatalf("creating ollama models dir: %v", err)
	}

	mounts := BuildMounts(home, workspace, []string{"node", "python"}, autoPolicy(), false)

	if _, ok := findMount(mounts, home, ".ollama/models"); ok {
		t.Error("ollama-models mount must not be added when ollama is not in the stacks list")
	}
}

// TestBuildMounts_HeadlessClaudeExtensionsReadOnly verifies that the Claude
// extension mounts (plugins, skills, agents) are set to read-only for headless
// profiles, preventing unattended modification of host extension directories.
func TestBuildMounts_HeadlessClaudeExtensionsReadOnly(t *testing.T) {
	home := t.TempDir()
	workspace := filepath.Join(home, "code")

	// Unset policy for headless resolves to the default set which includes all
	// three Claude extension directories.
	mounts := BuildMounts(home, workspace, nil, unsetPolicy(), true)

	claudePaths := []string{".claude/plugins", ".claude/skills", ".claude/agents"}
	for _, sub := range claudePaths {
		m, ok := findMount(mounts, home, sub)
		if !ok {
			t.Errorf("expected %q mount to be present for headless profile", sub)
			continue
		}
		if m.Writable {
			t.Errorf("mount %q must be read-only for headless profiles, but Writable=true", sub)
		}
	}
}

// TestBuildMounts_InteractiveClaudeExtensionsReadWrite verifies that the Claude
// extension mounts are writable for interactive profiles, allowing users to
// install or update plugins, skills, and agents from within the VM.
func TestBuildMounts_InteractiveClaudeExtensionsReadWrite(t *testing.T) {
	home := t.TempDir()
	workspace := filepath.Join(home, "code")

	mounts := BuildMounts(home, workspace, nil, autoPolicy(), false)

	claudePaths := []string{".claude/plugins", ".claude/skills", ".claude/agents"}
	for _, sub := range claudePaths {
		m, ok := findMount(mounts, home, sub)
		if !ok {
			t.Errorf("expected %q mount to be present for interactive profile", sub)
			continue
		}
		if !m.Writable {
			t.Errorf("mount %q must be writable for interactive profiles, but Writable=false", sub)
		}
	}
}

// TestMountsChanged verifies that MountsChanged correctly identifies when the
// mount set has grown or shrunk, and returns false for identical-length slices.
func TestMountsChanged(t *testing.T) {
	a := []Mount{{Location: "/a"}}
	b := []Mount{{Location: "/a"}, {Location: "/b"}}

	if !MountsChanged(a, b) {
		t.Error("MountsChanged should return true when lengths differ")
	}
	if MountsChanged(a, a) {
		t.Error("MountsChanged should return false for identical slices")
	}
	if MountsChanged([]Mount{}, []Mount{}) {
		t.Error("MountsChanged should return false for two empty slices")
	}
}

// TestBuildMounts_DeduplicatedWorkspaceMount verifies that the workspace
// directory appears exactly once in the mount list. Because "code" is no longer
// a named policy entry, there is no mechanism for the policy filter to produce
// a duplicate workspace entry.
func TestBuildMounts_DeduplicatedWorkspaceMount(t *testing.T) {
	home := t.TempDir()
	workspace := filepath.Join(home, "code")

	mounts := BuildMounts(home, workspace, nil, listPolicy("ssh"), false)

	count := 0
	for _, m := range mounts {
		if m.Location == workspace {
			count++
		}
	}

	if count != 1 {
		t.Errorf("expected exactly one workspace mount, got %d", count)
	}
}

// TestBuildMountsCustomWorkspace verifies that a non-default workspace path is
// used as the first mount entry when a custom start_dir is configured on the
// profile.
func TestBuildMountsCustomWorkspace(t *testing.T) {
	home := t.TempDir()
	workspace := filepath.Join(home, "Projects", "my-app")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("creating workspace dir: %v", err)
	}

	mounts := BuildMounts(home, workspace, nil, autoPolicy(), false)

	if len(mounts) == 0 {
		t.Fatal("expected at least one mount, got none")
	}
	if mounts[0].Location != workspace {
		t.Errorf("first mount should be custom workspace %q, got %q", workspace, mounts[0].Location)
	}
	if !mounts[0].Writable {
		t.Error("workspace mount must be writable")
	}
}
