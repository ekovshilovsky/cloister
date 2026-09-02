package broker

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	brokerignore "cloister.io/internal/broker/ignore"
)

// mapXattrInspector answers from a filename-keyed table, so a test can describe
// a project's extended attributes without needing a filesystem that carries
// them. What is under test is the classification, not listxattr.
type mapXattrInspector map[string][]string

func (m mapXattrInspector) Xattrs(path string) ([]string, error) {
	return m[filepath.Base(path)], nil
}

// writeProject creates the named files under a new root and compiles the
// default ignore policy for it.
func writeProject(t *testing.T, names ...string) (string, brokerignore.Policy) {
	t.Helper()
	root := t.TempDir()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(root, name), []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	policy, err := brokerignore.Compile(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	return root, policy
}

// TestPreflightReportsMaterialAndNotHostRelationshipXattrs is the test this
// change exists to pass. A project of 500 files carrying the attribute macOS
// stamps on everything it downloads or copies, and one file carrying an
// attribute tooling set deliberately: the one is reported and the 500 are not.
//
// Counting or aggregating the 501 would produce a shorter console and the same
// non-answer. Only deciding which of them matters passes this.
func TestPreflightReportsMaterialAndNotHostRelationshipXattrs(t *testing.T) {
	names := []string{"deliberate.bin"}
	inspector := mapXattrInspector{"deliberate.bin": {"user.test"}}
	for i := 0; i < 500; i++ {
		name := fmt.Sprintf("downloaded-%d.go", i)
		names = append(names, name)
		inspector[name] = []string{"com.apple.provenance"}
	}
	root, policy := writeProject(t, names...)

	report, err := preflightProject(root, policy, inspector)
	if err != nil {
		t.Fatalf("preflightProject() error = %v", err)
	}

	if len(report.Material) != 1 {
		t.Fatalf("Material = %v, want only the deliberate attribute", report.Material)
	}
	if got := report.Material[0].Attribute; got != "user.test" {
		t.Errorf("material attribute = %q, want %q", got, "user.test")
	}
	if got := report.Material[0].Files; got != 1 {
		t.Errorf("material files = %d, want 1", got)
	}

	summary := report.MaterialSummary()
	if !strings.Contains(summary, "user.test") {
		t.Errorf("summary does not report the material attribute: %q", summary)
	}
	if strings.Contains(summary, "provenance") {
		t.Errorf("summary reports a host-relationship attribute: %q", summary)
	}

	// The 500 are unreported, not unexamined: the count survives so the record
	// can say what was set aside and why.
	if len(report.Immaterial) != 1 || report.Immaterial[0].Attribute != "com.apple.provenance" {
		t.Fatalf("Immaterial = %v, want the provenance count", report.Immaterial)
	}
	if got := report.Immaterial[0].Files; got != 500 {
		t.Errorf("immaterial files = %d, want 500", got)
	}
}

// TestPreflightReportsUnrecognizedXattrs pins the rule that makes the
// classification safe to ship: the immaterial set is finite and listed, so an
// attribute nobody has classified is reported rather than assumed harmless.
func TestPreflightReportsUnrecognizedXattrs(t *testing.T) {
	root, policy := writeProject(t, "source.go")
	inspector := mapXattrInspector{"source.go": {"com.example.novel.attr"}}

	report, err := preflightProject(root, policy, inspector)
	if err != nil {
		t.Fatalf("preflightProject() error = %v", err)
	}
	if len(report.Material) != 1 || report.Material[0].Attribute != "com.example.novel.attr" {
		t.Fatalf("Material = %v, want the unrecognized attribute reported", report.Material)
	}
}

// TestPreflightCountsEveryMaterialFile covers the disclosure the old cap did
// not make. The detail list is bounded, so the count is the only thing telling
// the reader how much of the project is affected, and it has to be exact.
func TestPreflightCountsEveryMaterialFile(t *testing.T) {
	var names []string
	inspector := mapXattrInspector{}
	for i := 0; i < 50; i++ {
		name := fmt.Sprintf("asset-%d.icns", i)
		names = append(names, name)
		inspector[name] = []string{"com.apple.ResourceFork"}
	}
	root, policy := writeProject(t, names...)

	report, err := preflightProject(root, policy, inspector)
	if err != nil {
		t.Fatalf("preflightProject() error = %v", err)
	}
	if len(report.Material) != 1 {
		t.Fatalf("Material = %v, want one finding", report.Material)
	}
	finding := report.Material[0]
	if finding.Files != 50 {
		t.Errorf("Files = %d, want 50", finding.Files)
	}
	if len(finding.Examples) > maxXattrExamples {
		t.Errorf("Examples = %d, want at most %d", len(finding.Examples), maxXattrExamples)
	}
	if report.MaterialFiles != 50 {
		t.Errorf("MaterialFiles = %d, want 50", report.MaterialFiles)
	}
	if summary := report.MaterialSummary(); !strings.Contains(summary, "50") {
		t.Errorf("summary hides the true count: %q", summary)
	}
}

// TestPreflightWritesPerFileDetail covers where the per-file lines went: off
// the console and into the record, which is what makes dropping them from the
// console a relocation rather than a loss.
func TestPreflightWritesPerFileDetail(t *testing.T) {
	root, policy := writeProject(t, "a.icns", "b.go")
	inspector := mapXattrInspector{
		"a.icns": {"com.apple.ResourceFork", "com.apple.provenance"},
		"b.go":   {"com.apple.provenance"},
	}

	var detail bytes.Buffer
	report, err := preflightProjectWith(root, policy, PreflightOptions{Detail: &detail}, inspector)
	if err != nil {
		t.Fatalf("preflightProjectWith() error = %v", err)
	}
	if report.MaterialFiles != 1 {
		t.Fatalf("MaterialFiles = %d, want 1", report.MaterialFiles)
	}

	logged := detail.String()
	if !strings.Contains(logged, "a.icns") || !strings.Contains(logged, "com.apple.ResourceFork") {
		t.Errorf("detail is missing the material finding: %q", logged)
	}
	// The file whose only attributes are host-relationship ones is not a
	// finding, so it is not a line.
	if strings.Contains(logged, "b.go") {
		t.Errorf("detail reports a file with no material attributes: %q", logged)
	}
}

// TestXattrClassification is the audit of the classification data itself. Every
// entry in the immaterial list appears here with the reason it is there, so a
// change to that list has to be argued for in this table.
func TestXattrClassification(t *testing.T) {
	for _, testCase := range []struct {
		attribute string
		material  bool
	}{
		// Content and access semantics: material.
		{"com.apple.ResourceFork", true},
		{"system.posix_acl_access", true},
		{"system.posix_acl_default", true},
		{"com.apple.system.Security", true},
		{"user.test", true},
		{"user.anything.at.all", true},
		// Unclassified: material, because nobody has decided otherwise.
		{"com.example.novel.attr", true},
		{"com.apple.SomethingAddedInAFutureRelease", true},
		// The file's relationship to this Mac: immaterial.
		{"com.apple.provenance", false},
		{"com.apple.quarantine", false},
		{"com.apple.macl", false},
		{"com.apple.FinderInfo", false},
		{"com.apple.metadata:kMDItemWhereFroms", false},
		{"com.apple.lastuseddate#PS", false},
	} {
		if got := isMaterialXattr(testCase.attribute); got != testCase.material {
			t.Errorf("isMaterialXattr(%q) = %v, want %v", testCase.attribute, got, testCase.material)
		}
	}
}

// TestMaterialRulesOutrankImmaterialPrefixes pins the ordering the safety of
// the classification rests on: no later broadening of an immaterial prefix can
// quietly swallow an attribute that carries content or access semantics.
func TestMaterialRulesOutrankImmaterialPrefixes(t *testing.T) {
	// The broadest immaterial rule anyone could plausibly add later.
	overBroad := append(append([]xattrRule(nil), immaterialXattrRules...),
		xattrRule{prefix: "com.apple.", why: "test-only over-broad rule"})

	if !classifyXattr("com.apple.ResourceFork", materialXattrRules, overBroad) {
		t.Error("an over-broad immaterial prefix swallowed the resource fork")
	}
	if classifyXattr("com.apple.provenance", materialXattrRules, overBroad) {
		t.Error("the over-broad rule should still cover what no material rule claims")
	}
}

// TestPreflightStillRefusesUnsupportedFiles keeps the rejections that are not
// warnings. Classifying attributes must not soften what preflight refuses.
func TestPreflightStillRefusesUnsupportedFiles(t *testing.T) {
	t.Run("xattr inspection failure", func(t *testing.T) {
		root, policy := writeProject(t, "source.go")
		_, err := preflightProject(root, policy, failingXattrInspector{})
		if err == nil || !strings.Contains(err.Error(), "listing extended attributes") {
			t.Fatalf("preflightProject() error = %v, want the inspection failure", err)
		}
	})

	t.Run("hardlink", func(t *testing.T) {
		root, policy := writeProject(t, "first")
		if err := os.Link(filepath.Join(root, "first"), filepath.Join(root, "second")); err != nil {
			t.Fatal(err)
		}
		_, err := preflightProject(root, policy, mapXattrInspector{})
		if err == nil || !strings.Contains(err.Error(), "hardlinked file") {
			t.Fatalf("preflightProject() error = %v, want the hardlink refusal", err)
		}
	})

	t.Run("escaping symlink", func(t *testing.T) {
		root, policy := writeProject(t)
		if err := os.Symlink("../outside", filepath.Join(root, "escape")); err != nil {
			t.Fatal(err)
		}
		_, err := preflightProject(root, policy, mapXattrInspector{})
		if err == nil || !strings.Contains(err.Error(), "portable relative symlinks") {
			t.Fatalf("preflightProject() error = %v, want the symlink refusal", err)
		}
	})
}

type failingXattrInspector struct{}

func (failingXattrInspector) Xattrs(path string) ([]string, error) {
	return nil, fmt.Errorf("listing extended attributes for %q: permission denied", path)
}
