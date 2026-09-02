// Proprietary and confidential. All rights reserved.

package cmd

import (
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// adviceCommand matches a command this CLI tells the user to run. Only whole
// lines are considered, because "cloister" also appears in prose; a line whose
// first word is the binary name is an invocation the user can copy. The token
// after the binary name is the subcommand, and a placeholder such as <profile>
// is an argument rather than a command, so it does not match.
var adviceCommand = regexp.MustCompile(`^cloister ([a-z][a-z0-9-]*)(?: ([a-z][a-z0-9-]*))?`)

// registeredCommand reports whether cloister has the named command, and
// returns it so a nested subcommand can be checked against its parent.
func registeredCommand(parent *cobra.Command, name string) (*cobra.Command, bool) {
	for _, c := range parent.Commands() {
		// Use is "logs <profile>"; the command name is the first field.
		if strings.Fields(c.Use)[0] == name {
			return c, true
		}
	}
	return nil, false
}

// assertAdviceIsRunnable fails for any command the advice names that cloister
// does not actually have.
func assertAdviceIsRunnable(t *testing.T, source, advice string) {
	t.Helper()
	for _, line := range strings.Split(advice, "\n") {
		match := adviceCommand.FindStringSubmatch(strings.TrimSpace(line))
		if match == nil {
			continue
		}
		sub, ok := registeredCommand(rootCmd, match[1])
		if !ok {
			t.Errorf("%s recommends %q, which is not a cloister command\nadvice:\n%s",
				source, "cloister "+match[1], advice)
			continue
		}
		// Existing is not enough. The command this advice used to name is
		// still registered, as a hidden shim whose whole output is that it was
		// removed, so a check for registration alone would have accepted it.
		// Hidden commands are absent from help and are not something to send a
		// user to.
		if sub.Hidden {
			t.Errorf("%s recommends %q, which is hidden and does no work\nadvice:\n%s",
				source, "cloister "+match[1], advice)
			continue
		}
		if match[2] == "" {
			continue
		}
		// A second token is only a subcommand when the first one has any;
		// otherwise it is an argument such as a profile name.
		if len(sub.Commands()) == 0 {
			continue
		}
		if _, ok := registeredCommand(sub, match[2]); !ok {
			t.Errorf("%s recommends %q, which is not a cloister command\nadvice:\n%s",
				source, "cloister "+match[1]+" "+match[2], advice)
		}
	}
}

// TestHeadlessAdviceNamesOnlyRealCommands pins the invariant these messages
// exist to serve: a user following printed guidance must land on a command
// that runs.
//
// Headless entry used to point at "cloister agent <profile>", a command
// removed when Lume profiles made a separate container lifecycle unnecessary,
// and the removal shim in turn recommended "cloister start" and
// "cloister forward", neither of which has ever existed. Two dead ends in
// sequence, with nothing in the build to notice.
func TestHeadlessAdviceNamesOnlyRealCommands(t *testing.T) {
	assertAdviceIsRunnable(t, "headless profile advice", headlessProfileAdvice("work"))
}

// TestLumeAdviceNamesOnlyRealCommands covers the other message on the entry
// path, which pointed at the same removed command tree.
func TestLumeAdviceNamesOnlyRealCommands(t *testing.T) {
	assertAdviceIsRunnable(t, "lume profile advice", lumeProfileAdvice("work"))
}

// TestAgentRemovalAdviceNamesOnlyRealCommands covers the removal shim itself.
func TestAgentRemovalAdviceNamesOnlyRealCommands(t *testing.T) {
	assertAdviceIsRunnable(t, "agent removal advice", agentRemovalAdvice())
}

// TestAdviceCheckRejectsAnUnknownCommand guards the check itself. A matcher
// that silently found nothing would pass every test above while the messages
// went on naming commands that do not exist.
func TestAdviceCheckRejectsAnUnknownCommand(t *testing.T) {
	fake := &testing.T{}
	assertAdviceIsRunnable(fake, "sample", "Use these commands instead:\n  cloister start <profile>\n")
	if !fake.Failed() {
		t.Error("the advice check accepted 'cloister start', which is not a cloister command")
	}
}

// TestAdviceCheckRejectsARemovedCommand covers the subtler half of the
// original defect: "cloister agent" resolves, so a check that only asked
// whether a command exists would have passed the advice that sent users to it.
func TestAdviceCheckRejectsARemovedCommand(t *testing.T) {
	fake := &testing.T{}
	assertAdviceIsRunnable(fake, "sample", "  cloister agent work\n")
	if !fake.Failed() {
		t.Error("the advice check accepted 'cloister agent', a hidden removal shim")
	}
}

// TestAdviceCheckAcceptsARealCommand guards the opposite direction, so the
// check cannot pass by rejecting everything.
func TestAdviceCheckAcceptsARealCommand(t *testing.T) {
	fake := &testing.T{}
	assertAdviceIsRunnable(fake, "sample", "  cloister logs <profile>\n  cloister status\n")
	if fake.Failed() {
		t.Error("the advice check rejected commands that cloister does have")
	}
}
