// Proprietary and confidential. All rights reserved.

package colima

import (
	"reflect"
	"testing"
)

// sshShellArgs must pass the script to bash -lc as a single, verbatim argv
// element. exec.Command does not involve a host shell, and colima forwards the
// element to the guest intact, so the guest's bash -lc is what parses `&&`,
// redirects, and expansions. Pre-quoting the script here would make bash -lc
// treat the whole quoted string as one command name (exit 127).
func TestSSHShellArgsPassesCompoundScriptVerbatim(t *testing.T) {
	script := `cd "$HOME/workspaces/urlprocessorapi-39aeb6a46515" && git status`
	want := []string{
		"ssh", "--profile", "cloister-work", "--", "bash", "-lc", script,
	}
	if got := sshShellArgs("cloister-work", script); !reflect.DeepEqual(got, want) {
		t.Fatalf("sshShellArgs() = %#v, want %#v", got, want)
	}
}

func TestSSHShellArgsDoesNotAlterEmbeddedQuotes(t *testing.T) {
	script := `printf '%s\n' "it's intact"`
	if got := sshShellArgs("cloister-work", script); got[len(got)-1] != script {
		t.Fatalf("script arg = %q, want verbatim %q", got[len(got)-1], script)
	}
}
