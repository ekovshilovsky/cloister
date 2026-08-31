package layout

import (
	"fmt"
	"path"
	"strings"
	"unicode"

	"cloister.io/internal/config"
)

// Attributes are the project facts the guest-path renderer may consult.
type Attributes struct {
	// Selector is the workspace-relative portable path of the project.
	Selector string
	// Org is the GitHub organization captured for the project, if any.
	Org string
}

// GuestRelPath renders a posix, workspace-relative guest path for one project.
// The result has no leading slash, does not contain "..", and is nonempty.
// scheme "mirror" uses Selector; org grouping prefixes "<org>/" when groupByOrg
// is true and Org is nonempty. scheme "flat" is not rendered here.
func GuestRelPath(attrs Attributes, cfg config.Layout, groupByOrg bool) (string, error) {
	scheme := cfg.Scheme
	if scheme == "" {
		scheme = config.LayoutSchemeMirror
	}
	if scheme != config.LayoutSchemeMirror {
		return "", fmt.Errorf("workspace layout scheme %q cannot be rendered as a selector path; use %q", scheme, config.LayoutSchemeMirror)
	}
	selector, err := cleanGuestSegmentPath(attrs.Selector)
	if err != nil {
		return "", err
	}
	if groupByOrg && attrs.Org != "" {
		org, orgErr := cleanGuestSegmentPath(attrs.Org)
		if orgErr != nil {
			return "", fmt.Errorf("workspace layout org %q is not a safe guest path segment: %w", attrs.Org, orgErr)
		}
		if strings.Contains(org, "/") {
			return "", fmt.Errorf("workspace layout org %q must be a single path segment", attrs.Org)
		}
		selector = org + "/" + selector
	}
	if err := validateGuestRel(selector); err != nil {
		return "", err
	}
	return selector, nil
}

// EffectiveGroupByOrg reports whether org prefixes should be applied given the
// configured grouping policy and whether the collection spans multiple orgs.
func EffectiveGroupByOrg(cfg config.Layout, multiOrg bool) bool {
	switch cfg.GroupByOrg {
	case config.LayoutGroupTrue:
		return true
	case config.LayoutGroupFalse:
		return false
	default:
		return multiOrg
	}
}

// MultiOrg reports whether orgs contains more than one distinct non-empty value.
func MultiOrg(orgs []string) bool {
	seen := make(map[string]struct{})
	for _, org := range orgs {
		if org == "" {
			continue
		}
		seen[org] = struct{}{}
		if len(seen) > 1 {
			return true
		}
	}
	return false
}

func cleanGuestSegmentPath(value string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("workspace guest path is empty")
	}
	if strings.ContainsRune(value, '\\') || strings.ContainsRune(value, '\x00') {
		return "", fmt.Errorf("workspace guest path %q contains illegal characters", value)
	}
	if path.IsAbs(value) || strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("workspace guest path %q must be relative", value)
	}
	clean := path.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("workspace guest path %q is not a portable relative path", value)
	}
	if clean != strings.TrimPrefix(value, "./") && clean != value {
		return "", fmt.Errorf("workspace guest path %q is not a clean portable path", value)
	}
	return clean, nil
}

func validateGuestRel(relative string) error {
	if relative == "" {
		return fmt.Errorf("workspace guest path is empty")
	}
	if strings.HasPrefix(relative, "/") || strings.Contains(relative, "..") {
		return fmt.Errorf("workspace guest path %q is not a portable relative path", relative)
	}
	for _, r := range relative {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || strings.ContainsRune("/-_.", r) {
			continue
		}
		return fmt.Errorf("workspace guest path %q contains unsupported characters", relative)
	}
	return nil
}
