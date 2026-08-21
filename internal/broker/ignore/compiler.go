// Proprietary and confidential. All rights reserved.

// Package ignore compiles repository Git ignore files into a deterministic
// project-root policy suitable for Mutagen's Git-style ignore engine.
package ignore

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// Pattern is one ordered ignore rule with enough source information to explain
// a refusal without exposing any file content.
type Pattern struct {
	Text      string
	Source    string
	Line      int
	Mandatory bool
}

// Policy is the complete ordered policy passed to Mutagen. Mandatory rules are
// always the final entries and therefore cannot be negated by project config.
type Policy struct {
	Patterns []Pattern
}

// Strings returns the ordered pattern text expected by Mutagen.
func (p Policy) Strings() []string {
	result := make([]string, len(p.Patterns))
	for i := range p.Patterns {
		result[i] = p.Patterns[i].Text
	}
	return result
}

// RepositoryStrings returns only compiled repository and profile rules. It is
// primarily useful for semantic conformance tests against git check-ignore.
func (p Policy) RepositoryStrings() []string {
	var result []string
	for _, pattern := range p.Patterns {
		if !pattern.Mandatory {
			result = append(result, pattern.Text)
		}
	}
	return result
}

// Compile reads every .gitignore below root, rebases nested rules to the
// synchronization root, appends profile rules, then seals the policy with
// mandatory exclusions.
func Compile(root string, extra []string) (Policy, error) {
	return CompileWithMandatory(root, extra, MandatoryPatterns())
}

// CompileWithMandatory compiles a policy and seals it with the supplied final
// exclusions. A non-nil empty slice intentionally means no mandatory rules.
func CompileWithMandatory(root string, extra, mandatory []string) (Policy, error) {
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return Policy{}, fmt.Errorf("resolving project root %q: %w", root, err)
	}
	info, err := os.Lstat(canonical)
	if err != nil {
		return Policy{}, fmt.Errorf("reading project root %q: %w", canonical, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return Policy{}, fmt.Errorf("project root %q must be a real directory", canonical)
	}
	rootDevice, err := statDevice(info)
	if err != nil {
		return Policy{}, err
	}

	policy := Policy{}
	if err := compileTree(canonical, "", rootDevice, mandatory, &policy); err != nil {
		return Policy{}, err
	}

	for i, text := range extra {
		compiled, skip, err := compileLine(text, "", "profile workspace.ignore", i+1)
		if err != nil {
			return Policy{}, err
		}
		if !skip {
			policy.Patterns = append(policy.Patterns, compiled)
		}
	}
	for _, text := range mandatory {
		policy.Patterns = append(policy.Patterns, Pattern{
			Text:      text,
			Source:    "cloister mandatory policy",
			Mandatory: true,
		})
	}
	return policy, nil
}

func compileTree(root, relativeDir string, rootDevice uint64, mandatory []string, policy *Policy) error {
	directory := filepath.Join(root, filepath.FromSlash(relativeDir))
	ignorePath := filepath.Join(directory, ".gitignore")
	if info, err := os.Lstat(ignorePath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("%q must be a regular file so ignore policy cannot change through a link", ignorePath)
		}
		patterns, err := compileFile(ignorePath, relativeDir)
		if err != nil {
			return err
		}
		policy.Patterns = append(policy.Patterns, patterns...)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("reading %q: %w", ignorePath, err)
	}

	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("reading project directory %q: %w", directory, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || mandatoryDirectory(mandatory, entry.Name()) {
			continue
		}
		child := entry.Name()
		if relativeDir != "" {
			child = relativeDir + "/" + child
		}
		if policy.Ignored(child, true) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		childDevice, err := statDevice(info)
		if err != nil {
			return err
		}
		if childDevice != rootDevice {
			return fmt.Errorf("nested filesystem at %q is unsupported by synchronized copies", child)
		}
		if err := compileTree(root, child, rootDevice, mandatory, policy); err != nil {
			return err
		}
	}
	return nil
}

func mandatoryDirectory(patterns []string, name string) bool {
	for _, pattern := range patterns {
		trimmed := strings.TrimSuffix(strings.TrimPrefix(pattern, "/"), "/")
		if trimmed == name {
			return true
		}
	}
	return false
}

func statDevice(info os.FileInfo) (uint64, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("filesystem metadata is unavailable for %q", info.Name())
	}
	return uint64(stat.Dev), nil
}

func compileFile(path, base string) ([]Pattern, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening %q: %w", path, err)
	}
	defer file.Close()

	var result []Pattern
	scanner := bufio.NewScanner(file)
	buffer := make([]byte, 64*1024)
	scanner.Buffer(buffer, 1024*1024)
	for line := 1; scanner.Scan(); line++ {
		pattern, skip, err := compileLine(scanner.Text(), base, path, line)
		if err != nil {
			return nil, err
		}
		if !skip {
			result = append(result, pattern)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading %q: %w", path, err)
	}
	return result, nil
}

func compileLine(raw, base, source string, line int) (Pattern, bool, error) {
	raw = strings.TrimSuffix(raw, "\r")
	raw = trimUnescapedTrailingSpaces(raw)
	if raw == "" || (raw[0] == '#' && !strings.HasPrefix(raw, `\#`)) {
		return Pattern{}, true, nil
	}
	if strings.IndexByte(raw, 0) >= 0 {
		return Pattern{}, false, unsupported(source, line, "NUL bytes")
	}
	if raw == "!" {
		return Pattern{}, false, unsupported(source, line, "a lone negation marker")
	}
	if strings.Contains(raw, "***") {
		return Pattern{}, false, unsupported(source, line, "three or more consecutive asterisks")
	}
	if strings.ContainsAny(raw, "{}") {
		return Pattern{}, false, unsupported(source, line, "brace expressions, which Mutagen treats as expansions")
	}
	if strings.Contains(raw, "//") {
		return Pattern{}, false, unsupported(source, line, "empty path components")
	}
	if hasDanglingEscape(raw) {
		return Pattern{}, false, unsupported(source, line, "a dangling escape")
	}
	if !balancedCharacterClasses(raw) {
		return Pattern{}, false, unsupported(source, line, "an unbalanced character class")
	}

	negated := raw[0] == '!'
	body := raw
	if negated {
		body = body[1:]
	}
	directoryOnly := strings.HasSuffix(body, "/")
	matchBody := strings.TrimSuffix(body, "/")
	anchored := strings.HasPrefix(matchBody, "/")
	matchBody = strings.TrimPrefix(matchBody, "/")
	if matchBody == "" {
		return Pattern{}, false, unsupported(source, line, "an empty path")
	}

	hasSlash := strings.Contains(matchBody, "/")
	compiled := matchBody
	if base == "" {
		if anchored || hasSlash {
			compiled = "/" + matchBody
		}
	} else if anchored || hasSlash {
		compiled = "/" + base + "/" + matchBody
	} else {
		compiled = "/" + base + "/**/" + matchBody
	}
	if directoryOnly {
		compiled += "/"
	}
	if negated {
		compiled = "!" + compiled
	}
	return Pattern{Text: compiled, Source: source, Line: line}, false, nil
}

func trimUnescapedTrailingSpaces(value string) string {
	for len(value) > 0 && value[len(value)-1] == ' ' {
		backslashes := 0
		for i := len(value) - 2; i >= 0 && value[i] == '\\'; i-- {
			backslashes++
		}
		if backslashes%2 == 1 {
			break
		}
		value = value[:len(value)-1]
	}
	return value
}

func hasDanglingEscape(value string) bool {
	count := 0
	for i := len(value) - 1; i >= 0 && value[i] == '\\'; i-- {
		count++
	}
	return count%2 == 1
}

func balancedCharacterClasses(value string) bool {
	escaped := false
	inClass := false
	for _, r := range value {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		switch r {
		case '[':
			if inClass {
				return false
			}
			inClass = true
		case ']':
			if !inClass {
				return false
			}
			inClass = false
		}
	}
	return !inClass
}

func unsupported(source string, line int, detail string) error {
	return fmt.Errorf("%s:%d: ignore rule cannot be represented safely: %s", source, line, detail)
}

// Ignored provides a conservative local interpretation for safety preflight.
// Mutagen remains the synchronization matcher; this helper exists only to
// avoid rejecting metadata found below excluded directories.
func (p Policy) Ignored(relative string, isDir bool) bool {
	relative = strings.TrimPrefix(filepath.ToSlash(filepath.Clean(relative)), "./")
	ignored := false
	for _, pattern := range p.Patterns {
		negated := strings.HasPrefix(pattern.Text, "!")
		text := strings.TrimPrefix(pattern.Text, "!")
		if matches(text, relative, isDir) {
			ignored = !negated
		}
	}
	return ignored
}

func matches(pattern, relative string, isDir bool) bool {
	directoryOnly := strings.HasSuffix(pattern, "/")
	pattern = strings.TrimSuffix(strings.TrimPrefix(pattern, "/"), "/")
	if pattern == "" {
		return false
	}
	if !strings.Contains(pattern, "/") {
		pattern = "**/" + pattern
	}
	candidates := []string{relative}
	parts := strings.Split(relative, "/")
	for i := 1; i < len(parts); i++ {
		candidates = append(candidates, strings.Join(parts[:i], "/"))
	}
	rx, err := globRegexp(pattern)
	if err != nil {
		return false
	}
	for i, candidate := range candidates {
		candidateIsDir := i > 0 || isDir
		if directoryOnly && !candidateIsDir {
			continue
		}
		if rx.MatchString(candidate) {
			return true
		}
	}
	return false
}

func globRegexp(pattern string) (*regexp.Regexp, error) {
	var expression strings.Builder
	expression.WriteString("^")
	for i := 0; i < len(pattern); {
		switch pattern[i] {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i += 2
				if i < len(pattern) && pattern[i] == '/' {
					expression.WriteString("(?:.*/)?")
					i++
				} else {
					expression.WriteString(".*")
				}
			} else {
				expression.WriteString("[^/]*")
				i++
			}
		case '?':
			expression.WriteString("[^/]")
			i++
		case '[':
			end := i + 1
			for end < len(pattern) && pattern[end] != ']' {
				end++
			}
			if end == len(pattern) {
				return nil, fmt.Errorf("unbalanced character class")
			}
			class := pattern[i : end+1]
			if strings.HasPrefix(class, "[!") {
				class = "[^" + class[2:]
			}
			expression.WriteString(class)
			i = end + 1
		case '\\':
			if i+1 >= len(pattern) {
				return nil, fmt.Errorf("dangling escape")
			}
			expression.WriteString(regexp.QuoteMeta(pattern[i+1 : i+2]))
			i += 2
		default:
			expression.WriteString(regexp.QuoteMeta(pattern[i : i+1]))
			i++
		}
	}
	expression.WriteString("$")
	return regexp.Compile(expression.String())
}
