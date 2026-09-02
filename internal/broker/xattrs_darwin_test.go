//go:build darwin

package broker

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	brokerignore "cloister.io/internal/broker/ignore"
)

func TestListXattrsIncludesNativeMacOSACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "protected.txt")
	if err := os.WriteFile(path, []byte("content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("xattr", "-c", path).CombinedOutput(); err != nil {
		t.Fatalf("clearing enumerated extended attributes: %v: %s", err, output)
	}
	if output, err := exec.Command("chmod", "+a", "everyone deny write", path).CombinedOutput(); err != nil {
		t.Fatalf("setting native macOS ACL: %v: %s", err, output)
	}

	attributes, err := listXattrs(path)
	if err != nil {
		t.Fatalf("listXattrs() error = %v", err)
	}
	if !slices.Contains(attributes, "com.apple.system.Security") {
		t.Fatalf("listXattrs() = %v, want native macOS ACL marker", attributes)
	}

	policy, err := brokerignore.Compile(filepath.Dir(path), nil)
	if err != nil {
		t.Fatal(err)
	}
	report, err := PreflightProject(filepath.Dir(path), policy)
	if err != nil {
		t.Fatalf("PreflightProject() error = %v", err)
	}
	if !slices.ContainsFunc(report.Material, func(finding XattrFinding) bool {
		return finding.Attribute == "com.apple.system.Security" && finding.Files == 1
	}) {
		t.Fatalf("PreflightProject() material findings = %v, want native macOS ACL", report.Material)
	}
}

func TestListXattrsIncludesNativeMacOSACLOnSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	link := filepath.Join(dir, "link.txt")
	if err := os.WriteFile(target, []byte("content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.txt", link); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("xattr", "-c", "-s", link).CombinedOutput(); err != nil {
		t.Fatalf("clearing enumerated symlink attributes: %v: %s", err, output)
	}
	if output, err := exec.Command("xattr", "-w", "-s", "com.example.symlink", "link metadata", link).CombinedOutput(); err != nil {
		t.Fatalf("setting enumerated symlink attribute: %v: %s", err, output)
	}
	if output, err := exec.Command("chmod", "-h", "+a", "everyone deny write", link).CombinedOutput(); err != nil {
		t.Fatalf("setting native macOS ACL on symlink: %v: %s", err, output)
	}

	attributes, err := listXattrs(link)
	if err != nil {
		t.Fatalf("listXattrs() error = %v", err)
	}
	if !slices.Contains(attributes, "com.apple.system.Security") {
		t.Fatalf("listXattrs() = %v, want native macOS ACL marker for the symlink itself", attributes)
	}
	if !slices.Contains(attributes, "com.example.symlink") {
		t.Fatalf("listXattrs() = %v, want enumerated attribute from the symlink itself", attributes)
	}

	policy, err := brokerignore.Compile(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	report, err := PreflightProject(dir, policy)
	if err != nil {
		t.Fatalf("PreflightProject() error = %v", err)
	}
	if !slices.ContainsFunc(report.Material, func(finding XattrFinding) bool {
		return finding.Attribute == "com.apple.system.Security" &&
			finding.Files == 1 && slices.Contains(finding.Examples, "link.txt")
	}) {
		t.Fatalf("PreflightProject() material findings = %v, want the symlink's native macOS ACL", report.Material)
	}
}
