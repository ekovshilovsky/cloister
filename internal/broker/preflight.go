package broker

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	brokerignore "cloister.io/internal/broker/ignore"
)

// maxXattrExamples is how many paths a finding names. The count beside them is
// exact, so the list is an illustration and does not have to be complete.
const maxXattrExamples = 3

// PreflightReport summarizes bounded, transient project inspection. The scan
// retains no descriptor or per-path bookkeeping after it returns: attributes
// are counted per name and only a bounded sample of paths is kept, so a project
// with five thousand affected paths costs what one with five costs. The
// per-path record is streamed to PreflightOptions.Detail as the walk runs.
type PreflightReport struct {
	Entries uint64

	// MaterialFiles is how many regular files or directories carry at least
	// one material attribute, which is smaller than the sum of the findings
	// when a path carries several. The field name is retained for API
	// compatibility.
	MaterialFiles uint64

	// Material is what the user is told about, sorted by attribute name.
	Material []XattrFinding

	// Immaterial is what was set aside, kept so the record can say what was
	// not reported rather than leaving the reader to wonder whether anything
	// was looked at.
	Immaterial []XattrFinding
}

// XattrFinding is one attribute and how much of the project carries it. Files
// counts regular files and directories exactly, however many Examples were
// kept; the field name is retained for API compatibility.
type XattrFinding struct {
	Attribute string
	Files     uint64
	Examples  []string
}

// PreflightOptions carries the inspection's limits and its record.
type PreflightOptions struct {
	// MaxEntries stops the scan once the synchronized entry count exceeds the
	// per-session Mutagen guardrail. Zero disables the check.
	MaxEntries uint64

	// Detail receives one line per path carrying material attributes. A nil
	// Detail discards them. A write failure stops the scan so a successful
	// report never points at an incomplete detail record.
	Detail io.Writer
}

type metadataInspector interface {
	Xattrs(string) ([]string, error)
}

type commandMetadataInspector struct{}

func (commandMetadataInspector) Xattrs(path string) ([]string, error) {
	return listXattrs(path)
}

// PreflightProject rejects filesystem features outside the synchronized-copy
// contract and warns about material xattrs that will remain host-only.
func PreflightProject(root string, policy brokerignore.Policy) (PreflightReport, error) {
	return PreflightProjectWith(root, policy, PreflightOptions{})
}

func preflightProject(root string, policy brokerignore.Policy, inspector metadataInspector) (PreflightReport, error) {
	return preflightProjectWith(root, policy, PreflightOptions{}, inspector)
}

// PreflightProjectWithLimit stops as soon as the synchronized entry count
// exceeds the per-session Mutagen guardrail. A zero limit disables the check.
func PreflightProjectWithLimit(root string, policy brokerignore.Policy, maxEntries uint64) (PreflightReport, error) {
	return PreflightProjectWith(root, policy, PreflightOptions{MaxEntries: maxEntries})
}

// PreflightProjectWith is PreflightProject with the caller's limits and a
// destination for the per-path record.
func PreflightProjectWith(root string, policy brokerignore.Policy, opts PreflightOptions) (PreflightReport, error) {
	return preflightProjectWith(root, policy, opts, commandMetadataInspector{})
}

func preflightProjectWith(root string, policy brokerignore.Policy, opts PreflightOptions, inspector metadataInspector) (PreflightReport, error) {
	original, err := filepath.Abs(root)
	if err != nil {
		return PreflightReport{}, fmt.Errorf("making project root absolute: %w", err)
	}
	originalInfo, err := os.Lstat(original)
	if err != nil {
		return PreflightReport{}, err
	}
	if originalInfo.Mode()&os.ModeSymlink != 0 {
		return PreflightReport{}, fmt.Errorf("project root %q is a symlink; broker roots must be real directories", root)
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return PreflightReport{}, fmt.Errorf("resolving project root: %w", err)
	}
	rootInfo, err := os.Lstat(canonical)
	if err != nil {
		return PreflightReport{}, err
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return PreflightReport{}, fmt.Errorf("project root %q must be a real directory", canonical)
	}
	rootDevice, err := device(rootInfo)
	if err != nil {
		return PreflightReport{}, err
	}

	report := PreflightReport{}
	material := newXattrTally()
	immaterial := newXattrTally()
	err = filepath.WalkDir(canonical, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative := "."
		if path != canonical {
			var err error
			relative, err = filepath.Rel(canonical, path)
			if err != nil {
				return err
			}
			if policy.Ignored(relative, entry.IsDir()) {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			report.Entries++
			if opts.MaxEntries > 0 && report.Entries > opts.MaxEntries {
				return fmt.Errorf("project exceeds maxEntryCount %d", opts.MaxEntries)
			}
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		entryDevice, err := device(info)
		if err != nil {
			return err
		}
		if entryDevice != rootDevice {
			return fmt.Errorf("nested filesystem at %q is unsupported by synchronized copies", relative)
		}

		mode := info.Mode()
		if mode&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if filepath.IsAbs(target) || escapesRoot(relative, target) {
				return fmt.Errorf("symlink %q targets %q outside the project; broker mode requires portable relative symlinks", relative, target)
			}
			return nil
		}
		if !mode.IsRegular() && !mode.IsDir() {
			return fmt.Errorf("special file %q (%s) is unsupported by synchronized copies", relative, mode.Type())
		}
		if mode.IsRegular() {
			links, err := linkCount(info)
			if err != nil {
				return err
			}
			if links > 1 {
				return fmt.Errorf("hardlinked file %q has %d links; broker mode does not preserve hardlink identity", relative, links)
			}
		}
		xattrs, err := inspector.Xattrs(path)
		if err != nil {
			return err
		}
		// Every attribute is counted; only the material ones are a finding.
		// The count is kept whole rather than capped, because the number of
		// affected paths is the part the reader cannot reconstruct.
		var found []string
		for _, attribute := range xattrs {
			if isMaterialXattr(attribute) {
				material.add(attribute, relative)
				found = append(found, attribute)
				continue
			}
			immaterial.add(attribute, relative)
		}
		if len(found) > 0 {
			report.MaterialFiles++
			if opts.Detail != nil {
				if _, err := fmt.Fprintf(opts.Detail, "%s has host-only extended attributes: %s\n", relative, strings.Join(found, ", ")); err != nil {
					return fmt.Errorf("writing metadata detail for %q: %w", relative, err)
				}
			}
		}
		return nil
	})
	if err != nil {
		return report, err
	}
	report.Material = material.findings()
	report.Immaterial = immaterial.findings()
	return report, nil
}

// xattrTally counts attributes across a project while keeping only a bounded
// sample of the paths carrying each one.
type xattrTally struct {
	byAttribute map[string]*XattrFinding
}

func newXattrTally() *xattrTally {
	return &xattrTally{byAttribute: make(map[string]*XattrFinding)}
}

func (t *xattrTally) add(attribute, relative string) {
	finding, ok := t.byAttribute[attribute]
	if !ok {
		finding = &XattrFinding{Attribute: attribute}
		t.byAttribute[attribute] = finding
	}
	finding.Files++
	if len(finding.Examples) < maxXattrExamples {
		finding.Examples = append(finding.Examples, relative)
	}
}

// findings returns the tally sorted by attribute name, so a report of the same
// project reads the same way twice.
func (t *xattrTally) findings() []XattrFinding {
	if len(t.byAttribute) == 0 {
		return nil
	}
	findings := make([]XattrFinding, 0, len(t.byAttribute))
	for _, finding := range t.byAttribute {
		findings = append(findings, *finding)
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].Attribute < findings[j].Attribute })
	return findings
}

// MaterialSummary is the one line a project contributes to the console, or ""
// when it has nothing material. It names each attribute with its exact file
// count; the affected paths are in the detail record.
func (r PreflightReport) MaterialSummary() string {
	if len(r.Material) == 0 {
		return ""
	}
	carry := "carry"
	if r.MaterialFiles == 1 {
		carry = "carries"
	}
	return fmt.Sprintf("%s %s extended attributes that stay on the host: %s",
		pluralPaths(r.MaterialFiles), carry, strings.Join(describeFindings(r.Material), ", "))
}

// ImmaterialSummary reports what was examined and deliberately not raised, so
// the record shows the classification happening rather than only its result.
func (r PreflightReport) ImmaterialSummary() string {
	if len(r.Immaterial) == 0 {
		return ""
	}
	return "host-relationship attributes, not reported: " + strings.Join(describeFindings(r.Immaterial), ", ")
}

func describeFindings(findings []XattrFinding) []string {
	described := make([]string, 0, len(findings))
	for _, finding := range findings {
		described = append(described, fmt.Sprintf("%s (%s)", finding.Attribute, pluralPaths(finding.Files)))
	}
	return described
}

func pluralPaths(count uint64) string {
	if count == 1 {
		return "1 path"
	}
	return fmt.Sprintf("%d paths", count)
}

func escapesRoot(relative, target string) bool {
	joined := filepath.Clean(filepath.Join(filepath.Dir(relative), target))
	return joined == ".." || strings.HasPrefix(joined, ".."+string(filepath.Separator))
}

func device(info os.FileInfo) (uint64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("filesystem metadata is unavailable for %q", info.Name())
	}
	return uint64(stat.Dev), nil
}

func linkCount(info os.FileInfo) (uint64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("hardlink metadata is unavailable for %q", info.Name())
	}
	return uint64(stat.Nlink), nil
}
