// Package profile provides validation helpers for cloister profile names and
// stack identifiers.
package profile

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// namePattern enforces the naming rule: must begin with a lowercase letter,
// followed by zero or more lowercase letters, digits, or hyphens.
var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// reservedNames is the set of command names that may not be used as profile
// identifiers, as they would shadow built-in cloister subcommands.
var reservedNames = map[string]struct{}{
	"all":         {},
	"status":      {},
	"version":     {},
	"help":        {},
	"create":      {},
	"stop":        {},
	"delete":      {},
	"update":      {},
	"backup":      {},
	"restore":     {},
	"rebuild":     {},
	"setup":       {},
	"config":      {},
	"self-update": {},
	"add-stack":   {},
	"agent":       {},
}

// validStacks is the complete set of provisioning stacks that cloister
// supports. Any stack name outside this set is rejected at profile creation
// time so errors are surfaced before a VM is started.
var validStacks = map[string]struct{}{
	"web":    {},
	"cloud":  {},
	"dotnet": {},
	"python": {},
	"go":     {},
	"rust":   {},
	"data":   {},
	"ollama": {},
}

// ValidateName returns an error when name is empty, contains characters outside
// the allowed set (^[a-z][a-z0-9-]*$), or matches a reserved command name.
func ValidateName(name string) error {
	if name == "" {
		return errors.New("profile name must not be empty")
	}
	if !namePattern.MatchString(name) {
		return fmt.Errorf("profile name %q is invalid: must match ^[a-z][a-z0-9-]*$", name)
	}
	if _, reserved := reservedNames[name]; reserved {
		return fmt.Errorf("profile name %q is reserved and cannot be used", name)
	}
	return nil
}

// ValidateStacks returns an error when any element of stacks is not in the
// supported stack set. The error names the first unrecognised stack found.
func ValidateStacks(stacks []string) error {
	for _, s := range stacks {
		if _, ok := validStacks[s]; !ok {
			valid := make([]string, 0, len(validStacks))
			for k := range validStacks {
				valid = append(valid, k)
			}
			sort.Strings(valid)
			return fmt.Errorf("unknown stack %q: valid stacks are %s", s, strings.Join(valid, ", "))
		}
	}
	return nil
}
