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
