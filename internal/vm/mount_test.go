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

	mounts := BuildMounts(home, nil, autoPolicy(), false)

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
// explicit "none" mount policy receives only the mandatory code mount.
func TestBuildMounts_HeadlessNonePolicy(t *testing.T) {
	home := t.TempDir()

	mounts := BuildMounts(home, nil, nonePolicy(), true)

	if len(mounts) != 1 {
		t.Fatalf("expected exactly 1 mount (code), got %d: %v", len(mounts), mounts)
	}

	if _, ok := findMount(mounts, home, "code"); !ok {
		t.Error("expected mandatory code mount to be present under none policy")
	}
}

// TestBuildMounts_HeadlessUnsetPolicy verifies that a headless profile with no
// mount policy configured receives the curated default set (code + Claude
// extension directories), which mirrors defaultHeadlessMounts in policy.go.
func TestBuildMounts_HeadlessUnsetPolicy(t *testing.T) {
	home := t.TempDir()

	mounts := BuildMounts(home, nil, unsetPolicy(), true)

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
// admits exactly the named mounts and excludes all others.
func TestBuildMounts_ExplicitPolicy(t *testing.T) {
	home := t.TempDir()

	mounts := BuildMounts(home, nil, listPolicy("code", "ssh"), false)

	// Both explicitly named mounts must be present.
	if _, ok := findMount(mounts, home, "code"); !ok {
		t.Error("expected code mount to be present under explicit [code, ssh] policy")
	}
	if _, ok := findMount(mounts, home, ".ssh"); !ok {
		t.Error("expected ssh mount to be present under explicit [code, ssh] policy")
	}

	// All other standard mounts must be excluded.
	excluded := []string{".gnupg", "Downloads", ".claude/plugins", ".claude/skills", ".claude/agents"}
	for _, sub := range excluded {
		if _, ok := findMount(mounts, home, sub); ok {
			t.Errorf("mount %q should be absent under explicit [code, ssh] policy, but was present", sub)
		}
	}
}

// TestBuildMounts_CodeAlwaysIncluded verifies that the code mount is included
// even when an explicit policy omits "code" from the allowlist.
func TestBuildMounts_CodeAlwaysIncluded(t *testing.T) {
	home := t.TempDir()

	// Explicit list that intentionally omits "code".
	mounts := BuildMounts(home, nil, listPolicy("ssh"), false)

	if _, ok := findMount(mounts, home, "code"); !ok {
		t.Error("code mount must always be present regardless of mount policy")
	}
}

// TestBuildMounts_OllamaStackWithDirectory verifies that the ollama-models mount
// is appended when the ollama stack is present and the models directory exists.
func TestBuildMounts_OllamaStackWithDirectory(t *testing.T) {
	home := t.TempDir()

	// Create the ~/.ollama/models directory so the filesystem check succeeds.
	ollamaModels := filepath.Join(home, ".ollama", "models")
	if err := os.MkdirAll(ollamaModels, 0o755); err != nil {
		t.Fatalf("creating ollama models dir: %v", err)
	}

	mounts := BuildMounts(home, []string{"ollama"}, autoPolicy(), false)

	if _, ok := findMount(mounts, home, ".ollama/models"); !ok {
		t.Error("expected ollama-models mount to be present when stack is active and directory exists")
	}
}

// TestBuildMounts_OllamaStackDirectoryAbsent verifies that the ollama-models
// mount is NOT added when the ollama stack is present but the directory does
// not exist on disk.
func TestBuildMounts_OllamaStackDirectoryAbsent(t *testing.T) {
	home := t.TempDir()
	// Do not create ~/.ollama/models — it must remain absent.

	mounts := BuildMounts(home, []string{"ollama"}, autoPolicy(), false)

	if _, ok := findMount(mounts, home, ".ollama/models"); ok {
		t.Error("ollama-models mount must not be added when the directory does not exist")
	}
}

// TestBuildMounts_NoOllamaStack verifies that the ollama-models mount is NOT
// added when the models directory exists but the ollama stack is not requested.
func TestBuildMounts_NoOllamaStack(t *testing.T) {
	home := t.TempDir()

	// Create the directory so the only discriminator is the stack list.
	ollamaModels := filepath.Join(home, ".ollama", "models")
	if err := os.MkdirAll(ollamaModels, 0o755); err != nil {
		t.Fatalf("creating ollama models dir: %v", err)
	}

	mounts := BuildMounts(home, []string{"node", "python"}, autoPolicy(), false)

	if _, ok := findMount(mounts, home, ".ollama/models"); ok {
		t.Error("ollama-models mount must not be added when ollama is not in the stacks list")
	}
}

// TestBuildMounts_HeadlessClaudeExtensionsReadOnly verifies that the Claude
// extension mounts (plugins, skills, agents) are set to read-only for headless
// profiles, preventing unattended modification of host extension directories.
func TestBuildMounts_HeadlessClaudeExtensionsReadOnly(t *testing.T) {
	home := t.TempDir()

	// Unset policy for headless resolves to the default set which includes all
	// three Claude extension directories.
	mounts := BuildMounts(home, nil, unsetPolicy(), true)

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

	mounts := BuildMounts(home, nil, autoPolicy(), false)

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

// TestBuildMounts_DeduplicatedCodeMount verifies that including "code" explicitly
// in the policy list does not produce a duplicate code mount entry.
func TestBuildMounts_DeduplicatedCodeMount(t *testing.T) {
	home := t.TempDir()

	mounts := BuildMounts(home, nil, listPolicy("code", "ssh"), false)

	count := 0
	for _, m := range mounts {
		if m.Location == filepath.Join(home, "code") {
			count++
		}
	}

	if count != 1 {
		t.Errorf("expected exactly one code mount, got %d", count)
	}
}
